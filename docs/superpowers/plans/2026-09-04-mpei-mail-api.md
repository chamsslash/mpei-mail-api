# mpei-mail-api Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** HTTP-сервис на Go, отдающий письма из почты МЭИ в JSON, чтобы шедулерная таска в Claude дёргала одну ручку вместо работы с IMAP.

**Architecture:** Три слоя с зависимостями внутрь: `httpapi` (роутинг, JSON, коды ошибок) → `mail` (доменные типы и интерфейс `Source`) ← `mail/imapsrc` (go-imap/v2). Протокол доступа к почте изолирован за интерфейсом `Source`, потому что разведка показала недоступность 993/995 при живом Exchange EWS по 443, и замена реализации не должна задевать контракт ручек.

**Tech Stack:** Go 1.27, `github.com/emersion/go-imap/v2`, `github.com/emersion/go-message`, `net/http.ServeMux` (Go 1.22+ паттерны с `{uid}`), stdlib `testing`.

**Spec:** `docs/superpowers/specs/2026-09-04-mpei-mail-api-design.md`

## Global Constraints

- Модуль: `github.com/gyattalert/mpei-mail-api`. Go 1.24+ (собирается на 1.27.1).
- Зависимости только: `emersion/go-imap/v2`, `emersion/go-message`. HTTP-фреймворки запрещены — роутинг на `net/http.ServeMux`.
- `uid` в интерфейсе `Source` и во всём JSON — `string`, никогда `uint32`.
- Пустой `API_TOKEN` → процесс не стартует.
- Сравнение токена — `subtle.ConstantTimeCompare`.
- `InsecureSkipVerify` запрещён.
- Пароль и токен никогда не попадают в логи, в том числе внутри текстов ошибок из библиотеки.
- Чтение письма всегда через `BODY.PEEK[...]` — `GET` не меняет флаг `\Seen`.
- Дефолт `LISTEN_ADDR` — `127.0.0.1:8080`.
- Конверт ошибки везде: `{"error":{"code":"...","message":"..."}}`.

---

### Task 1: Каркас модуля и конфиг из ENV

**Files:**
- Create: `go.mod`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: ничего.
- Produces: `config.Config{MailSource, IMAPAddr, User, Pass, APIToken, ListenAddr string; RequestTimeout time.Duration}`, `config.Load(getenv func(string) string) (*Config, error)`.

- [ ] **Step 1: Инициализировать модуль**

```bash
cd ~/projects/mpei-mail-api
go mod init github.com/gyattalert/mpei-mail-api
```

- [ ] **Step 2: Написать падающий тест**

`internal/config/config_test.go`:

```go
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
	if c.MailSource != "imap" {
		t.Errorf("MailSource = %q, want imap", c.MailSource)
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
```

- [ ] **Step 3: Убедиться, что тест падает**

Run: `go test ./internal/config/`
Expected: FAIL — `undefined: Load`

- [ ] **Step 4: Реализовать конфиг**

`internal/config/config.go`:

```go
// Package config читает настройки процесса из окружения.
package config

import (
	"fmt"
	"time"
)

type Config struct {
	MailSource     string
	IMAPAddr       string
	User           string
	Pass           string
	APIToken       string
	ListenAddr     string
	RequestTimeout time.Duration
}

// Load собирает конфиг из окружения. Отсутствие любого секрета — ошибка старта:
// сервис с пустым API_TOKEN означал бы открытый наружу доступ к почте.
func Load(getenv func(string) string) (*Config, error) {
	c := &Config{
		MailSource: or(getenv("MAIL_SOURCE"), "imap"),
		IMAPAddr:   or(getenv("IMAP_ADDR"), "mail.mpei.ru:993"),
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

	if c.MailSource != "imap" {
		return nil, fmt.Errorf("MAIL_SOURCE=%q не поддерживается, доступно: imap", c.MailSource)
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
```

- [ ] **Step 5: Прогнать тесты**

Run: `go test ./internal/config/`
Expected: PASS

- [ ] **Step 6: Коммит**

