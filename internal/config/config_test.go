package config

import "testing"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadRequiresSecrets(t *testing.T) {
	for _, missing := range []string{"MPEI_USER", "MPEI_PASS", "API_TOKEN"} {
		m := map[string]string{"MPEI_USER": "u", "MPEI_PASS": "p", "API_TOKEN": "t"}
		delete(m, missing)
		if _, err := Load(env(m)); err == nil {
			t.Fatalf("Load() без %s должен возвращать ошибку", missing)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"MPEI_USER": "u", "MPEI_PASS": "p", "API_TOKEN": "t"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != "127.0.0.1:8080" {
		t.Errorf("ListenAddr = %q, want 127.0.0.1:8080", c.ListenAddr)
	}
	if c.IMAPAddr != "mail.mpei.ru:993" {
		t.Errorf("IMAPAddr = %q, want mail.mpei.ru:993", c.IMAPAddr)
	}
	if c.MailSource != "owa" {
		t.Errorf("MailSource = %q, want owa", c.MailSource)
	}
	if c.OWABaseURL != "https://mail.mpei.ru" {
		t.Errorf("OWABaseURL = %q, want https://mail.mpei.ru", c.OWABaseURL)
	}
	if c.RequestTimeout.String() != "30s" {
		t.Errorf("RequestTimeout = %v, want 30s", c.RequestTimeout)
	}
}

func TestLoadRejectsUnknownSource(t *testing.T) {
	_, err := Load(env(map[string]string{
		"MPEI_USER": "u", "MPEI_PASS": "p", "API_TOKEN": "t", "MAIL_SOURCE": "pop3",
	}))
	if err == nil {
		t.Fatal("Load() с MAIL_SOURCE=pop3 должен возвращать ошибку")
	}
}

func TestLoadAcceptsOWAAndIMAP(t *testing.T) {
	for _, src := range []string{"imap", "owa"} {
		c, err := Load(env(map[string]string{
			"MPEI_USER": "u", "MPEI_PASS": "p", "API_TOKEN": "t", "MAIL_SOURCE": src,
		}))
		if err != nil {
			t.Fatalf("MAIL_SOURCE=%s: %v", src, err)
		}
		if c.MailSource != src {
			t.Errorf("MailSource = %q, want %q", c.MailSource, src)
		}
	}
}

func TestLoadRejectsBadTimeout(t *testing.T) {
	_, err := Load(env(map[string]string{
		"MPEI_USER": "u", "MPEI_PASS": "p", "API_TOKEN": "t", "REQUEST_TIMEOUT": "полчаса",
	}))
	if err == nil {
		t.Fatal("Load() с невалидным REQUEST_TIMEOUT должен возвращать ошибку")
	}
}
