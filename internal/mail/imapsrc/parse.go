package imapsrc

import (
	"bytes"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"unicode"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"

	"github.com/gyattalert/mpei-mail-api/internal/mail"
)

func toAddress(a imap.Address) mail.Address {
	return mail.Address{Name: a.Name, Email: a.Addr()}
}

func toAddresses(in []imap.Address) []mail.Address {
	out := make([]mail.Address, 0, len(in))
	for _, a := range in {
		if a.IsGroupStart() || a.IsGroupEnd() {
			continue
		}
		out = append(out, toAddress(a))
	}
	return out
}

func hasFlag(flags []imap.Flag, want imap.Flag) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// buildMessage собирает элемент списка. Envelope библиотека уже отдаёт в UTF-8,
// поэтому декодировать тему повторно не нужно.
func buildMessage(buf *imapclient.FetchMessageBuffer, section *imap.FetchItemBodySection) mail.Message {
	m := mail.Message{
		UID:  strconv.FormatUint(uint64(buf.UID), 10),
		Seen: hasFlag(buf.Flags, imap.FlagSeen),
	}

	if e := buf.Envelope; e != nil {
		m.Subject = e.Subject
		m.Date = e.Date
		m.To = toAddresses(e.To)
		if from := toAddresses(e.From); len(from) > 0 {
			m.From = from[0]
		}
	}

	if buf.BodyStructure != nil {
		m.HasAttachments = len(collectAttachments(buf.BodyStructure)) > 0
	}

	m.Snippet = snippetFrom(buf.FindBodySection(section))

	return m
}

func buildMessageFull(buf *imapclient.FetchMessageBuffer, section *imap.FetchItemBodySection) *mail.MessageFull {
	full := &mail.MessageFull{
		Message:     buildMessage(buf, section),
		Attachments: []mail.Attachment{},
	}

	if e := buf.Envelope; e != nil {
		full.CC = toAddresses(e.Cc)
	}
	if buf.BodyStructure != nil {
		full.Attachments = collectAttachments(buf.BodyStructure)
	}

	raw := buf.FindBodySection(section)
	full.Text, full.HTML = parseBodies(raw)

	// Для полного письма сниппет считаем по уже разобранному тексту:
	// он точнее, чем догадка по сырому префиксу.
	if full.Text != "" {
		full.Snippet = truncateRunes(collapseSpaces(full.Text), snippetRunes)
	} else if full.HTML != "" {
		full.Snippet = truncateRunes(collapseSpaces(stripTags(full.HTML)), snippetRunes)
	}

	return full
}

// collectAttachments обходит структуру письма и собирает части с именем файла
// либо с disposition attachment. Части без имени (тело письма, инлайновые
// альтернативы) вложениями не считаются.
func collectAttachments(bs imap.BodyStructure) []mail.Attachment {
	out := []mail.Attachment{}

	bs.Walk(func(_ []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}

		name := sp.Filename()
		disp := sp.Disposition()
		isAttachment := name != "" || (disp != nil && strings.EqualFold(disp.Value, "attachment"))
		if !isAttachment {
			return true
		}

		out = append(out, mail.Attachment{
			Name: name,
			MIME: strings.ToLower(sp.MediaType()),
			Size: int64(sp.Size),
		})
		return true
	})

	return out
}

// parseBodies разбирает сырое письмо и достаёт text/plain и text/html.
// go-message сам декодирует transfer-encoding и кодировку части, что и
// закрывает windows-1251 и KOI8-R у российских отправителей.
func parseBodies(raw []byte) (text, html string) {
	if len(raw) == 0 {
		return "", ""
	}

	// Ошибка неизвестной кодировки не фатальна: reader всё равно пригоден,
	// просто часть тела останется недекодированной.
	r, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil && !gomessage.IsUnknownCharset(err) {
		slog.Debug("не удалось разобрать письмо как MIME", "err", err)
		return string(raw), ""
	}

	for {
		part, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if gomessage.IsUnknownCharset(err) {
				continue
			}
			slog.Debug("не удалось прочитать часть письма", "err", err)
			break
		}

		h, ok := part.Header.(*gomail.InlineHeader)
		if !ok {
			continue // вложение: содержимое не отдаём, только метаданные
		}

		body, err := io.ReadAll(part.Body)
		if err != nil {
			continue
		}

		mediaType, _, _ := h.ContentType()
		switch {
		case strings.EqualFold(mediaType, "text/plain") && text == "":
			text = string(body)
		case strings.EqualFold(mediaType, "text/html") && html == "":
			html = string(body)
		}
	}

	return text, html
}

// snippetFrom строит сниппет из префикса письма. Префикс обрывается на
// произвольном байте, поэтому MIME-разбор может не сойтись — тогда
// откатываемся на грубую очистку сырого текста.
func snippetFrom(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}

	if text, html := parseBodies(raw); text != "" {
		return truncateRunes(collapseSpaces(text), snippetRunes)
	} else if html != "" {
		return truncateRunes(collapseSpaces(stripTags(html)), snippetRunes)
	}

	return truncateRunes(collapseSpaces(stripHeaders(string(raw))), snippetRunes)
}

// stripHeaders отбрасывает всё до первой пустой строки — заголовки письма
// в сниппете не нужны.
func stripHeaders(s string) string {
	for _, sep := range []string{"\r\n\r\n", "\n\n"} {
		if _, body, found := strings.Cut(s, sep); found {
			return body
		}
	}
	return s
}

func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func collapseSpaces(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// truncateRunes режет по символам, а не байтам: иначе кириллица обрывается
// на середине руны.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return strings.TrimSpace(string(runes[:n])) + "…"
}