```bash
git add go.mod internal/config/
git commit -m "feat(config): чтение настроек из окружения с fail-fast на секретах"
```

---

### Task 2: Доменные типы и интерфейс Source

**Files:**
- Create: `internal/mail/mail.go`
- Create: `internal/mail/errors.go`

**Interfaces:**
- Consumes: ничего.
- Produces: типы `Address`, `Message`, `MessageFull`, `Attachment`, `ListQuery`; интерфейс `Source`; сентинелы `ErrNotFound`, `ErrUpstreamUnavailable`, `ErrUpstreamTimeout`.

Тестов нет: пакет содержит только объявления типов, тестировать здесь нечего. Поведение проверяется в Task 4 и Task 5.

- [ ] **Step 1: Объявить доменные типы**

`internal/mail/mail.go`:

```go
// Package mail описывает доменную модель почты и интерфейс источника писем.
// Пакет не знает ни про HTTP, ни про конкретный протокол доступа к серверу.
package mail

import (
	"context"
	"time"
)

type Address struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Message struct {
	UID            string    `json:"uid"`
	From           Address   `json:"from"`
	To             []Address `json:"to"`
	Subject        string    `json:"subject"`
	Date           time.Time `json:"date"`
	Seen           bool      `json:"seen"`
	HasAttachments bool      `json:"has_attachments"`
	Snippet        string    `json:"snippet"`
}

type Attachment struct {
	Name string `json:"name"`
	MIME string `json:"mime"`
	Size int64  `json:"size"`
}

type MessageFull struct {
	Message
	CC          []Address    `json:"cc"`
	Text        string       `json:"text"`
	HTML        string       `json:"html"`
	Attachments []Attachment `json:"attachments"`
}

type ListQuery struct {
	Mailbox string
	Limit   int
	Unseen  bool
	Since   time.Time // нулевое значение — без ограничения по дате
}

// Source — источник писем. uid здесь строка, а не число: в IMAP это UID,
// но в EWS ItemId — base64, и вторая реализация не должна ломать контракт ручек.
type Source interface {
	List(ctx context.Context, q ListQuery) ([]Message, error)
	Get(ctx context.Context, mailbox, uid string) (*MessageFull, error)
	MarkSeen(ctx context.Context, mailbox, uid string) error
	Ping(ctx context.Context) error
}
```

- [ ] **Step 2: Объявить сентинельные ошибки**

`internal/mail/errors.go`:

```go
package mail

import "errors"

// Сентинелы, по которым httpapi выбирает HTTP-код. Реализации Source обязаны
// заворачивать в них ошибки протокола: наружу не должен утекать текст ошибки
// библиотеки, в котором может оказаться строка подключения с логином.
var (
	ErrNotFound            = errors.New("письмо не найдено")
	ErrUpstreamUnavailable = errors.New("почтовый сервер недоступен")
	ErrUpstreamTimeout     = errors.New("почтовый сервер не ответил вовремя")
)
```

- [ ] **Step 3: Проверить сборку**

Run: `go build ./...`
Expected: успех, без вывода

- [ ] **Step 4: Коммит**

```bash
git add internal/mail/
git commit -m "feat(mail): доменная модель писем и интерфейс Source"
```

---

### Task 3: HTTP-слой — авторизация, конверт ошибок, /healthz

**Files:**
- Create: `internal/httpapi/api.go`
- Create: `internal/httpapi/errors.go`
- Test: `internal/httpapi/api_test.go`

**Interfaces:**
- Consumes: `mail.Source`, `mail.Err*`.
- Produces: `httpapi.New(src mail.Source, token string, sourceName string) http.Handler`; внутренние `writeErr(w, code string, status int, msg string)`, `writeJSON(w, status int, v any)`.

- [ ] **Step 1: Написать падающие тесты**

`internal/httpapi/api_test.go`:

