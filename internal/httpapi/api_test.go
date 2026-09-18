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
	getCalls    []string
	failUIDs    map[string]bool
}

func (f *fakeSource) List(_ context.Context, q mail.ListQuery) ([]mail.Message, error) {
	f.lastQuery = q
	return f.list, f.err
}

func (f *fakeSource) Get(_ context.Context, _, uid string) (*mail.MessageFull, error) {
	f.getCalls = append(f.getCalls, uid)
	if f.failUIDs[uid] {
		return nil, mail.ErrUpstreamUnavailable
	}
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
		// Несуществующая папка — вина запроса, а не апстрима: ретрай её не
		// вылечит, поэтому 400, а не 502.
		{mail.ErrMailboxNotFound, http.StatusBadRequest, "bad_request"},
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

// ?full=true склеивает листинг с телами: одного запроса должно хватать, чтобы
// собрать историю, вместо N+1 походов со стороны потребителя.
func TestListFullLoadsBodies(t *testing.T) {
	src := &fakeSource{
		list: []mail.Message{{UID: "1"}, {UID: "2"}},
		full: &mail.MessageFull{Text: "тело"},
	}
	rec := do(t, New(src, "s", "owa"), "GET", "/messages?full=true", "s")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body struct {
		Messages  []mail.MessageFull `json:"messages"`
		Count     int                `json:"count"`
		Failed    int                `json:"failed"`
		Truncated bool               `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 2 || body.Failed != 0 || body.Truncated {
		t.Fatalf("тело = %+v", body)
	}
	if body.Messages[0].Text != "тело" {
		t.Errorf("текст письма не доехал: %+v", body.Messages[0])
	}
	if len(src.getCalls) != 2 {
		t.Errorf("Get вызван %d раз, want 2", len(src.getCalls))
	}
}

// Нечитаемое письмо не должно ронять всю выборку — история собирается
// пакетно, и один сбойный элемент из двадцати это не повод отдать 502.
func TestListFullSkipsUnreadable(t *testing.T) {
	src := &fakeSource{
		list:     []mail.Message{{UID: "1"}, {UID: "2"}},
		full:     &mail.MessageFull{Text: "тело"},
		failUIDs: map[string]bool{"2": true},
	}
	rec := do(t, New(src, "s", "owa"), "GET", "/messages?full=true", "s")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body struct {
		Count  int `json:"count"`
		Failed int `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 1 || body.Failed != 1 {
		t.Fatalf("count=%d failed=%d, want 1/1", body.Count, body.Failed)
	}
}

// Потолок full отдельный от limit: иначе один запрос уходил бы в почтовый
// сервер на десятки минут.
func TestListFullTruncatesAtMax(t *testing.T) {
	list := make([]mail.Message, maxFullLimit+10)
	for i := range list {
		list[i].UID = string(rune('a' + i%26))
	}
	src := &fakeSource{list: list, full: &mail.MessageFull{}}
	rec := do(t, New(src, "s", "owa"), "GET", "/messages?full=true&limit=100", "s")

	var body struct {
		Count     int  `json:"count"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != maxFullLimit || !body.Truncated {
		t.Fatalf("count=%d truncated=%v, want %d/true", body.Count, body.Truncated, maxFullLimit)
	}
}

// fakeBatchSource умеет GetMany — как owasrc. Нужен, чтобы проверить, что
// HTTP-слой выбирает пакетный путь, а не бьёт источник по одному письму.
type fakeBatchSource struct {
	fakeSource
	batchCalls int
	batchUIDs  []string
}

func (f *fakeBatchSource) GetMany(_ context.Context, _ string, uids []string) ([]*mail.MessageFull, []error) {
	f.batchCalls++
	f.batchUIDs = uids
	out := make([]*mail.MessageFull, len(uids))
	errs := make([]error, len(uids))
	for i := range uids {
		out[i] = f.full
	}
	return out, errs
}

// Источник с GetMany должен опрашиваться одним вызовом. Для OWA это не вопрос
// скорости: повторный Get там означает повторный логин, а серия логинов
// временно закрывает доступ к ящику.
func TestListFullUsesBatchWhenAvailable(t *testing.T) {
	src := &fakeBatchSource{
		fakeSource: fakeSource{
			list: []mail.Message{{UID: "1"}, {UID: "2"}, {UID: "3"}},
			full: &mail.MessageFull{Text: "тело"},
		},
	}
	rec := do(t, New(src, "s", "owa"), "GET", "/messages?full=true", "s")
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, want 200: %s", rec.Code, rec.Body)
	}
	if src.batchCalls != 1 {
		t.Errorf("GetMany вызван %d раз, want 1", src.batchCalls)
	}
	if len(src.getCalls) != 0 {
		t.Errorf("Get вызван %d раз, хотя источник пакетный", len(src.getCalls))
	}
	if len(src.batchUIDs) != 3 {
		t.Errorf("в пачку ушло %d uid, want 3", len(src.batchUIDs))
	}
}
