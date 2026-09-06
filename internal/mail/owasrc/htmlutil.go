package owasrc

import (
	"mime"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// walk обходит дерево в глубину, вызывая f на каждом узле.
func walk(n *html.Node, f func(*html.Node)) {
	f(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, f)
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, cls string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == cls {
			return true
		}
	}
	return false
}

// ancestor поднимается по родителям до элемента с указанным тегом.
func ancestor(n *html.Node, tag string) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == tag {
			return p
		}
	}
	return nil
}

func findFirst(n *html.Node, pred func(*html.Node) bool) *html.Node {
	var found *html.Node
	walk(n, func(x *html.Node) {
		if found == nil && x.Type == html.ElementNode && pred(x) {
			found = x
		}
	})
	return found
}

func firstByClass(root *html.Node, cls string) string {
	if n := findFirst(root, func(x *html.Node) bool { return hasClass(x, cls) }); n != nil {
		return text(n)
	}
	return ""
}

// nextElement возвращает следующий братский узел-элемент, пропуская текст
// и пробелы между тегами.
func nextElement(n *html.Node) *html.Node {
	for s := n.NextSibling; s != nil; s = s.NextSibling {
		if s.Type == html.ElementNode {
			return s
		}
	}
	return nil
}

// text собирает весь текстовый контент поддерева.
func text(n *html.Node) string {
	var b strings.Builder
	walk(n, func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
	})
	return b.String()
}

// renderInner сериализует содержимое узла обратно в HTML — без самого узла.
// Нужен, чтобы отдать тело письма в поле html: обёртку OWA (td/div.bdy)
// потребителю знать незачем, а разметку письма — нужно.
func renderInner(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		// meta внутри тела — служебные вставки OWA (charset, viewport),
		// к содержимому письма они не относятся.
		if c.Type == html.ElementNode && (c.Data == "meta" || c.Data == "script" || c.Data == "style") {
			continue
		}
		if err := html.Render(&b, c); err != nil {
			return b.String()
		}
	}
	return strings.TrimSpace(b.String())
}

// blockTags — элементы, границы которых в тексте значат перевод строки.
// Без них абзацы и строки таблиц слипаются в одну строку, и перечисление
// вида «02.09.2026 – 6 выплат / 01.10.2026 – 4 выплаты» становится
// неразличимым месивом.
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "tr": true, "li": true,
	"table": true, "blockquote": true, "pre": true, "hr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

var skipTags = map[string]bool{
	"script": true, "style": true, "head": true, "meta": true, "title": true,
}

// blockText собирает текст поддерева, сохраняя разбиение на строки.
func blockText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(x *html.Node) {
		if x.Type == html.ElementNode {
			if skipTags[x.Data] {
				return
			}
			if blockTags[x.Data] {
				b.WriteByte('\n')
			}
		}
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
		if x.Type == html.ElementNode && blockTags[x.Data] && x.Data != "br" {
			b.WriteByte('\n')
		}
	}
	rec(n)
	return b.String()
}

var invisibleReplacer = strings.NewReplacer(
	"\u00a0", " ",
	"\u200b", "",
	"\u200e", "",
	"\u200f", "",
	"\ufeff", "",
)

// cleanText схлопывает пробелы, убирает невидимые метки, которыми OWA
// разбавляет разметку (nbsp, zero-width, LRM), и доразбирает числовые
// HTML-сущности: часть текста (единица размера вложения) приходит двойным
// кодированием и после одного разбора остаётся как "&#1050;&#1041;".
func cleanText(s string) string {
	s = decodeNumericEntities(s)
	s = invisibleReplacer.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// cleanMultiline приводит в порядок текст, у которого разбиение на строки
// значимо: пробелы схлопываются внутри строки, но сами переводы строк
// сохраняются, а пустые серии сжимаются до одной.
func cleanMultiline(s string) string {
	s = decodeNumericEntities(s)
	s = invisibleReplacer.Replace(s)

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln == "" {
			if blank > 0 || len(out) == 0 {
				continue
			}
			blank++
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// singleLine схлопывает многострочный текст в одну строку — для сниппета,
// который потребитель показывает одной строчкой.
func singleLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// extraMIME — типы, которых нет во встроенной таблице Go. Полагаться на
// системный /etc/mime.types нельзя: рабочий образ distroless, там его нет,
// и определение типа молча деградировало бы до octet-stream.
var extraMIME = map[string]string{
	".txt":  "text/plain",
	".csv":  "text/csv",
	".rtf":  "application/rtf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt":  "application/vnd.oasis.opendocument.text",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
	".zip":  "application/zip",
	".rar":  "application/vnd.rar",
	".7z":   "application/x-7z-compressed",
	".gz":   "application/gzip",
}

// mimeByName определяет тип вложения по расширению имени: сам OWA MIME не
// печатает, в разметке есть только имя файла и иконка типа.
func mimeByName(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		return "application/octet-stream"
	}
	if t, ok := extraMIME[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		if mt, _, err := mime.ParseMediaType(t); err == nil {
			return mt
		}
		return t
	}
	return "application/octet-stream"
}

var numEntityRe = regexp.MustCompile(`&#(x?[0-9a-fA-F]+);`)

func decodeNumericEntities(s string) string {
	if !strings.Contains(s, "&#") {
		return s
	}
	return numEntityRe.ReplaceAllStringFunc(s, func(m string) string {
		body := m[2 : len(m)-1]
		base := 10
		if len(body) > 0 && (body[0] == 'x' || body[0] == 'X') {
			base, body = 16, body[1:]
		}
		n, err := strconv.ParseInt(body, base, 32)
		if err != nil {
			return m
		}
		return string(rune(n))
	})
}

func looksLikeDate(s string) bool { return listDate.MatchString(strings.TrimSpace(s)) }

func looksLikeSize(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasSuffix(s, "КБ") || strings.HasSuffix(s, "МБ") ||
		strings.HasSuffix(s, "Б") || strings.HasSuffix(s, "байт")
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func scaleSize(digits, unit string) int64 {
	n, _ := strconv.ParseInt(digits, 10, 64)
	switch {
	case strings.Contains(unit, "МБ"):
		return n * 1024 * 1024
	case strings.Contains(unit, "КБ"):
		return n * 1024
	default:
		return n
	}
}

// truncateRunes режет строку по символам, добавляя многоточие.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
