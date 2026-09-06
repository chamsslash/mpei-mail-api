package owasrc

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/gyattalert/mpei-mail-api/internal/mail"
)

// mskLocation — часовой пояс, в котором OWA печатает даты. Грузим один раз;
// при отсутствии базы tzdata откатываемся на фиксированный +03:00.
var mskLocation = loadMSK()

func loadMSK() *time.Location {
	if loc, err := time.LoadLocation("Europe/Moscow"); err == nil {
		return loc
	}
	return time.FixedZone("MSK", 3*60*60)
}

// ---- извлечение мелких значений ----

var canaryRe = regexp.MustCompile(`name="X-OWA-CANARY"\s+value="([^"]+)"`)

func extractCanary(page string) string {
	if m := canaryRe.FindStringSubmatch(page); m != nil {
		return m[1]
	}
	return ""
}

// ---- контекст страницы списка ----

// listContext — переменные страницы списка, из которых OWA собирает адрес
// отправки формы (gtPrfx/gtFrmActn в msglst.js).
//
// Строить этот адрес по своему разумению нельзя: на стартовой странице
// ae=StartPage и пустой t, а не Folder/IPF.Note, как можно решить по ссылкам
// навигации. Форма, отправленная не туда, принимается сервером с кодом 200,
// но команда молча не выполняется.
type listContext struct {
	ae     string
	t      string
	fldID  string // уже percent-encoded — подставляется в адрес как есть
	slUsng string
	pg     string
}

var (
	reVarAe     = regexp.MustCompile(`var\s+a_sAe\s*=\s*"([^"]*)"`)
	reVarT      = regexp.MustCompile(`var\s+a_sT\s*=\s*"([^"]*)"`)
	reVarFldID  = regexp.MustCompile(`var\s+a_sFldId\s*=\s*"([^"]*)"`)
	reVarSlUsng = regexp.MustCompile(`var\s+a_iSlUsng\s*=\s*(\d+)`)
	reVarPg     = regexp.MustCompile(`var\s+a_sPg\s*=\s*"([^"]*)"`)
)

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func parseListContext(page string) listContext {
	lc := listContext{
		ae:     firstGroup(reVarAe, page),
		t:      firstGroup(reVarT, page),
		fldID:  firstGroup(reVarFldID, page),
		slUsng: firstGroup(reVarSlUsng, page),
		pg:     firstGroup(reVarPg, page),
	}
	if lc.slUsng == "" {
		lc.slUsng = "0"
	}
	return lc
}

// formAction повторяет gtFrmActn() из msglst.js.
func (lc listContext) formAction(base string) string {
	if lc.ae == "" {
		return ""
	}
	p := "?ae=" + lc.ae
	if lc.t != "" {
		p += "&t=" + lc.t
	}
	p += "&id=" + lc.fldID + "&slUsng=" + lc.slUsng
	if lc.pg != "" {
		p += "&pg=" + lc.pg
	}
	return base + "/owa/" + p
}

// ---- папки ящика ----

// folder — папка ящика из навигации OWA. ID нужен и как контекст для
// POST-команд markread/markunread, и чтобы открыть саму папку.
type folder struct {
	Title string
	ID    string
}

// parseFolders собирает папки из навигации: каждая — ссылка lnkFldr, подпись
// лежит в title, идентификатор — в параметре id.
func parseFolders(page string) []folder {
	root, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return nil
	}

	var out []folder
	walk(root, func(n *html.Node) {
		if n.Type != html.ElementNode || n.Data != "a" || attr(n, "name") != "lnkFldr" {
			return
		}
		u, err := url.Parse(attr(n, "href"))
		if err != nil {
			return
		}
		id := u.Query().Get("id")
		if id == "" {
			return
		}
		title := cleanText(attr(n, "title"))
		if title == "" {
			title = cleanText(text(n))
		}
		out = append(out, folder{Title: title, ID: id})
	})
	return out
}

