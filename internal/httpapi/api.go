// Package httpapi — HTTP-слой сервиса. Не знает, каким протоколом
// забираются письма: работает только с интерфейсом mail.Source.
package httpapi

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gyattalert/mpei-mail-api/internal/mail"
)

const upstreamCacheTTL = 30 * time.Second

type api struct {
	src        mail.Source
	token      string
	sourceName string

	mu          sync.Mutex
	pingAt      time.Time
	pingHealthy bool
}

// New собирает http.Handler со всеми ручками сервиса.
func New(src mail.Source, token, sourceName string) http.Handler {
	a := &api{src: src, token: token, sourceName: sourceName}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.handleHealthz)
	mux.Handle("GET /messages", a.auth(http.HandlerFunc(a.handleList)))
	mux.Handle("GET /messages/{uid}", a.auth(http.HandlerFunc(a.handleGet)))
	mux.Handle("POST /messages/{uid}/seen", a.auth(http.HandlerFunc(a.handleSeen)))

	return recoverer(mux)
}

func (a *api) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "нужен корректный Bearer-токен")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("паника в хендлере", "path", r.URL.Path, "panic", v)
				writeErr(w, http.StatusInternalServerError, "internal", "внутренняя ошибка")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *api) handleHealthz(w http.ResponseWriter, r *http.Request) {
	upstream := "ok"
	if !a.upstreamHealthy(r.Context()) {
		upstream = "fail"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "ok",
		"source":   a.sourceName,
		"upstream": upstream,
	})
}

// upstreamHealthy кэширует результат на 30 секунд: иначе частые опросы health
// превращаются в шторм логинов на почтовый сервер.
func (a *api) upstreamHealthy(ctx context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.pingAt.IsZero() && time.Since(a.pingAt) < upstreamCacheTTL {
		return a.pingHealthy
	}

	a.pingHealthy = a.src.Ping(ctx) == nil
	a.pingAt = time.Now()
	return a.pingHealthy
}
