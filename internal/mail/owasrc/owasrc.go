// Package owasrc реализует mail.Source поверх облегчённой версии Outlook Web
// App (OWA Light) — HTML-интерфейса Exchange.
//
// Это запасной транспорт: у почты МЭИ IMAP (993) обрывает TLS, а EWS и
// ActiveSync отвечают 403 (аутентификация проходит, но протокол выключен
// администраторами). Рабочим остаётся только веб-OWA, поэтому источник
// ходит в него Basic-аутентификацией и разбирает HTML.
//
// Как и imapsrc, клиент держит сессию на запрос: сложную долгоживущую
// сессию OWA (canary-токены, cadata-cookie) переживать между вызовами не
// стоит ради задачи, дёргающей ручку раз в несколько минут.
package owasrc

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/gyattalert/mpei-mail-api/internal/mail"
)

type Source struct {
	base string
	user string
	pass string
}

var _ mail.Source = (*Source)(nil)

// New создаёт источник. base — корень сервера, например https://mail.mpei.ru.
func New(base, user, pass string) *Source {
	return &Source{base: strings.TrimRight(base, "/"), user: user, pass: pass}
}

// session — одна залогиненная HTTP-сессия OWA: cookie jar плюс canary-токен
// (CSRF-защита OWA, обязателен для POST-команд).
type session struct {
	src    *Source
	client *http.Client
	canary string
	lc     listContext
}

func (s *Source) newSession(ctx context.Context) (*session, error) {
	jar, _ := cookiejar.New(nil)
	return &session{
		src:    s,
		client: &http.Client{Jar: jar, Timeout: 30 * time.Second},
	}, nil
}

// close освобождает серверную сессию OWA. Без этого каждый запрос оставляет
// висящую сессию, и после нескольких десятков логинов Exchange упирается в
// лимит сессий на ящик и начинает отвечать 500. logoff зовём на best-effort:
// его неудача не влияет на уже полученный ответ.
func (se *session) close(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, se.src.base+"/owa/logoff.owa", nil)
	if err != nil {
		return
	}
	req.SetBasicAuth(se.src.user, se.src.pass)
	if resp, err := se.client.Do(req); err == nil {
		resp.Body.Close()
	}
}

func (se *session) do(ctx context.Context, method, u string, body io.Reader) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return "", fmt.Errorf("%w: сборка запроса", mail.ErrUpstreamUnavailable)
	}
	req.SetBasicAuth(se.src.user, se.src.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := se.client.Do(req)
	if err != nil {
		slog.Error("запрос к OWA не удался", "err", err)
		return "", wrapHTTPErr(ctx, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", wrapHTTPErr(ctx, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return string(data), nil
	case http.StatusUnauthorized, http.StatusForbidden:
		// Логин/пароль неверны либо доступ к OWA закрыт. Наружу — единый
		// «недоступен», подробности только в лог.
		slog.Error("OWA отклонил аутентификацию", "status", resp.StatusCode)
		return "", mail.ErrUpstreamUnavailable
	default:
		slog.Error("OWA вернул неожиданный статус", "status", resp.StatusCode)
		return "", mail.ErrUpstreamUnavailable
	}
}

// userContext возвращает значение cookie UserContext — именно его OWA кладёт
// в поле hidcanary перед отправкой формы (sbtFrm в cmn.js), затирая значение
// из разметки. Форма с «страничным» токеном отвергается молча: сервер
// отвечает 200 и не делает ничего.
func (se *session) userContext() string {
	u, err := url.Parse(se.src.base)
	if err != nil {
		return ""
	}
	for _, c := range se.client.Jar.Cookies(u) {
		if c.Name == "UserContext" {
			return c.Value
		}
	}
	return ""
}

func wrapHTTPErr(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return mail.ErrUpstreamTimeout
	}
	return mail.ErrUpstreamUnavailable
}

// inbox загружает страницу списка и попутно запоминает canary-токен.
//
// Отдельно проверяем, что пришла именно рабочая страница OWA: при перегрузке
// Exchange отдаёт 200 с усечённой или служебной страницей без списка. Такой
// ответ нельзя молча трактовать как «ящик пуст» — валидная страница OWA всегда
// несёт canary-токен, поэтому его отсутствие считаем недоступностью апстрима.
func (se *session) inbox(ctx context.Context) (string, error) {
	html, err := se.do(ctx, http.MethodGet, se.src.base+"/owa/?modurl=0", nil)
	if err != nil {
		return "", err
	}
	c := extractCanary(html)
	if c == "" {
		slog.Error("OWA вернул страницу без canary — вероятно перегрузка", "size", len(html))
		return "", mail.ErrUpstreamUnavailable
	}
	se.canary = c
	return html, nil
}

// openFolder открывает папку ящика и возвращает её страницу со списком писем
// вместе с самой папкой.
//
// Корневая страница OWA — это и есть «Входящие», поэтому для них
// дополнительный запрос не нужен; за остальными папками надо сходить.
func (se *session) openFolder(ctx context.Context, mailbox string) (string, folder, error) {
	root, err := se.inbox(ctx)
	if err != nil {
		return "", folder{}, err
	}

	se.lc = parseListContext(root)

	f, ok := resolveFolder(root, mailbox)
	if !ok {
		// Для «Входящих» отсутствие ссылки в навигации — не повод падать:
		// нужная страница уже загружена, а id папки идёт лишь контекстом
		// POST-команд и допускает пустое значение.
		if isInboxName(mailbox) {
			return root, folder{}, nil
		}
		slog.Warn("запрошена папка, которой нет в ящике", "mailbox", mailbox)
		return "", folder{}, mail.ErrMailboxNotFound
	}
	if isInboxName(mailbox) {
		return root, f, nil
	}

	page, err := se.do(ctx, http.MethodGet,
		se.src.base+"/owa/?ae=Folder&t=IPF.Note&id="+url.QueryEscape(f.ID), nil)
	if err != nil {
		return "", folder{}, err
	}
	se.lc = parseListContext(page)
	return page, f, nil
}

