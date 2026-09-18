package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gyattalert/mpei-mail-api/internal/mail"
)

const (
	defaultLimit = 20
	maxLimit     = 100
)

var errBadParam = errors.New("некорректный параметр")

func parseListQuery(r *http.Request) (mail.ListQuery, error) {
	v := r.URL.Query()

	q := mail.ListQuery{
		Mailbox: valueOr(v.Get("mailbox"), "INBOX"),
		Limit:   defaultLimit,
		Unseen:  v.Get("unseen") == "true",
	}

	if raw := v.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return q, errBadParam
		}
		q.Limit = min(n, maxLimit)
	}

	if raw := v.Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t, err = time.Parse("2006-01-02", raw)
			if err != nil {
				return q, errBadParam
			}
		}
		q.Since = t
	}

	return q, nil
}

func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (a *api) handleList(w http.ResponseWriter, r *http.Request) {
	q, err := parseListQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "некорректные limit, since или mailbox")
		return
	}

	msgs, err := a.src.List(r.Context(), q)
	if err != nil {
		writeSourceErr(w, err)
		return
	}
	// Пустой результат отдаём как [], а не null: иначе потребитель ручки
	// обязан отдельно обрабатывать отсутствующий массив.
	if msgs == nil {
		msgs = []mail.Message{}
	}

	if r.URL.Query().Get("full") == "true" {
		a.writeFullList(w, r, q.Mailbox, msgs)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "count": len(msgs)})
}

// maxFullLimit ограничивает ?full=true отдельно от limit. Причина в цене: в
// листинге тел писем нет, и каждое приходится забирать отдельным походом в
// почтовый сервер на 2–4 секунды. Полсотни — это уже около трёх минут в одном
// HTTP-запросе; больше отдавать одним куском бессмысленно, клиент отвалится по
// таймауту раньше.
const maxFullLimit = 50

// batchGetter — источник, умеющий забрать пачку писем дешевле, чем повторный
// вызов Get. Опциональный интерфейс, а не часть mail.Source: для IMAP разницы
// нет, а обязательный метод заставил бы писать там пустую обёртку.
type batchGetter interface {
	GetMany(ctx context.Context, mailbox string, uids []string) ([]*mail.MessageFull, []error)
}

// writeFullList дозагружает тела к уже полученному листингу.
//
// Через batchGetter, если источник его поддерживает. Это не оптимизация, а
// условие работоспособности: у OWA каждый Get — отдельный логин, и два десятка
// логинов подряд Exchange встречает auth_error, после чего ящик временно не
// пускает вообще. Пакетный путь укладывается в один логин.
func (a *api) writeFullList(w http.ResponseWriter, r *http.Request, mailbox string, msgs []mail.Message) {
	truncated := false
	if len(msgs) > maxFullLimit {
		msgs, truncated = msgs[:maxFullLimit], true
	}

	uids := make([]string, len(msgs))
	for i, m := range msgs {
		uids[i] = m.UID
	}

	var (
		got  []*mail.MessageFull
		errs []error
	)
	if b, ok := a.src.(batchGetter); ok {
		got, errs = b.GetMany(r.Context(), mailbox, uids)
	} else {
		got, errs = a.getSequentially(r.Context(), mailbox, uids)
	}

	out := make([]*mail.MessageFull, 0, len(got))
	failed := 0
	for i, full := range got {
		if full == nil || (i < len(errs) && errs[i] != nil) {
			// Одно нечитаемое письмо не должно рушить всю историю: пропускаем
			// его и сообщаем счётчиком, сколько потерялось.
			var err error
			if i < len(errs) {
				err = errs[i]
			}
			slog.Warn("не удалось забрать тело письма", "mailbox", mailbox, "uid", uids[i], "err", err)
			failed++
			continue
		}
		out = append(out, full)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"messages":  out,
		"count":     len(out),
		"failed":    failed,
		"truncated": truncated,
	})
}

// getSequentially — запасной путь для источников без GetMany.
func (a *api) getSequentially(ctx context.Context, mailbox string, uids []string) ([]*mail.MessageFull, []error) {
	out := make([]*mail.MessageFull, len(uids))
	errs := make([]error, len(uids))
	for i, uid := range uids {
		if err := ctx.Err(); err != nil {
			for j := i; j < len(errs); j++ {
				errs[j] = err
			}
			break
		}
		out[i], errs[i] = a.src.Get(ctx, mailbox, uid)
	}
	return out, errs
}

func (a *api) handleGet(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "пустой uid")
		return
	}

	msg, err := a.src.Get(r.Context(), valueOr(r.URL.Query().Get("mailbox"), "INBOX"), uid)
	if err != nil {
		writeSourceErr(w, err)
		return
	}
	if msg == nil {
		writeSourceErr(w, mail.ErrNotFound)
		return
	}

	writeJSON(w, http.StatusOK, msg)
}

func (a *api) handleSeen(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "пустой uid")
		return
	}

	mailbox := valueOr(r.URL.Query().Get("mailbox"), "INBOX")
	if err := a.src.MarkSeen(r.Context(), mailbox, uid); err != nil {
		writeSourceErr(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
