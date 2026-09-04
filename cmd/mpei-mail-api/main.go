// Command mpei-mail-api поднимает HTTP-сервис, отдающий письма из почты МЭИ
// в виде JSON — чтобы шедулерная задача дёргала одну ручку вместо IMAP.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gyattalert/mpei-mail-api/internal/config"
	"github.com/gyattalert/mpei-mail-api/internal/httpapi"
	"github.com/gyattalert/mpei-mail-api/internal/mail"
	"github.com/gyattalert/mpei-mail-api/internal/mail/imapsrc"
	"github.com/gyattalert/mpei-mail-api/internal/mail/owasrc"
)

const shutdownGrace = 10 * time.Second

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		// Отказ стартовать — намеренный: сервис с пустым API_TOKEN означал бы
		// ручку с доступом к почте, открытую наружу без авторизации.
		slog.Error("некорректная конфигурация", "err", err)
		os.Exit(1)
	}

	var src mail.Source
	var upstream string
	switch cfg.MailSource {
	case "owa":
		src = owasrc.New(cfg.OWABaseURL, cfg.User, cfg.Pass)
		upstream = cfg.OWABaseURL
	default:
		src = imapsrc.New(cfg.IMAPAddr, cfg.User, cfg.Pass)
		upstream = cfg.IMAPAddr
	}
	handler := withTimeout(httpapi.New(src, cfg.APIToken, cfg.MailSource), cfg.RequestTimeout)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("слушаю", "addr", cfg.ListenAddr, "source", cfg.MailSource, "upstream", upstream)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("сервер остановился", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("получен сигнал остановки, завершаюсь")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("не удалось завершиться мягко", "err", err)
	}
}

// withTimeout ограничивает время обработки запроса. Дедлайн уходит в контекст
// и через него рвёт IMAP-сессию, если клиент отвалился или сервер завис.
func withTimeout(next http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