```go
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gyattalert/mpei-mail-api/internal/mail"
)

// fakeSource — управляемая заглушка Source для контрактных тестов.
type fakeSource struct {
	list     []mail.Message
	full     *mail.MessageFull
	err      error
	pingErr  error
	seenUID  string
	lastQuery mail.ListQuery
}

func (f *fakeSource) List(_ context.Context, q mail.ListQuery) ([]mail.Message, error) {
	f.lastQuery = q
	return f.list, f.err
}
func (f *fakeSource) Get(_ context.Context, _, _ string) (*mail.MessageFull, error) {
	return f.full, f.err
}
func (f *fakeSource) MarkSeen(_ context.Context, _, uid string) error {
	f.seenUID = uid
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
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Upstream != "fail" {
		t.Errorf("upstream = %q, want fail", body.Upstream)
	}
}
```

- [ ] **Step 2: Убедиться, что тесты падают**

Run: `go test ./internal/httpapi/`
Expected: FAIL — `undefined: New`

- [ ] **Step 3: Реализовать конверт ошибок**

`internal/httpapi/errors.go`:

```go
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

// writeSourceErr переводит доменную ошибку в HTTP-код. Разделение 502 и 504
// функционально: 504 шедулеру имеет смысл повторить сразу, 502 (например,
// выключенный администраторами IMAP) повторять бессмысленно.
func writeSourceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mail.ErrNotFound):
		writeErr(w, http.StatusNotFound, "message_not_found", "письмо не найдено")
	case errors.Is(err, mail.ErrUpstreamTimeout), errors.Is(err, context.DeadlineExceeded):
		writeErr(w, http.StatusGatewayTimeout, "upstream_timeout", "почтовый сервер не ответил вовремя")
	case errors.Is(err, mail.ErrUpstreamUnavailable):
		writeErr(w, http.StatusBadGateway, "upstream_unavailable", "почтовый сервер недоступен")
	default:
		slog.Error("необработанная ошибка источника", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "внутренняя ошибка")
	}
}
```

- [ ] **Step 4: Реализовать роутер, авторизацию и /healthz**

`internal/httpapi/api.go`:

```go
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

	mu         sync.Mutex
	pingAt     time.Time
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
		"status": "ok", "source": a.sourceName, "upstream": upstream,
	})
}

// upstreamHealthy кэширует результат на 30 секунд, чтобы частые опросы health
// не превращались в шторм логинов на почтовый сервер.
func (a *api) upstreamHealthy(ctx context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.pingAt) < upstreamCacheTTL {
		return a.pingHealthy
	}
	a.pingHealthy = a.src.Ping(ctx) == nil
	a.pingAt = time.Now()
	return a.pingHealthy
}
```

- [ ] **Step 5: Прогнать тесты**

Run: `go test ./internal/httpapi/`
Expected: PASS

- [ ] **Step 6: Коммит**

```bash
git add internal/httpapi/
git commit -m "feat(httpapi): роутер, Bearer-авторизация, конверт ошибок и /healthz"
```

---

### Task 4: Ручки писем и разбор параметров

**Files:**
- Create: `internal/httpapi/messages.go`
- Modify: `internal/httpapi/api_test.go` (дописать тесты ручек)

**Interfaces:**
- Consumes: `mail.Source`, `writeJSON`, `writeErr`, `writeSourceErr`, `fakeSource` из Task 3.
- Produces: методы `(*api).handleList`, `(*api).handleGet`, `(*api).handleSeen`; `parseListQuery(r *http.Request) (mail.ListQuery, error)`.

- [ ] **Step 1: Дописать падающие тесты**

Добавить в `internal/httpapi/api_test.go`:

```go
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

func TestSeenReturns204(t *testing.T) {
	src := &fakeSource{}
	rec := do(t, New(src, "s", "imap"), "POST", "/messages/77/seen", "s")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("код = %d, want 204: %s", rec.Code, rec.Body)
	}
	if src.seenUID != "77" {
		t.Errorf("seenUID = %q, want 77", src.seenUID)
	}
}
```

- [ ] **Step 2: Убедиться, что тесты падают**

Run: `go test ./internal/httpapi/`
Expected: FAIL — `a.handleList undefined`

- [ ] **Step 3: Реализовать ручки**

`internal/httpapi/messages.go`:

