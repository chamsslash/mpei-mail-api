package owasrc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gyattalert/mpei-mail-api/internal/mail"
)

// fakeOWA — заглушка сервера OWA на тех же фикстурах, что и парсер. Она
// нужна, чтобы проверять не только разбор HTML, но и сам сценарий работы
// источника: сколько запросов уходит, с каким canary и в какую папку.
type fakeOWA struct {
	srv *httptest.Server

	mu          sync.Mutex
	posts       []postRecord
	folderPages []string // id папок, которые источник открывал отдельно
	reads       int
	loggedOff   bool
}

type postRecord struct {
	cmd       string
	itemID    string
	canary    string // скрытое поле X-OWA-CANARY со страницы
	hidcanary string // то, что OWA берёт из cookie UserContext
	url       string
}

// userCtxCookie — значение, которое заглушка отдаёт в cookie UserContext.
// Оно намеренно отличается от canary в разметке фикстуры: именно на этом
// различии видно, какой из токенов уходит в форму.
const userCtxCookie = "USERCTX-FROM-COOKIE"

func newFakeOWA(t *testing.T) *fakeOWA {
	t.Helper()
	inbox := readFixture(t, "inbox.html")
	message := readFixture(t, "message.html")

	f := &fakeOWA{}
	mux := http.NewServeMux()

	mux.HandleFunc("/owa/logoff.owa", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.loggedOff = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/owa/", func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			f.mu.Lock()
			f.posts = append(f.posts, postRecord{
				cmd:       r.PostForm.Get("hidcmdpst"),
				itemID:    r.PostForm.Get("chkmsg"),
				canary:    r.PostForm.Get("X-OWA-CANARY"),
				hidcanary: r.PostForm.Get("hidcanary"),
				url:       r.URL.String(),
			})
			f.mu.Unlock()
			_, _ = w.Write([]byte(inbox))
			return
		}

		http.SetCookie(w, &http.Cookie{Name: "UserContext", Value: userCtxCookie, Path: "/"})

		q := r.URL.Query()
		switch {
		case q.Get("a") == "Read":
			f.mu.Lock()
			f.reads++
			f.mu.Unlock()
			_, _ = w.Write([]byte(message))
		case q.Get("ae") == "Folder":
			f.mu.Lock()
			f.folderPages = append(f.folderPages, q.Get("id"))
			f.mu.Unlock()
			_, _ = w.Write([]byte(inbox))
		default:
			_, _ = w.Write([]byte(inbox))
		}
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOWA) source() *Source { return New(f.srv.URL, "user", "pass") }

func (f *fakeOWA) snapshot() ([]postRecord, []string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]postRecord(nil), f.posts...), append([]string(nil), f.folderPages...), f.loggedOff
}

// firstUnreadUID возвращает uid первого письма фикстуры — оно непрочитанное.
func firstUnreadUID(t *testing.T) string {
	t.Helper()
	for _, m := range parseInbox(readFixture(t, "inbox.html")) {
		if !m.Seen {
			return m.UID
		}
	}
	t.Fatal("в фикстуре нет непрочитанных писем")
	return ""
}