// mailboxAliases переводит привычные имена ящиков в подписи папок OWA:
// контракт ручек говорит INBOX, а Exchange МЭИ печатает русские названия.
var mailboxAliases = map[string]string{
	"inbox":         "входящие",
	"sent":          "отправленные",
	"sent items":    "отправленные",
	"drafts":        "черновики",
	"trash":         "удаленные",
	"deleted":       "удаленные",
	"deleted items": "удаленные",
	"junk":          "нежелательная почта",
	"spam":          "нежелательная почта",
}

// normalizeFolderName приводит имя к сравнимому виду: регистр не важен, а «ё»
// в названиях папок OWA пишет то так, то иначе («Удаленные»/«Удалённые»).
func normalizeFolderName(s string) string {
	return strings.ReplaceAll(strings.ToLower(cleanText(s)), "ё", "е")
}

func isInboxName(mailbox string) bool {
	m := normalizeFolderName(mailbox)
	return m == "" || m == "inbox" || m == "входящие"
}

// resolveFolder ищет папку по имени среди навигации страницы.
//
// Возвращает false, если такой папки в ящике нет. Молча откатываться на
// «Входящие» нельзя: потребитель получил бы письма другой папки под видом
// запрошенной и не смог бы отличить это от пустого результата.
func resolveFolder(page, mailbox string) (folder, bool) {
	want := normalizeFolderName(mailbox)
	if want == "" {
		want = "inbox"
	}
	if alias, ok := mailboxAliases[want]; ok {
		want = normalizeFolderName(alias)
	}

	for _, f := range parseFolders(page) {
		if normalizeFolderName(f.Title) == want {
			return f, true
		}
	}
	return folder{}, false
}

// ---- список писем ----

// parseInbox разбирает страницу «Входящие». Каждое письмо — строка таблицы
// с чекбоксом chkmsg, в котором лежит ItemId. Непрочитанные строки помечены
// жирным начертанием.
func parseInbox(page string) []mail.Message {
	root, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return nil
	}

	var out []mail.Message
	walk(root, func(n *html.Node) {
		if n.Type != html.ElementNode || n.Data != "input" {
			return
		}
		if attr(n, "name") != "chkmsg" {
			return
		}
		itemID := attr(n, "value")
		if itemID == "" {
			return
		}
		row := ancestor(n, "tr")
		if row == nil {
			return
		}
		out = append(out, rowToMessage(row, itemID))
	})
	return out
}

func rowToMessage(row *html.Node, itemID string) mail.Message {
	m := mail.Message{
		UID:  encodeUID(itemID),
		Seen: !strings.Contains(strings.ToLower(attr(row, "style")), "bold"),
	}

	// Ячейки строки идут в порядке: отправитель, тема (ссылка onClkRdMsg),
	// дата, размер. Отправитель и тема в списке обрезаны сервером — полные
	// значения доступны при открытии письма.
	var cells []*html.Node
	walk(row, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "td" {
			cells = append(cells, n)
		}
	})

	for _, td := range cells {
		// вложение: иконка attch.png
		walk(td, func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "img" && strings.Contains(attr(n, "src"), "attch") {
				m.HasAttachments = true
			}
		})
		// тема: ссылка с onClkRdMsg
		if a := findFirst(td, func(n *html.Node) bool {
			return n.Data == "a" && strings.Contains(attr(n, "onclick"), "onClkRdMsg")
		}); a != nil && m.Subject == "" {
			m.Subject = cleanText(text(a))
		}
	}

	m.From = mail.Address{Name: senderFromRow(cells)}
	m.Date = parseListDate(dateFromRow(cells))
	return m
}