```go
package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gyattalert/mpei-mail-api/internal/mail"
)

const (
	defaultLimit = 20
	maxLimit     = 100
)

var errBadParam = errors.New("некорректный параметр")

func parseListQuery(r *http.Request) (mail.ListQuery, error) {
	q := mail.ListQuery{
		Mailbox: valueOr(r.URL.Query().Get("mailbox"), "INBOX"),
		Limit:   defaultLimit,
		Unseen:  r.URL.Query().Get("unseen") == "true",
	}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return q, errBadParam
		}
		q.Limit = min(n, maxLimit)
	}

	if raw := r.URL.Query().Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t, err = time.Parse("2006-01-02", raw)
			if err != nil {
				return q, errBadParam
			}
		}
		q.Since = t
	}

	return q, nil
}

func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (a *api) handleList(w http.ResponseWriter, r *http.Request) {
	q, err := parseListQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "некорректные limit, since или mailbox")
		return
	}

	msgs, err := a.src.List(r.Context(), q)
	if err != nil {
		writeSourceErr(w, err)
		return
	}
	if msgs == nil {
		msgs = []mail.Message{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "count": len(msgs)})
}

func (a *api) handleGet(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "пустой uid")
		return
	}

	msg, err := a.src.Get(r.Context(), valueOr(r.URL.Query().Get("mailbox"), "INBOX"), uid)
	if err != nil {
		writeSourceErr(w, err)
		return
	}
	if msg == nil {
		writeSourceErr(w, mail.ErrNotFound)
		return
	}

	writeJSON(w, http.StatusOK, msg)
}

func (a *api) handleSeen(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "пустой uid")
		return
	}

	if err := a.src.MarkSeen(r.Context(), valueOr(r.URL.Query().Get("mailbox"), "INBOX"), uid); err != nil {
		writeSourceErr(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 4: Прогнать тесты**

Run: `go test ./internal/httpapi/`
Expected: PASS

- [ ] **Step 5: Коммит**

```bash
git add internal/httpapi/
git commit -m "feat(httpapi): ручки списка, письма и пометки прочитанным"
```

---

### Task 5: Реализация Source на IMAP

**Files:**
- Create: `internal/mail/imapsrc/imapsrc.go`
- Create: `internal/mail/imapsrc/parse.go`
- Test: `internal/mail/imapsrc/imapsrc_integration_test.go`

**Interfaces:**
- Consumes: `mail.Source`, `mail.Message`, `mail.MessageFull`, `mail.Err*`.
- Produces: `imapsrc.New(addr, user, pass string) *Source`, реализующий `mail.Source`.

Реализация держит коннект на запрос, без пула: для таски, дёргающей ручку раз в несколько минут, экономия 300–600 мс на TLS и LOGIN не стоит класса багов с протухшими сессиями и гонкой `Select(ReadOnly)` против `Select(RW)`.

- [ ] **Step 1: Добавить зависимости**

```bash
go get github.com/emersion/go-imap/v2
go get github.com/emersion/go-message
```

- [ ] **Step 2: Реализовать подключение и операции**

`internal/mail/imapsrc/imapsrc.go` — структура `Source{addr, user, pass}`, метод `connect(ctx)` (`imapclient.DialTLS` с `tls.Config{ServerName}`, затем `Login().Wait()`), и четыре метода интерфейса. Все ошибки протокола заворачиваются: таймаут контекста → `mail.ErrUpstreamTimeout`, остальные ошибки соединения и логина → `mail.ErrUpstreamUnavailable`. Текст ошибки библиотеки наружу не отдаётся, только логируется.

`List`: `Select(mailbox, &imap.SelectOptions{ReadOnly: true})` → `Search` по `imap.SearchCriteria{Flag: []imap.Flag{imap.FlagUnseen}}` при `Unseen` и `Since` при непустой дате → взять хвост из `Limit` UID → `Fetch` с `UID`, `Envelope`, `Flags`, `BodyStructure{Extended:true}` и секцией текстовой части с `Peek: true` и `Partial: &imap.SectionPartial{Size: 2048}`.

`Get`: тот же `Select(ReadOnly:true)`, `Fetch` секции `{Peek: true}` целиком, разбор через `go-message`.

`MarkSeen`: `Select(mailbox, nil)` → `Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil).Wait()`.

`Ping`: `connect` + `Logout`.

- [ ] **Step 3: Реализовать разбор письма**

`internal/mail/imapsrc/parse.go` — перевод `imap.Envelope` в `mail.Address`/`mail.Message`, обход `BodyStructure.Walk` для поиска номера текстовой части и сбора вложений, разбор полного тела через `go-message/mail.CreateReader`.

Выбор части для сниппета: первая `text/plain`, при её отсутствии — первая `text/html` с вырезанием тегов. Слепо брать часть `1` нельзя: в `multipart/alternative` порядок частей задаёт отправитель.

- [ ] **Step 4: Проверить сборку и vet**

Run: `go build ./... && go vet ./...`
Expected: успех

- [ ] **Step 5: Написать интеграционный тест**

`internal/mail/imapsrc/imapsrc_integration_test.go` с `//go:build integration`. Тест берёт `MPEI_USER`/`MPEI_PASS`/`IMAP_ADDR` из окружения и вызывает `t.Skip`, если их нет. Проверяет `Ping`, затем `List` с `Limit: 3`.

