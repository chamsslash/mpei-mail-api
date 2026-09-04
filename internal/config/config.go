// Package config читает настройки процесса из окружения.
package config

import (
	"fmt"
	"time"
)

type Config struct {
	MailSource     string
	IMAPAddr       string
	OWABaseURL     string
	User           string
	Pass           string
	APIToken       string
	ListenAddr     string
	RequestTimeout time.Duration
}

// Load собирает конфиг из окружения. Отсутствие любого секрета — ошибка старта:
// сервис с пустым API_TOKEN означал бы открытый наружу доступ к чужой почте,
// и тихий дефолт здесь опаснее отказа подниматься.
func Load(getenv func(string) string) (*Config, error) {
	// Дефолт — owa: для аккаунтов МЭИ IMAP и EWS выключены администраторами
	// (993 обрывает TLS, EWS отвечает 403), рабочим остаётся только веб-OWA.
	c := &Config{
		MailSource: or(getenv("MAIL_SOURCE"), "owa"),
		IMAPAddr:   or(getenv("IMAP_ADDR"), "mail.mpei.ru:993"),
		OWABaseURL: or(getenv("OWA_BASE_URL"), "https://mail.mpei.ru"),
		User:       getenv("MPEI_USER"),
		Pass:       getenv("MPEI_PASS"),
		APIToken:   getenv("API_TOKEN"),
		ListenAddr: or(getenv("LISTEN_ADDR"), "127.0.0.1:8080"),
	}

	for _, f := range []struct{ name, val string }{
		{"MPEI_USER", c.User}, {"MPEI_PASS", c.Pass}, {"API_TOKEN", c.APIToken},
	} {
		if f.val == "" {
			return nil, fmt.Errorf("не задана обязательная переменная %s", f.name)
		}
	}

	if c.MailSource != "imap" && c.MailSource != "owa" {
		return nil, fmt.Errorf("MAIL_SOURCE=%q не поддерживается, доступно: imap, owa", c.MailSource)
	}

	d, err := time.ParseDuration(or(getenv("REQUEST_TIMEOUT"), "30s"))
	if err != nil {
		return nil, fmt.Errorf("REQUEST_TIMEOUT: %w", err)
	}
	c.RequestTimeout = d

	return c, nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