// senderFromRow берёт текст первой ячейки-«отправителя»: она идёт перед
// ячейкой с темой и не содержит ссылок.
func senderFromRow(cells []*html.Node) string {
	for _, td := range cells {
		if findFirst(td, func(n *html.Node) bool { return n.Data == "a" || n.Data == "input" || n.Data == "img" }) != nil {
			continue
		}
		if t := cleanText(text(td)); t != "" && !looksLikeDate(t) && !looksLikeSize(t) {
			return t
		}
	}
	return ""
}

func dateFromRow(cells []*html.Node) string {
	for _, td := range cells {
		if t := cleanText(text(td)); looksLikeDate(t) {
			return t
		}
	}
	return ""
}

// sortNewestFirst упорядочивает письма от свежих к старым.
//
// Полагаться на порядок строк OWA нельзя: он зависит от того, по какой
// колонке ящик в последний раз отсортировали в веб-интерфейсе, и эта
// настройка запоминается на сервере (на живом ящике наблюдалась сортировка
// по отправителю). Контракт же обещает свежие письма первыми, и именно из
// этого порядка limit берёт первые N.
func sortNewestFirst(msgs []mail.Message) {
	sort.SliceStable(msgs, func(i, j int) bool {
		a, b := msgs[i].Date, msgs[j].Date
		// Письма с неразобранной датой уезжают в конец, чтобы не вытеснять
		// заведомо свежие из выдачи под limit.
		if a.IsZero() != b.IsZero() {
			return !a.IsZero()
		}
		return a.After(b)
	})
}

func filterList(msgs []mail.Message, q mail.ListQuery) []mail.Message {
	out := msgs[:0:0]
	for _, m := range msgs {
		if q.Unseen && m.Seen {
			continue
		}
		if !q.Since.IsZero() && !m.Date.IsZero() && m.Date.Before(q.Since) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ---- одно письмо ----

func parseMessage(page string) *mail.MessageFull {
	full := &mail.MessageFull{Attachments: []mail.Attachment{}}

	root, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return full
	}

	full.Subject = cleanText(firstByClass(root, "sub"))
	full.From = parseAddressField(firstByClass(root, "rwRRO"))

	headers := headerPairs(root)
	if v := headers["отправлено"]; v != "" {
		full.Date = parseReadDate(v)
	}
	full.To = splitAddresses(headers["кому"])
	full.CC = splitAddresses(headers["копия"])
	if full.From.Name == "" && full.From.Email == "" {
		full.From = parseAddressField(headers["от"])
	}

	// Тело письма лежит в div.bdy внутри td.bdy — берём именно внутренний
	// div: во внешней ячейке кроме тела живёт вёрстка OWA.
	body := findFirst(root, func(n *html.Node) bool { return n.Data == "div" && hasClass(n, "bdy") })
	if body == nil {
		body = findFirst(root, func(n *html.Node) bool { return hasClass(n, "bdy") })
	}
	if body != nil {
		full.HTML = renderInner(body)
		full.Text = cleanMultiline(blockText(body))
	}

	// Вложения лежат ссылками на attachment.ashx внутри контейнера divAtt,
	// в тексте — «имя (размер)». Содержимое файлов не тянем, только метаданные.
	if att := findFirst(root, func(n *html.Node) bool { return attr(n, "id") == "divAtt" }); att != nil {
		walk(att, func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "a" && strings.Contains(attr(n, "href"), "attachment.ashx") {
				if name, size := parseAttachment(cleanText(text(n))); name != "" {
					full.Attachments = append(full.Attachments, mail.Attachment{
						Name: name,
						MIME: mimeByName(name),
						Size: size,
					})
				}
			}
		})
	}
	full.HasAttachments = len(full.Attachments) > 0

	if full.Text != "" {
		// Сниппет — одной строкой: разбиение на абзацы в нём только мешает.
		full.Snippet = truncateRunes(singleLine(full.Text), 300)
	}
	return full
}

