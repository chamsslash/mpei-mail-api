package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gyattalert/mpei-mail-api/internal/mail"
)

type errBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("не удалось записать ответ", "err", err)
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	var b errBody
	b.Error.Code = code
	b.Error.Message = msg
	writeJSON(w, status, b)
}

// writeSourceErr переводит доменную ошибку в HTTP-код.
//
// Разделение 502 и 504 функционально: 504 шедулеру имеет смысл повторить
// сразу, 502 (например, выключенный администраторами IMAP) — бессмысленно.
// Наружу отдаётся только своя формулировка: текст ошибки библиотеки может
// содержать строку подключения с логином и уходит лишь в лог.
func writeSourceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mail.ErrNotFound):
		writeErr(w, http.StatusNotFound, "message_not_found", "письмо не найдено")
	// Несуществующая папка — ошибка параметров запроса, а не апстрима:
	// ретраить её бессмысленно, чинится она на стороне потребителя.
	case errors.Is(err, mail.ErrMailboxNotFound):
		writeErr(w, http.StatusBadRequest, "bad_request", "папка не найдена")
	case errors.Is(err, mail.ErrUpstreamTimeout), errors.Is(err, context.DeadlineExceeded):
		writeErr(w, http.StatusGatewayTimeout, "upstream_timeout", "почтовый сервер не ответил вовремя")
	case errors.Is(err, mail.ErrUpstreamUnavailable):
		writeErr(w, http.StatusBadGateway, "upstream_unavailable", "почтовый сервер недоступен")
	default:
		slog.Error("необработанная ошибка источника", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "внутренняя ошибка")
	}
}
