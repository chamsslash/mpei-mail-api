//go:build integration

// Интеграционные тесты против живого почтового сервера.
//
// Запуск:
//
//	MPEI_USER=... MPEI_PASS=... go test -tags integration ./internal/mail/imapsrc/ -v
//
// Именно этот тест даёт практический ответ на вопрос, доступен ли порт 993
// из целевой среды: с машины разработки TLS-handshake на 993 обрывается.
package imapsrc

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gyattalert/mpei-mail-api/internal/mail"
)

func newSource(t *testing.T) *Source {
	t.Helper()

	user, pass := os.Getenv("MPEI_USER"), os.Getenv("MPEI_PASS")
	if user == "" || pass == "" {
		t.Skip("нет MPEI_USER/MPEI_PASS — интеграционный тест пропущен")
	}

	addr := os.Getenv("IMAP_ADDR")
	if addr == "" {
		addr = "mail.mpei.ru:993"
	}

	return New(addr, user, pass)
}

func TestPingReachesServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := newSource(t).Ping(ctx); err != nil {
		t.Fatalf("Ping: %v (порт 993 недоступен или креды неверны)", err)
	}
}

func TestListReturnsRecentMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	msgs, err := newSource(t).List(ctx, mail.ListQuery{Mailbox: "INBOX", Limit: 3})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	t.Logf("получено писем: %d", len(msgs))
	for _, m := range msgs {
		if m.UID == "" {
			t.Error("письмо без UID")
		}
		t.Logf("uid=%s seen=%v from=%s subject=%q snippet=%.60q",
			m.UID, m.Seen, m.From.Email, m.Subject, m.Snippet)
	}
}

func TestGetDoesNotMarkSeen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	src := newSource(t)

	msgs, err := src.List(ctx, mail.ListQuery{Mailbox: "INBOX", Limit: 5, Unseen: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) == 0 {
		t.Skip("нет непрочитанных писем — проверить нечего")
	}

	uid := msgs[0].UID
	if _, err := src.Get(ctx, "INBOX", uid); err != nil {
		t.Fatalf("Get: %v", err)
	}

	after, err := src.List(ctx, mail.ListQuery{Mailbox: "INBOX", Limit: 50, Unseen: true})
	if err != nil {
		t.Fatalf("повторный List: %v", err)
	}
	for _, m := range after {
		if m.UID == uid {
			return // письмо всё ещё непрочитано — этого и добивались
		}
	}
	t.Errorf("после Get письмо uid=%s пропало из непрочитанных: BODY.PEEK не сработал", uid)
}

func TestGetUnknownUIDIsNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := newSource(t).Get(ctx, "INBOX", "не-число")
	if err != mail.ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
