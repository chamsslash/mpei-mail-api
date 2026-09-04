package owasrc

import (
	"os"
	"testing"
)

// Тесты гоняются на реальных страницах OWA Light, снятых с mail.mpei.ru
// (testdata/), — именно на них видно, что парсер попадает в живую вёрстку,
// а не в выдуманную.

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseInbox(t *testing.T) {
	msgs := parseInbox(readFixture(t, "inbox.html"))
	if len(msgs) == 0 {
		t.Fatal("не разобрано ни одного письма")
	}

	first := msgs[0]
	if first.UID == "" {
		t.Error("первое письмо без UID")
	}
	if _, err := decodeUID(first.UID); err != nil {
		t.Errorf("UID %q не декодируется обратно в ItemId: %v", first.UID, err)
	}
	if first.Subject == "" {
		t.Error("первое письмо без темы")
	}
	if first.Date.IsZero() {
		t.Errorf("первое письмо без даты (subject=%q)", first.Subject)
	}

	// В снятом ящике первое письмо непрочитанное — строка жирная.
	if first.Seen {
		t.Errorf("первое письмо помечено прочитанным, ожидалось непрочитанное")
	}

	t.Logf("разобрано писем: %d", len(msgs))
	for _, m := range msgs {
		t.Logf("seen=%-5v attach=%-5v %s | %q", m.Seen, m.HasAttachments, m.Date.Format("2006-01-02 15:04"), m.Subject)
	}
}

func TestParseMessage(t *testing.T) {
	full := parseMessage(readFixture(t, "message.html"))

	if full.Subject == "" {
		t.Error("тема письма не разобрана")
	}
	if full.From.Email == "" && full.From.Name == "" {
		t.Error("отправитель не разобран")
	}
	if len(full.To) == 0 {
		t.Error("получатель не разобран")
	}
	if full.Date.IsZero() {
		t.Error("дата письма не разобрана")
	}
	if full.Text == "" {
		t.Error("тело письма пустое")
	}
	if full.Snippet == "" {
		t.Error("сниппет пустой")
	}

	t.Logf("subject: %s", full.Subject)
	t.Logf("from:    %s <%s>", full.From.Name, full.From.Email)
	t.Logf("to:      %+v", full.To)
	t.Logf("date:    %s", full.Date.Format("2006-01-02 15:04 MST"))
	t.Logf("attach:  %+v", full.Attachments)
	t.Logf("snippet: %s", full.Snippet)
}

func TestParseMessageAttachments(t *testing.T) {
	full := parseMessage(readFixture(t, "message.html"))
	if !full.HasAttachments {
		t.Fatal("во вложенном письме не найдено вложений")
	}
	for _, a := range full.Attachments {
		if a.Name == "" {
			t.Error("вложение без имени")
		}
		if a.Size <= 0 {
			t.Errorf("вложение %q без размера", a.Name)
		}
	}
}

func TestExtractCanaryAndFolder(t *testing.T) {
	inbox := readFixture(t, "inbox.html")
	if extractCanary(inbox) == "" {
		t.Error("canary-токен не извлечён")
	}
	if extractFolderID(inbox) == "" {
		t.Error("id папки не извлечён")
	}
}

func TestUIDRoundTrip(t *testing.T) {
	// ItemId содержит + и /, которые ломают маршрут /messages/{uid};
	// кодировка uid должна их устранять и полностью обращаться.
	raw := "RgAAAACVYZVB17tgTohbJCvLZ37hBwB1dl+pIzRES5t3b6AyJxOd/AAAAAAk+AAAJ"
	enc := encodeUID(raw)
	if containsAny(enc, "+/=") {
		t.Errorf("закодированный uid содержит небезопасные символы: %q", enc)
	}
	back, err := decodeUID(enc)
	if err != nil {
		t.Fatal(err)
	}
	if back != raw {
		t.Errorf("round-trip: got %q, want %q", back, raw)
	}
}

func TestParseAddressField(t *testing.T) {
	cases := map[string]struct{ name, email string }{
		"Новостная информация НИУ МЭИ [news_info@mpei.ru]": {"Новостная информация НИУ МЭИ", "news_info@mpei.ru"},
		"user@mpei.ru":         {"", "user@mpei.ru"},
		"Иванов Иван Иванович": {"Иванов Иван Иванович", ""},
	}
	for in, want := range cases {
		got := parseAddressField(in)
		if got.Name != want.name || got.Email != want.email {
			t.Errorf("parseAddressField(%q) = {%q,%q}, want {%q,%q}", in, got.Name, got.Email, want.name, want.email)
		}
	}
}

func containsAny(s, chars string) bool {
	for _, c := range chars {
		for _, r := range s {
			if r == c {
				return true
			}
		}
	}
	return false
}
