package owasrc

import (
	"os"
	"strings"
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
	f, ok := resolveFolder(inbox, "INBOX")
	if !ok {
		t.Fatal("папка INBOX не разрешена")
	}
	if f.ID == "" {
		t.Error("id папки не извлечён")
	}
}

func TestParseFolders(t *testing.T) {
	folders := parseFolders(readFixture(t, "inbox.html"))
	if len(folders) == 0 {
		t.Fatal("навигация по папкам не разобрана")
	}
	for _, f := range folders {
		if f.Title == "" || f.ID == "" {
			t.Errorf("папка разобрана не полностью: %+v", f)
		}
		// В href идентификатор приходит percent-encoded; наружу он должен
		// выходить уже раскодированным, иначе POST-команды уйдут не туда.
		if strings.Contains(f.ID, "%2b") || strings.Contains(f.ID, "%2B") {
			t.Errorf("id папки %q остался закодированным: %q", f.Title, f.ID)
		}
		t.Logf("папка %-20q id=%s", f.Title, f.ID[:12]+"…")
	}
}

// Имя папки приходит от потребителя, а Exchange МЭИ печатает русские
// подписи — резолвер обязан покрывать оба словаря и не путать регистр.
func TestResolveFolderAliases(t *testing.T) {
	inbox := readFixture(t, "inbox.html")

	sameAs := func(name string) string {
		f, ok := resolveFolder(inbox, name)
		if !ok {
			t.Fatalf("папка %q не разрешена", name)
		}
		return f.ID
	}

	if sameAs("INBOX") != sameAs("Входящие") || sameAs("INBOX") != sameAs("inbox") {
		t.Error("INBOX/Входящие/inbox должны указывать на одну папку")
	}
	if sameAs("Sent") != sameAs("Отправленные") {
		t.Error("Sent и Отправленные должны указывать на одну папку")
	}
	if sameAs("Junk") != sameAs("Нежелательная почта") {
		t.Error("Junk и Нежелательная почта должны указывать на одну папку")
	}

	// Несуществующая папка не должна тихо схлопываться во «Входящие»:
	// иначе потребитель получит чужие письма под видом запрошенных.
	if f, ok := resolveFolder(inbox, "Нет такой папки"); ok {
		t.Errorf("несуществующая папка разрешилась в %+v", f)
	}
}

// text не должен схлопывать письмо в одну строку: в перечислениях вида
// «02.09.2026 – 6 выплат / 01.10.2026 – 4 выплаты» без переводов строк
// теряется граница между пунктами.
func TestParseMessageTextKeepsLineBreaks(t *testing.T) {
	full := parseMessage(readFixture(t, "message.html"))

	if !strings.Contains(full.Text, "\n") {
		t.Fatal("в тексте письма нет ни одного перевода строки")
	}
	for _, ln := range strings.Split(full.Text, "\n") {
		if ln != strings.TrimSpace(ln) {
			t.Errorf("строка не обрезана по краям: %q", ln)
		}
	}
	if strings.Contains(full.Text, "\n\n\n") {
		t.Error("в тексте остались серии пустых строк")
	}

	// Сниппет, наоборот, однострочный.
	if strings.ContainsAny(full.Snippet, "\n\r") {
		t.Errorf("сниппет многострочный: %q", full.Snippet)
	}
	if r := []rune(full.Snippet); len(r) > 301 {
		t.Errorf("сниппет длиннее 300 символов: %d", len(r))
	}
}

func TestParseMessageHTML(t *testing.T) {
	full := parseMessage(readFixture(t, "message.html"))

	if full.HTML == "" {
		t.Fatal("html-тело письма пустое")
	}
	// Именно разметка, а не текст: иначе поле не отличается от text.
	if !strings.Contains(full.HTML, "<p") {
		t.Error("в html-теле нет разметки абзацев")
	}
	// Служебные вставки OWA к письму не относятся.
	if strings.Contains(full.HTML, "<meta") {
		t.Error("в html-тело просочились служебные meta от OWA")
	}
	if strings.Contains(full.HTML, "<script") {
		t.Error("в html-тело просочился script")
	}
}

func TestAttachmentMIME(t *testing.T) {
	full := parseMessage(readFixture(t, "message.html"))
	for _, a := range full.Attachments {
		if a.MIME == "" {
			t.Errorf("вложение %q без MIME", a.Name)
		}
	}

	// Типы офисных документов во встроенной таблице Go отсутствуют, а
	// системного /etc/mime.types в distroless-образе нет — они обязаны
	// определяться собственной таблицей, а не деградировать в octet-stream.
	cases := map[string]string{
		"Category-23.pdf": "application/pdf",
		"смета.docx":      "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"списки.xlsx":     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"скан.PDF":        "application/pdf",
		"архив.zip":       "application/zip",
		"без-расширения":  "application/octet-stream",
	}
	for name, want := range cases {
		if got := mimeByName(name); got != want {
			t.Errorf("mimeByName(%q) = %q, want %q", name, got, want)
		}
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
