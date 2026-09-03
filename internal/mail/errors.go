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
