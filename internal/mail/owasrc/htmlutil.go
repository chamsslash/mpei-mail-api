package owasrc

import (
	"net/url"
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

// cleanText схлопывает пробелы, убирает невидимые метки, которыми OWA
// разбавляет разметку (nbsp, zero-width, LRM), и доразбирает числовые
// HTML-сущности: часть текста (единица размера вложения) приходит двойным
// кодированием и после одного разбора остаётся как "&#1050;&#1041;".
func cleanText(s string) string {
	s = decodeNumericEntities(s)
	s = strings.NewReplacer(
		"\u00a0", " ",
		"\u200b", "",
		"\u200e", "",
		"\u200f", "",
		"\ufeff", "",
	).Replace(s)
	return strings.Join(strings.Fields(s), " ")
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

func urlUnescape(s string) (string, error) { return url.QueryUnescape(s) }

// truncateRunes режет строку по символам, добавляя многоточие.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
