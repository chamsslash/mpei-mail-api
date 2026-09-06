package mail

import "errors"

// Сентинелы, по которым httpapi выбирает HTTP-код. Реализации Source обязаны
// заворачивать в них ошибки протокола: наружу не должен утекать текст ошибки
// библиотеки, в котором может оказаться строка подключения с логином.
var (
	ErrNotFound = errors.New("письмо не найдено")
	// ErrMailboxNotFound отделён от ErrNotFound намеренно: «нет такой папки» —
	// ошибка запроса, её чинит потребитель, а «нет такого письма» — обычный
	// исход обхода ящика.
	ErrMailboxNotFound     = errors.New("папка не найдена")
	ErrUpstreamUnavailable = errors.New("почтовый сервер недоступен")
	ErrUpstreamTimeout     = errors.New("почтовый сервер не ответил вовремя")
)