// headerPairs собирает таблицу заголовков письма: подпись в ячейке класса
// hdtxt, значение — в следующей ячейке. Ключи приведены к нижнему регистру
// без двоеточия.
func headerPairs(root *html.Node) map[string]string {
	out := map[string]string{}
	walk(root, func(n *html.Node) {
		if n.Type != html.ElementNode || !hasClass(n, "hdtxt") {
			return
		}
		label := strings.ToLower(strings.TrimRight(cleanText(text(n)), ": "))
		if label == "" {
			return
		}
		if val := nextElement(n); val != nil {
			out[label] = cleanText(text(val))
		}
	})
	return out
}

var (
	sizeRe   = regexp.MustCompile(`\(([\d\s.,]+)\s*(байт|КБ|МБ|Б)\)`)
	emailRe  = regexp.MustCompile(`\[([^\]]+)\]`)
	listDate = regexp.MustCompile(`^\d{2}\.\d{2}\.\d{4}`)
)

// parseAddressField разбирает «Имя [адрес@домен]» на имя и почту.
func parseAddressField(s string) mail.Address {
	s = cleanText(s)
	if s == "" {
		return mail.Address{}
	}
	if m := emailRe.FindStringSubmatch(s); m != nil {
		name := cleanText(s[:strings.Index(s, "[")])
		return mail.Address{Name: name, Email: strings.TrimSpace(m[1])}
	}
	if strings.Contains(s, "@") && !strings.ContainsAny(s, " \t") {
		return mail.Address{Email: s}
	}
	return mail.Address{Name: s}
}

func splitAddresses(s string) []mail.Address {
	s = cleanText(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	out := make([]mail.Address, 0, len(parts))
	for _, p := range parts {
		if a := parseAddressField(p); a.Name != "" || a.Email != "" {
			out = append(out, a)
		}
	}
	return out
}

func parseAttachment(s string) (name string, size int64) {
	name = s
	if loc := sizeRe.FindStringSubmatchIndex(s); loc != nil {
		name = cleanText(s[:loc[0]])
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, s[loc[2]:loc[3]])
		unit := s[loc[4]:loc[5]]
		size = scaleSize(digits, unit)
	}
	return cleanText(name), size
}

func parseListDate(s string) time.Time {
	s = strings.ReplaceAll(cleanText(s), " ", " ")
	if t, err := time.ParseInLocation("02.01.2006 15:04", s, mskLocation); err == nil {
		return t
	}
	return time.Time{}
}

var ruMonths = map[string]int{
	"января": 1, "февраля": 2, "марта": 3, "апреля": 4, "мая": 5, "июня": 6,
	"июля": 7, "августа": 8, "сентября": 9, "октября": 10, "ноября": 11, "декабря": 12,
}

// parseReadDate разбирает дату вида «3 сентября 2026 г. 17:29».
func parseReadDate(s string) time.Time {
	f := strings.Fields(strings.ReplaceAll(cleanText(s), " ", " "))
	if len(f) < 5 {
		return time.Time{}
	}
	day := atoi(f[0])
	mon := ruMonths[strings.ToLower(f[1])]
	year := atoi(f[2])
	var hh, mm int
	for _, tok := range f {
		if strings.Contains(tok, ":") {
			hm := strings.SplitN(tok, ":", 2)
			hh, mm = atoi(hm[0]), atoi(hm[1])
			break
		}
	}
	if day == 0 || mon == 0 || year == 0 {
		return time.Time{}
	}
	return time.Date(year, time.Month(mon), day, hh, mm, 0, 0, mskLocation)
}

func isUnread(inboxHTML, itemID string) bool {
	for _, m := range parseInbox(inboxHTML) {
		if decoded, err := decodeUID(m.UID); err == nil && decoded == itemID {
			return !m.Seen
		}
	}
	return false
}

func isNotFoundPage(page string) bool {
	low := strings.ToLower(page)
	return strings.Contains(low, "не удается найти") ||
		strings.Contains(low, "элемент был перемещен или удален") ||
		strings.Contains(low, "itemnotfound")
}
