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
	html, err := se.inbox(ctx)
	if err != nil {
		return nil, err
	}

	msgs := parseInbox(html)
	msgs = filterList(msgs, q)

	if q.Limit > 0 && len(msgs) > q.Limit {
		msgs = msgs[:q.Limit]
	}
	return msgs, nil
}

func (s *Source) Get(ctx context.Context, _, uid string) (*mail.MessageFull, error) {
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
	inbox, err := se.inbox(ctx)
	if err != nil {
		return nil, err
	}
	wasUnread := isUnread(inbox, itemID)
	folderID := extractFolderID(inbox)

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

	if wasUnread {
		full.Seen = false
		if err := se.mark(ctx, folderID, itemID, "markunread"); err != nil {
			// Восстановить флаг не удалось — это побочный эффект, а не отказ
			// операции: письмо пользователь получил. Только предупреждаем.
			slog.Warn("не удалось вернуть письму флаг непрочитанного", "err", err)
		}
	}

	return full, nil
}

func (s *Source) MarkSeen(ctx context.Context, _, uid string) error {
	itemID, err := decodeUID(uid)
	if err != nil {
		return err
	}

	se, err := s.newSession(ctx)
	if err != nil {
		return err
	}
	defer se.close(ctx)
	inbox, err := se.inbox(ctx)
	if err != nil {
		return err
	}
	// Идемпотентность: письмо уже прочитано — делать нечего.
	if !isUnread(inbox, itemID) {
		return nil
	}

	return se.mark(ctx, extractFolderID(inbox), itemID, "markread")
}

// mark отправляет команду markread/markunread формой OWA. Без canary сервер
// отклонит POST как CSRF.
func (se *session) mark(ctx context.Context, folderID, itemID, cmd string) error {
	if se.canary == "" {
		return mail.ErrUpstreamUnavailable
	}
	form := url.Values{
		"hidcmdpst":    {cmd},
		"chkmsg":       {itemID},
		"X-OWA-CANARY": {se.canary},
		"hidpid":       {"MessageView"},
		"hidactbrfld":  {""},
		"hidso":        {""},
	}
	u := se.src.base + "/owa/?ae=Folder&t=IPF.Note"
	if folderID != "" {
		u += "&id=" + url.QueryEscape(folderID)
	}
	_, err := se.do(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
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