func TestListReadsInboxAndReleasesSession(t *testing.T) {
	f := newFakeOWA(t)

	msgs, err := f.source().List(context.Background(), mail.ListQuery{Mailbox: "INBOX", Limit: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("List не вернул писем")
	}

	_, folders, loggedOff := f.snapshot()
	// «Входящие» — это и есть корневая страница: отдельного запроса за папкой
	// быть не должно.
	if len(folders) != 0 {
		t.Errorf("за INBOX ушёл лишний запрос за папкой: %v", folders)
	}
	if !loggedOff {
		t.Error("сессия OWA не освобождена — logoff не вызван")
	}
}

func TestListLimitAndUnseen(t *testing.T) {
	f := newFakeOWA(t)
	src := f.source()

	all, err := src.List(context.Background(), mail.ListQuery{Mailbox: "INBOX", Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	limited, err := src.List(context.Background(), mail.ListQuery{Mailbox: "INBOX", Limit: 1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limit=1 вернул %d писем", len(limited))
	}

	unseen, err := src.List(context.Background(), mail.ListQuery{Mailbox: "INBOX", Limit: 100, Unseen: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(unseen) == 0 || len(unseen) >= len(all) {
		t.Errorf("unseen=true вернул %d из %d — фильтр не сработал", len(unseen), len(all))
	}
	for _, m := range unseen {
		if m.Seen {
			t.Errorf("в выдаче unseen оказалось прочитанное письмо %q", m.Subject)
		}
	}
}

// Порядок строк OWA задаётся сохранённой в ящике сортировкой (на живом
// ящике встречалась сортировка по отправителю), поэтому источник обязан
// упорядочить письма сам — иначе limit возьмёт не самые свежие.
func TestListReturnsNewestFirst(t *testing.T) {
	f := newFakeOWA(t)
	msgs, err := f.source().List(context.Background(), mail.ListQuery{Mailbox: "INBOX", Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i-1].Date.Before(msgs[i].Date) {
			t.Errorf("письмо %d (%s) свежее предыдущего (%s)", i,
				msgs[i].Date.Format("2006-01-02 15:04"), msgs[i-1].Date.Format("2006-01-02 15:04"))
		}
	}
}

func TestListOtherFolderIsFetchedSeparately(t *testing.T) {
	f := newFakeOWA(t)

	if _, err := f.source().List(context.Background(), mail.ListQuery{Mailbox: "Отправленные", Limit: 5}); err != nil {
		t.Fatalf("List: %v", err)
	}

	_, folders, _ := f.snapshot()
	if len(folders) != 1 {
		t.Fatalf("ожидался один запрос за папкой, было %d: %v", len(folders), folders)
	}

	want, ok := resolveFolder(readFixture(t, "inbox.html"), "Отправленные")
	if !ok {
		t.Fatal("папка «Отправленные» не разрешена в фикстуре")
	}
	if folders[0] != want.ID {
		t.Errorf("открыта папка с id %q, want %q", folders[0], want.ID)
	}
}

func TestUnknownMailboxIsRejected(t *testing.T) {
	f := newFakeOWA(t)
	ctx := context.Background()

	_, err := f.source().List(ctx, mail.ListQuery{Mailbox: "Такой папки нет", Limit: 5})
	if !errors.Is(err, mail.ErrMailboxNotFound) {
		t.Errorf("List: err = %v, want ErrMailboxNotFound", err)
	}

	if err := f.source().MarkSeen(ctx, "Такой папки нет", firstUnreadUID(t)); !errors.Is(err, mail.ErrMailboxNotFound) {
		t.Errorf("MarkSeen: err = %v, want ErrMailboxNotFound", err)
	}
}

// Открытие письма в OWA помечает его прочитанным, поэтому источник обязан
// вернуть флаг обратно. Команда принимается, только если повторяет форму
// самого OWA: адрес из переменных страницы и hidcanary из cookie
// UserContext. При отступлении сервер отвечает 200 и молча ничего не делает,
// то есть неудача выглядит как успех.
func TestGetRestoresUnreadFlagLikeOWAForm(t *testing.T) {
	f := newFakeOWA(t)
	uid := firstUnreadUID(t)

	full, err := f.source().Get(context.Background(), "INBOX", uid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if full.Seen {
		t.Error("Get вернул письмо прочитанным — контракт требует не менять состояние ящика")
	}

	posts, _, _ := f.snapshot()
	if len(posts) != 1 {
		t.Fatalf("ожидалась одна POST-команда, было %d: %+v", len(posts), posts)
	}
	p := posts[0]
	if p.cmd != "markunread" {
		t.Errorf("команда = %q, want markunread", p.cmd)
	}
	if p.hidcanary != userCtxCookie {
		t.Errorf("hidcanary = %q, want значение cookie UserContext %q", p.hidcanary, userCtxCookie)
	}
	if p.hidcanary == extractCanary(readFixture(t, "inbox.html")) {
		t.Error("в hidcanary ушёл токен из разметки — OWA перезаписывает его кукой")
	}

	// Адрес формы строится по переменным страницы (gtFrmActn), а не из
	// ссылок навигации: на стартовой странице ae=StartPage, а не Folder.
	want := parseListContext(readFixture(t, "inbox.html")).formAction("")
	if p.url != want {
		t.Errorf("POST ушёл на %q, want %q", p.url, want)
	}
}

// Флаг прочитанности страница чтения не показывает — он известен только из
// списка, снятого до открытия. Раньше ручка всегда отдавала seen=false.
func TestGetReportsSeenForReadMessage(t *testing.T) {
	f := newFakeOWA(t)

	var seenUID string
	for _, m := range parseInbox(readFixture(t, "inbox.html")) {
		if m.Seen {
			seenUID = m.UID
			break
		}
	}
	if seenUID == "" {
		t.Skip("в фикстуре нет прочитанных писем")
	}

	full, err := f.source().Get(context.Background(), "INBOX", seenUID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !full.Seen {
		t.Error("прочитанное письмо вернулось с seen=false")
	}
}

func TestGetOnSeenMessageDoesNotWrite(t *testing.T) {
	f := newFakeOWA(t)

	var seenUID string
	for _, m := range parseInbox(readFixture(t, "inbox.html")) {
		if m.Seen {
			seenUID = m.UID
			break
		}
	}
	if seenUID == "" {
		t.Skip("в фикстуре нет прочитанных писем")
	}

	if _, err := f.source().Get(context.Background(), "INBOX", seenUID); err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Письмо и так прочитано — восстанавливать нечего, лишних записей быть
	// не должно.
	if posts, _, _ := f.snapshot(); len(posts) != 0 {
		t.Errorf("на прочитанном письме ушли записи: %+v", posts)
	}
}

func TestMarkSeen(t *testing.T) {
	f := newFakeOWA(t)
	uid := firstUnreadUID(t)

	if err := f.source().MarkSeen(context.Background(), "INBOX", uid); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	posts, _, _ := f.snapshot()
	if len(posts) != 1 {
		t.Fatalf("ожидалась одна POST-команда, было %d: %+v", len(posts), posts)
	}
	if posts[0].cmd != "markread" {
		t.Errorf("команда = %q, want markread", posts[0].cmd)
	}
	if want, _ := decodeUID(uid); posts[0].itemID != want {
		t.Errorf("itemID = %q, want %q", posts[0].itemID, want)
	}
	if posts[0].hidcanary != userCtxCookie {
		t.Errorf("hidcanary = %q, want %q — иначе OWA молча проигнорирует команду",
			posts[0].hidcanary, userCtxCookie)
	}
}

// Повторная пометка уже прочитанного письма обязана быть тихой: контракт
// ручки объявляет её идемпотентной.
func TestMarkSeenIsIdempotent(t *testing.T) {
	f := newFakeOWA(t)

	var seenUID string
	for _, m := range parseInbox(readFixture(t, "inbox.html")) {
		if m.Seen {
			seenUID = m.UID
			break
		}
	}
	if seenUID == "" {
		t.Skip("в фикстуре нет прочитанных писем")
	}

	if err := f.source().MarkSeen(context.Background(), "INBOX", seenUID); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	if posts, _, _ := f.snapshot(); len(posts) != 0 {
		t.Errorf("на уже прочитанном письме ушла запись: %+v", posts)
	}
}

func TestBadUIDIsNotFound(t *testing.T) {
	f := newFakeOWA(t)
	ctx := context.Background()

	for _, uid := range []string{"", "не base64!!"} {
		if _, err := f.source().Get(ctx, "INBOX", uid); !errors.Is(err, mail.ErrNotFound) {
			t.Errorf("Get(%q): err = %v, want ErrNotFound", uid, err)
		}
	}
}

func TestPing(t *testing.T) {
	f := newFakeOWA(t)
	if err := f.source().Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

// Деградированную страницу OWA (200, но без canary) нельзя принимать за
// пустой ящик — это недоступность апстрима.
func TestDegradedPageIsUpstreamUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>Сервер занят</body></html>"))
	}))
	defer srv.Close()

	src := New(srv.URL, "user", "pass")
	ctx := context.Background()

	if _, err := src.List(ctx, mail.ListQuery{Mailbox: "INBOX", Limit: 10}); !errors.Is(err, mail.ErrUpstreamUnavailable) {
		t.Errorf("List: err = %v, want ErrUpstreamUnavailable", err)
	}
	if err := src.Ping(ctx); !errors.Is(err, mail.ErrUpstreamUnavailable) {
		t.Errorf("Ping: err = %v, want ErrUpstreamUnavailable", err)
	}
}

func TestUpstreamStatusIsMappedToDomainError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))

		_, err := New(srv.URL, "user", "pass").List(context.Background(), mail.ListQuery{Mailbox: "INBOX", Limit: 10})
		if !errors.Is(err, mail.ErrUpstreamUnavailable) {
			t.Errorf("статус %d: err = %v, want ErrUpstreamUnavailable", status, err)
		}
		// Учётные данные не должны утекать в текст ошибки, уходящий наружу.
		if err != nil && (strings.Contains(err.Error(), "pass") || strings.Contains(err.Error(), "user")) {
			t.Errorf("статус %d: в тексте ошибки учётные данные: %v", status, err)
		}
		srv.Close()
	}
}

// scrambledInbox — страница списка, где строки идут не по дате: так
// выглядит ящик, отсортированный в OWA по отправителю. Разметка сведена к
// тем элементам, на которые опирается источник.
const scrambledInbox = `<html><body>
<script>
var a_sAe = "StartPage";
var a_sT = "";
var a_sFldId = "FLD-ID";
var a_sPg = "1";
var a_iSlUsng = 0;
</script>
<input type="hidden" name="X-OWA-CANARY" value="CANARY">
<a name="lnkFldr" href="?ae=Folder&t=IPF.Note&id=FLD-ID" title="Входящие">Входящие</a>
<table>
<tr style="font-weight:normal">
  <td><input type="checkbox" name="chkmsg" value="ID-STAROE"></td>
  <td>Отправитель А</td>
  <td><a onclick="onClkRdMsg(this,'IPM.Note',0,0)">старое</a></td>
  <td>02.09.2026 10:00</td><td>1 КБ</td>
</tr>
<tr style="font-weight:bold">
  <td><input type="checkbox" name="chkmsg" value="ID-SVEZHEE"></td>
  <td>Отправитель Б</td>
  <td><a onclick="onClkRdMsg(this,'IPM.Note',1,0)">свежее</a></td>
  <td>05.09.2026 20:27</td><td>2 КБ</td>
</tr>
<tr style="font-weight:normal">
  <td><input type="checkbox" name="chkmsg" value="ID-SREDNEE"></td>
  <td>Отправитель В</td>
  <td><a onclick="onClkRdMsg(this,'IPM.Note',2,0)">среднее</a></td>
  <td>03.09.2026 17:32</td><td>3 КБ</td>
</tr>
</table></body></html>`

// Источник обязан упорядочить письма сам: на живом ящике порядок строк
// задаётся сохранённой в OWA сортировкой, и при limit=1 иначе вернётся
// не самое свежее письмо, а первое попавшееся.
func TestListSortsScrambledPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(scrambledInbox))
	}))
	defer srv.Close()
	src := New(srv.URL, "user", "pass")

	all, err := src.List(context.Background(), mail.ListQuery{Mailbox: "INBOX", Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := []string{}
	for _, m := range all {
		got = append(got, m.Subject)
	}
	want := []string{"свежее", "среднее", "старое"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("порядок = %v, want %v", got, want)
		}
	}

	one, err := src.List(context.Background(), mail.ListQuery{Mailbox: "INBOX", Limit: 1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(one) != 1 || one[0].Subject != "свежее" {
		t.Errorf("limit=1 вернул %+v, ожидалось самое свежее письмо", one)
	}
}