Это первый практический ответ на вопрос, жив ли 993 из целевой среды.

- [ ] **Step 6: Прогнать**

Run: `go test -tags integration ./internal/mail/imapsrc/ -v`
Expected: SKIP без кредов; при наличии кредов — реальный ответ о доступности 993

- [ ] **Step 7: Коммит**

```bash
git add go.mod go.sum internal/mail/imapsrc/
git commit -m "feat(imapsrc): реализация Source поверх go-imap/v2"
```

---

### Task 6: Сборка процесса, Dockerfile, документация

**Files:**
- Create: `cmd/mpei-mail-api/main.go`
- Create: `Dockerfile`
- Modify: `README.md`

**Interfaces:**
- Consumes: `config.Load`, `imapsrc.New`, `httpapi.New`.
- Produces: исполняемый бинарь.

- [ ] **Step 1: Написать main**

`cmd/mpei-mail-api/main.go`: `config.Load(os.Getenv)` → при ошибке `log.Fatal` (fail fast на пустом `API_TOKEN`) → `imapsrc.New` → `httpapi.New` → обёртка, вешающая `context.WithTimeout(r.Context(), cfg.RequestTimeout)` на каждый запрос → `http.Server{Addr: cfg.ListenAddr, ReadHeaderTimeout: 5s, WriteTimeout: 60s}` → graceful shutdown по `SIGINT`/`SIGTERM` с 10-секундным дедлайном.

- [ ] **Step 2: Проверить сборку**

Run: `CGO_ENABLED=0 go build -o /tmp/mpei-mail-api ./cmd/mpei-mail-api`
Expected: успех

- [ ] **Step 3: Проверить fail-fast на пустом токене**

Run: `MPEI_USER=u MPEI_PASS=p /tmp/mpei-mail-api`
Expected: выход с ненулевым кодом и сообщением про `API_TOKEN`

- [ ] **Step 4: Проверить ручки живьём**

Поднять с фиктивными кредами на свободном порту, затем:

```bash
curl -s localhost:8080/healthz
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/messages
curl -s -o /dev/null -w '%{http_code}\n' -H 'Authorization: Bearer testtoken' localhost:8080/messages
```

Expected: `{"status":"ok",...}`; `401`; `502` (креды фиктивные, до МЭИ не достучаться — и это корректное поведение)

- [ ] **Step 5: Написать Dockerfile и README**

Dockerfile: многостадийный, сборка на `golang:1.27`, рантайм на `gcr.io/distroless/static`, `CGO_ENABLED=0`. README: таблица ENV, примеры вызова всех четырёх ручек через curl, раздел про подключение шедулерной таски.

- [ ] **Step 6: Финальная проверка**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: всё зелёное

- [ ] **Step 7: Коммит**

```bash
git add cmd/ Dockerfile README.md
git commit -m "feat: сборка процесса, graceful shutdown, Dockerfile и README"
```
