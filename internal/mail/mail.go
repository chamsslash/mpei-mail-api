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

// Source — источник писем.
//
// uid здесь строка, а не число: в IMAP это UID, но в EWS ItemId — base64.
// Если зашить IMAP-овский тип, вторая реализация сломает контракт ручек
// и потребителя ручек придётся переписывать.
type Source interface {
	List(ctx context.Context, q ListQuery) ([]Message, error)
	Get(ctx context.Context, mailbox, uid string) (*MessageFull, error)
	MarkSeen(ctx context.Context, mailbox, uid string) error
	Ping(ctx context.Context) error
}
