// Package imapsrc реализует mail.Source поверх IMAP.
//
// Стратегия подключений — коннект на запрос, без пула. Для потребителя,
// который дёргает ручку раз в несколько минут, экономия 300–600 мс на TLS
// и LOGIN не окупает класса багов с протухшими сессиями и гонкой
// Select(ReadOnly) против Select(RW). Если сервер начнёт троттлить логины
// под нагрузкой, лечение — ленивый единственный коннект под мьютексом
// с идловым TTL; интерфейс mail.Source от этого не меняется.
package imapsrc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/gyattalert/mpei-mail-api/internal/mail"
)

// snippetPrefix — сколько байт текстовой части тянуть ради сниппета.
// 2048, а не 512: кириллица в UTF-8 занимает два байта на символ, а тело
// вдобавок приходит в quoted-printable или base64, так что до декодирования
// нужен запас.
const snippetPrefix = 2048

// snippetRunes — длина сниппета в символах после декодирования.
const snippetRunes = 300

type Source struct {
	addr string
	user string
	pass string
}

var _ mail.Source = (*Source)(nil)

func New(addr, user, pass string) *Source {
	return &Source{addr: addr, user: user, pass: pass}
}

// connect устанавливает TLS-сессию и логинится.
//
// Ошибки библиотеки наружу не отдаются: в них может оказаться строка
// подключения с логином. Наружу идёт сентинел, подробности — в лог.
func (s *Source) connect(ctx context.Context) (*imapclient.Client, error) {
	host, _, err := net.SplitHostPort(s.addr)
	if err != nil {
		return nil, fmt.Errorf("%w: некорректный адрес сервера", mail.ErrUpstreamUnavailable)
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if deadline, ok := ctx.Deadline(); ok {
		dialer.Deadline = deadline
	}

	c, err := imapclient.DialTLS(s.addr, &imapclient.Options{
		TLSConfig: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
		Dialer:    dialer,
	})
	if err != nil {
		slog.Error("не удалось подключиться к почтовому серверу", "addr", s.addr, "err", err)
		return nil, wrapUpstream(ctx, err)
	}

	if err := c.Login(s.user, s.pass).Wait(); err != nil {
		slog.Error("не удалось залогиниться на почтовом сервере", "addr", s.addr, "err", err)
		_ = c.Close()
		return nil, wrapUpstream(ctx, err)
	}

	return c, nil
}

// wrapUpstream разводит таймаут и общую недоступность: потребителю ручки
// они предписывают разное поведение — 504 стоит повторить, 502 нет.
func wrapUpstream(ctx context.Context, err error) error {
	var netErr net.Error
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, net.ErrClosed) && ctx.Err() != nil:
		return mail.ErrUpstreamTimeout
	case errors.As(err, &netErr) && netErr.Timeout():
		return mail.ErrUpstreamTimeout
	default:
		return mail.ErrUpstreamUnavailable
	}
}

func logout(c *imapclient.Client) {
	if err := c.Logout().Wait(); err != nil {
		slog.Debug("logout завершился с ошибкой", "err", err)
	}
	_ = c.Close()
}

func (s *Source) Ping(ctx context.Context) error {
	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	logout(c)
	return nil
}

func (s *Source) List(ctx context.Context, q mail.ListQuery) ([]mail.Message, error) {
	c, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer logout(c)

	if _, err := c.Select(q.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		slog.Error("не удалось выбрать папку", "mailbox", q.Mailbox, "err", err)
		return nil, wrapUpstream(ctx, err)
	}

	criteria := &imap.SearchCriteria{}
	if q.Unseen {
		criteria.NotFlag = []imap.Flag{imap.FlagSeen}
	}
	if !q.Since.IsZero() {
		criteria.Since = q.Since
	}

	data, err := c.UIDSearch(criteria, &imap.SearchOptions{ReturnAll: true}).Wait()
	if err != nil {
		slog.Error("поиск писем не удался", "mailbox", q.Mailbox, "err", err)
		return nil, wrapUpstream(ctx, err)
	}

	uids := data.AllUIDs()
	if len(uids) == 0 {
		return []mail.Message{}, nil
	}
	// UID монотонно растут, поэтому последние N в хвосте — это самые свежие письма.
	if len(uids) > q.Limit {
		uids = uids[len(uids)-q.Limit:]
	}

	section := &imap.FetchItemBodySection{
		Peek:    true,
		Partial: &imap.SectionPartial{Size: snippetPrefix},
	}
	buffers, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
		UID:           true,
		Envelope:      true,
		Flags:         true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		slog.Error("не удалось загрузить письма", "mailbox", q.Mailbox, "err", err)
		return nil, wrapUpstream(ctx, err)
	}

	msgs := make([]mail.Message, 0, len(buffers))
	for _, buf := range buffers {
		msgs = append(msgs, buildMessage(buf, section))
	}
	// Свежие письма сверху — потребителю ручки так удобнее.
	reverse(msgs)

	return msgs, nil
}

func (s *Source) Get(ctx context.Context, mailbox, uid string) (*mail.MessageFull, error) {
	num, err := parseUID(uid)
	if err != nil {
		return nil, err
	}

	c, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer logout(c)

	if _, err := c.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		slog.Error("не удалось выбрать папку", "mailbox", mailbox, "err", err)
		return nil, wrapUpstream(ctx, err)
	}

	// Peek, а не обычное чтение: GET не должен незаметно помечать письмо
	// прочитанным — за это отвечает отдельная ручка.
	section := &imap.FetchItemBodySection{Peek: true}
	buffers, err := c.Fetch(imap.UIDSetNum(num), &imap.FetchOptions{
		UID:           true,
		Envelope:      true,
		Flags:         true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		slog.Error("не удалось загрузить письмо", "mailbox", mailbox, "uid", uid, "err", err)
		return nil, wrapUpstream(ctx, err)
	}
	if len(buffers) == 0 {
		return nil, mail.ErrNotFound
	}

	return buildMessageFull(buffers[0], section), nil
}

func (s *Source) MarkSeen(ctx context.Context, mailbox, uid string) error {
	num, err := parseUID(uid)
	if err != nil {
		return err
	}

	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer logout(c)

	if _, err := c.Select(mailbox, nil).Wait(); err != nil {
		slog.Error("не удалось выбрать папку на запись", "mailbox", mailbox, "err", err)
		return wrapUpstream(ctx, err)
	}

	cmd := c.Store(imap.UIDSetNum(num), &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagSeen},
	}, nil)
	if err := cmd.Close(); err != nil {
		slog.Error("не удалось пометить письмо прочитанным", "mailbox", mailbox, "uid", uid, "err", err)
		return wrapUpstream(ctx, err)
	}

	return nil
}

// parseUID отсекает нечисловой uid ещё до подключения к серверу.
// Возвращает ErrNotFound, а не ошибку разбора: для потребителя ручки
// «такого письма нет» и «такого UID не бывает» — одно и то же.
func parseUID(uid string) (imap.UID, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(uid), 10, 32)
	if err != nil || n == 0 {
		return 0, mail.ErrNotFound
	}
	return imap.UID(n), nil
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