func (s *Source) Ping(ctx context.Context) error {
	se, err := s.newSession(ctx)
	if err != nil {
		return err
	}
	defer se.close(ctx)
	html, err := se.inbox(ctx)
	if err != nil {
		return err
	}
	// 200 может вернуть и страница логина; убеждаемся, что это именно OWA.
	if !strings.Contains(html, "OwaPage") && !strings.Contains(html, "modurl") {
		return mail.ErrUpstreamUnavailable
	}
	return nil
}

func (s *Source) List(ctx context.Context, q mail.ListQuery) ([]mail.Message, error) {
	se, err := s.newSession(ctx)
	if err != nil {
		return nil, err
	}
	defer se.close(ctx)
	page, _, err := se.openFolder(ctx, q.Mailbox)
	if err != nil {
		return nil, err
	}

	msgs := parseInbox(page)
	msgs = filterList(msgs, q)
	sortNewestFirst(msgs)

	if q.Limit > 0 && len(msgs) > q.Limit {
		msgs = msgs[:q.Limit]
	}
	return msgs, nil
}

func (s *Source) Get(ctx context.Context, mailbox, uid string) (*mail.MessageFull, error) {
	itemID, err := decodeUID(uid)
	if err != nil {
		return nil, err
	}

	se, err := s.newSession(ctx)
	if err != nil {
		return nil, err
	}
	defer se.close(ctx)

	// Узнаём исходный флаг «прочитано» до открытия: OWA помечает письмо
	// прочитанным в момент чтения, а контракт требует, чтобы GET не менял
	// состояние ящика. Если письмо было непрочитанным — вернём флаг после.
	list, _, err := se.openFolder(ctx, mailbox)
	if err != nil {
		return nil, err
	}
	wasUnread := isUnread(list, itemID)

	readURL := se.src.base + "/owa/?ae=Item&t=IPM.Note&a=Read&id=" + url.QueryEscape(itemID)
	page, err := se.do(ctx, http.MethodGet, readURL, nil)
	if err != nil {
		return nil, err
	}
	if isNotFoundPage(page) {
		return nil, mail.ErrNotFound
	}

	full := parseMessage(page)
	full.UID = uid
	// Страница чтения флаг не показывает — он известен только из списка,
	// снятого до открытия. Без этого ручка всегда отдавала seen=false.
	full.Seen = !wasUnread

	if wasUnread {
		if err := se.mark(ctx, itemID, "markunread"); err != nil {
			// Восстановить флаг не удалось — это побочный эффект, а не отказ
			// операции: письмо пользователь получил. Только предупреждаем.
			slog.Warn("не удалось вернуть письму флаг непрочитанного", "err", err)
		}
	}

	return full, nil
}

func (s *Source) MarkSeen(ctx context.Context, mailbox, uid string) error {
	itemID, err := decodeUID(uid)
	if err != nil {
		return err
	}

	se, err := s.newSession(ctx)
	if err != nil {
		return err
	}
	defer se.close(ctx)
	list, _, err := se.openFolder(ctx, mailbox)
	if err != nil {
		return err
	}
	// Идемпотентность: письмо уже прочитано — делать нечего.
	if !isUnread(list, itemID) {
		return nil
	}

	return se.mark(ctx, itemID, "markread")
}

// mark отправляет команду markread/markunread формой списка — так же, как
// это делает сам OWA (submtCmd + sbtFrm в msglst.js/cmn.js):
//
//   - адрес берётся из переменных страницы (gtFrmActn), а не собирается
//     вручную из ссылок навигации;
//   - hidcanary заполняется значением cookie UserContext;
//   - скрытое поле X-OWA-CANARY уходит вместе с остальной формой.
//
// Отступление от этого набора сервер не отвергает: он отвечает 200 и просто
// не выполняет команду, из-за чего ошибка выглядит как успех.
func (se *session) mark(ctx context.Context, itemID, cmd string) error {
	action := se.lc.formAction(se.src.base)
	canary := se.userContext()
	if action == "" || canary == "" {
		slog.Error("нет контекста формы OWA для команды", "cmd", cmd, "hasAction", action != "", "hasCanary", canary != "")
		return mail.ErrUpstreamUnavailable
	}

	form := url.Values{
		"hidcmdpst":   {cmd},
		"chkmsg":      {itemID},
		"hidcanary":   {canary},
		"hidpid":      {"MessageView"},
		"hidactbrfld": {""},
		"hidso":       {""},
		"hidcid":      {""},
		"hidpnst":     {""},
	}
	if se.canary != "" {
		form.Set("X-OWA-CANARY", se.canary)
	}

	_, err := se.do(ctx, http.MethodPost, action, strings.NewReader(form.Encode()))
	return err
}

// decodeUID разворачивает наш path-safe uid обратно в OWA ItemId. uid отдаётся
// клиенту как base64url(ItemId): «сырой» ItemId содержит + и /, которые ломают
// маршрут /messages/{uid} и требуют экранирования в каждом запросе.
func decodeUID(uid string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(uid)
	if err != nil || len(raw) == 0 {
		return "", mail.ErrNotFound
	}
	return string(raw), nil
}

// encodeUID — обратная операция, вызывается парсером списка.
func encodeUID(itemID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(itemID))
}
