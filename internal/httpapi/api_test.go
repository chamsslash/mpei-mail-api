package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gyattalert/mpei-mail-api/internal/mail"
)

// fakeSource — управляемая заглушка Source. Весь контракт ручек проверяется
// на ней, без сети: сетевое поведение — предмет интеграционного теста imapsrc.
type fakeSource struct {
	list        []mail.Message
	full        *mail.MessageFull
	err         error
	pingErr     error
	seenUID     string
	seenMailbox string
	lastQuery   mail.ListQuery
}

func (f *fakeSource) List(_ context.Context, q mail.ListQuery) ([]mail.Message, error) {
	f.lastQuery = q
	return f.list, f.err
}

func (f *fakeSource) Get(_ context.Context, _, _ string) (*mail.MessageFull, error) {
	return f.full, f.err
}

func (f *fakeSource) MarkSeen(_ context.Context, mailbox, uid string) error {
	f.seenMailbox, f.seenUID = mailbox, uid
	return f.err
}

func (f *fakeSource) Ping(context.Context) error { return f.pingErr }

func do(t *testing.T, h http.Handler, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело ответа не разбирается как конверт ошибки: %v (%s)", err, rec.Body)
	}
	return body.Error.Code
}

func TestAuthRequired(t *testing.T) {
	h := New(&fakeSource{}, "secret", "imap")
	for _, tc := range []struct{ name, token string }{
		{"без токена", ""},
		{"неверный токен", "wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, "GET", "/messages", tc.token)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("код = %d, want 401", rec.Code)
			}
			if got := errCode(t, rec); got != "unauthorized" {
				t.Errorf("code = %q, want unauthorized", got)
			}
		})
	}
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	rec := do(t, New(&fakeSource{}, "secret", "imap"), "GET", "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, want 200", rec.Code)
	}
	var body struct{ Status, Source, Upstream string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Source != "imap" || body.Upstream != "ok" {
		t.Errorf("тело = %+v", body)
	}
}

// Недоступность МЭИ не должна ронять код ответа health: иначе k8s-liveness
// начнёт перезапускать под из-за чужой почты, а рестарт её не чинит.
func TestHealthzStays200WhenUpstreamDown(t *testing.T) {
	src := &fakeSource{pingErr: mail.ErrUpstreamUnavailable}
	rec := do(t, New(src, "secret", "imap"), "GET", "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, want 200", rec.Code)
	}
	var body struct{ Upstream string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Upstream != "fail" {
		t.Errorf("upstream = %q, want fail", body.Upstream)
	}
}

func TestListReturnsMessages(t *testing.T) {
	src := &fakeSource{list: []mail.Message{{UID: "42", Subject: "тема"}}}
	rec := do(t, New(src, "s", "imap"), "GET", "/messages?limit=5&unseen=true", "s")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body struct {
		Messages []mail.Message `json:"messages"`
		Count    int            `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 1 || len(body.Messages) != 1 || body.Messages[0].UID != "42" {
		t.Fatalf("тело = %+v", body)
	}
	if src.lastQuery.Limit != 5 || !src.lastQuery.Unseen {
		t.Errorf("query = %+v", src.lastQuery)
	}
	if src.lastQuery.Mailbox != "INBOX" {
		t.Errorf("mailbox = %q, want INBOX", src.lastQuery.Mailbox)
	}
}

// Пустой список должен сериализоваться в [], а не в null: потребитель ручки
// иначе обязан отдельно обрабатывать nil.
func TestListEmptyIsArrayNotNull(t *testing.T) {
	rec := do(t, New(&fakeSource{}, "s", "imap"), "GET", "/messages", "s")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["messages"]) != "[]" {
		t.Errorf("messages = %s, want []", raw["messages"])
	}
}

func TestListDefaultsAndCaps(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"/messages", 20},
		{"/messages?limit=1000", 100},
	} {
		src := &fakeSource{}
		do(t, New(src, "s", "imap"), "GET", tc.query, "s")
		if src.lastQuery.Limit != tc.want {
			t.Errorf("%s: limit = %d, want %d", tc.query, src.lastQuery.Limit, tc.want)
		}
	}
}

func TestListRejectsBadParams(t *testing.T) {
	for _, q := range []string{"/messages?limit=abc", "/messages?limit=0", "/messages?since=вчера"} {
		rec := do(t, New(&fakeSource{}, "s", "imap"), "GET", q, "s")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: код = %d, want 400", q, rec.Code)
			continue
		}
		if got := errCode(t, rec); got != "bad_request" {
			t.Errorf("%s: code = %q, want bad_request", q, got)
		}
	}
}

func TestListAcceptsBothDateFormats(t *testing.T) {
	for _, q := range []string{"/messages?since=2026-09-01", "/messages?since=2026-09-01T10:00:00Z"} {
		src := &fakeSource{}
		do(t, New(src, "s", "imap"), "GET", q, "s")
		if src.lastQuery.Since.IsZero() {
			t.Errorf("%s: Since не разобран", q)
		}
	}
}

func TestUpstreamErrorsMapToCodes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{mail.ErrNotFound, http.StatusNotFound, "message_not_found"},
		{mail.ErrUpstreamUnavailable, http.StatusBadGateway, "upstream_unavailable"},
		{mail.ErrUpstreamTimeout, http.StatusGatewayTimeout, "upstream_timeout"},
	} {
		rec := do(t, New(&fakeSource{err: tc.err}, "s", "imap"), "GET", "/messages/1", "s")
		if rec.Code != tc.status {
			t.Errorf("%v: код = %d, want %d", tc.err, rec.Code, tc.status)
		}
		if got := errCode(t, rec); got != tc.code {
			t.Errorf("%v: code = %q, want %q", tc.err, got, tc.code)
		}
	}
}

func TestGetReturnsMessage(t *testing.T) {
	src := &fakeSource{full: &mail.MessageFull{
		Message: mail.Message{UID: "9", Subject: "привет"},
		Text:    "тело",
	}}
	rec := do(t, New(src, "s", "imap"), "GET", "/messages/9", "s")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, want 200: %s", rec.Code, rec.Body)
	}
	var got mail.MessageFull
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.UID != "9" || got.Text != "тело" {
		t.Errorf("тело = %+v", got)
	}
}

func TestSeenReturns204(t *testing.T) {
	src := &fakeSource{}
	rec := do(t, New(src, "s", "imap"), "POST", "/messages/77/seen", "s")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("код = %d, want 204: %s", rec.Code, rec.Body)
	}
	if src.seenUID != "77" {
		t.Errorf("seenUID = %q, want 77", src.seenUID)
	}
	if src.seenMailbox != "INBOX" {
		t.Errorf("seenMailbox = %q, want INBOX", src.seenMailbox)
	}
}

func TestMailboxParamPassedThrough(t *testing.T) {
	src := &fakeSource{}
	do(t, New(src, "s", "imap"), "POST", "/messages/1/seen?mailbox=Sent", "s")
	if src.seenMailbox != "Sent" {
		t.Errorf("seenMailbox = %q, want Sent", src.seenMailbox)
	}
}
