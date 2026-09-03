# mpei-mail-api

HTTP-обёртка над почтой МЭИ (`mail.mpei.ru`). Сервис ходит в ящик по IMAP и
отдаёт письма в JSON, чтобы шедулерная задача дёргала одну ручку вместо того,
чтобы самой разбираться с IMAP и MIME.

## Быстрый старт

```bash
go build -o mpei-mail-api ./cmd/mpei-mail-api

MPEI_USER='ваш_логин' \
MPEI_PASS='ваш_пароль' \
API_TOKEN="$(openssl rand -hex 32)" \
./mpei-mail-api
```

Сервис поднимется на `127.0.0.1:8080`. Без `API_TOKEN` он **намеренно не
стартует**: тихий дефолт означал бы ручку с доступом к почте, открытую без
авторизации.

## Настройки

| Переменная | Дефолт | Обязательна |
|---|---|---|
| `MPEI_USER` | — | да |
| `MPEI_PASS` | — | да |
| `API_TOKEN` | — | да |
| `LISTEN_ADDR` | `127.0.0.1:8080` | нет |
| `MAIL_SOURCE` | `imap` | нет |
| `IMAP_ADDR` | `mail.mpei.ru:993` | нет |
| `REQUEST_TIMEOUT` | `30s` | нет |

## Ручки

Все ручки, кроме `/healthz`, требуют заголовок `Authorization: Bearer <API_TOKEN>`.

### Список писем

```bash
curl -H "Authorization: Bearer $API_TOKEN" \
  'http://localhost:8080/messages?limit=10&unseen=true'
```

Параметры: `limit` (по умолчанию 20, максимум 100), `unseen=true`,
`since` (`2026-09-01` или RFC3339), `mailbox` (по умолчанию `INBOX`).

```json
{
  "messages": [
    {
      "uid": "12345",
      "from": {"name": "Иванов И.И.", "email": "ivanov@mpei.ru"},
      "to": [{"name": "", "email": "student@mpei.ru"}],
      "subject": "Расписание пересдач",
      "date": "2026-09-03T14:21:00+03:00",
      "seen": false,
      "has_attachments": true,
      "snippet": "Уважаемые студенты, пересдача по..."
    }
  ],
  "count": 1
}
```

Свежие письма идут первыми. Поле `snippet` — первые 300 символов текста; оно
существует, чтобы в большинстве случаев решать «интересно или нет» **по одному
запросу**, не ходя за телом каждого письма.

### Одно письмо целиком

```bash
curl -H "Authorization: Bearer $API_TOKEN" http://localhost:8080/messages/12345
```

Добавляет к полям списка `cc`, `text`, `html` и `attachments` (имя, MIME,
размер — содержимое файлов не отдаётся).

**Эта ручка не помечает письмо прочитанным.** Чтение идёт через `BODY.PEEK`,
чтобы автоматика не меняла незаметно состояние живого ящика.

### Пометить прочитанным

```bash
curl -X POST -H "Authorization: Bearer $API_TOKEN" \
  http://localhost:8080/messages/12345/seen
```

Отвечает `204`. Идемпотентна.

### Health

```bash
curl http://localhost:8080/healthz
# {"status":"ok","source":"imap","upstream":"ok"}
```

Отвечает `200`, пока жив процесс; доступность МЭИ вынесена в поле `upstream`.
Код ответа намеренно не завязан на почтовый сервер: иначе k8s-liveness
перезапускал бы под из-за чужой недоступной почты, а рестарт её не чинит.
Результат проверки апстрима кэшируется на 30 секунд.

## Ошибки

Единый конверт `{"error":{"code":"...","message":"..."}}`.

| Код | HTTP | Что делать шедулеру |
|---|---|---|
| `unauthorized` | 401 | починить токен |
| `bad_request` | 400 | починить параметры запроса |
| `message_not_found` | 404 | пропустить письмо |
| `upstream_unavailable` | 502 | **не ретраить** — почтовый сервер недоступен или IMAP выключен |
| `upstream_timeout` | 504 | ретраить, сервер не успел ответить |
| `internal` | 500 | смотреть логи сервиса |

Разделение 502 и 504 сделано именно ради потребителя: повторять запрос
осмысленно только во втором случае.

## Docker

```bash
docker build -t mpei-mail-api .
docker run --rm -p 8080:8080 \
  -e MPEI_USER=... -e MPEI_PASS=... -e API_TOKEN=... \
  mpei-mail-api
```

## Доступность порта 993

При разработке обнаружено, что с некоторых сетей TCP-коннект на
`mail.mpei.ru:993` проходит, но TLS-handshake немедленно обрывается — при том
что `443` у того же хоста работает нормально. Различить фильтрацию и
выключенный администраторами IMAP снаружи нельзя.

Проверить доступность из конкретной среды:

```bash
MPEI_USER=... MPEI_PASS=... go test -tags integration ./internal/mail/imapsrc/ -v
```

Если `Ping` падает, а `/healthz` показывает `"upstream":"fail"` — порт 993 из
этой среды недоступен. `mail.mpei.ru` — это Exchange, и у него по 443 живы
`/EWS/Exchange.asmx` и `/Microsoft-Server-ActiveSync` с Basic-аутентификацией,
поэтому запасной путь — вторая реализация `mail.Source` поверх EWS. Интерфейс
и контракт ручек под неё уже готовы: `uid` объявлен строкой именно потому, что
в EWS `ItemId` — base64, а не число.

## Разработка

```bash
go test ./...                                        # быстрые тесты, без сети
go test -tags integration ./internal/mail/imapsrc/   # против живого сервера
go build ./... && go vet ./...
```

Документы: спека — `docs/superpowers/specs/`, план реализации —
`docs/superpowers/plans/`.
