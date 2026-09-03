package httpapi

import (
	"errors"
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

	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "count": len(msgs)})
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
