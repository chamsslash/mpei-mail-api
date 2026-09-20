#!/usr/bin/env python3
"""Мост Telegram → облачная рутина.

Держит длинный опрос getUpdates и на каждое сообщение от разрешённого чата
дёргает API-триггер рутины. Рутина просыпается за секунды, читает команду и
работает коннекторами Gmail и Calendar.

Опрос живёт здесь, а не в рутине, потому что рутина не умеет запускаться чаще
раза в час. Здесь же лежит offset — его нужно помнить между сообщениями, а
окружение рутины каждый запуск свежее.

Только стандартная библиотека. Настройки — из окружения:
    TELEGRAM_BOT_TOKEN   токен бота
    TELEGRAM_CHAT_ID     единственный чат, чьи команды исполняются
    ROUTINE_URL          эндпоинт API-триггера рутины
    ROUTINE_TOKEN        bearer-токен этого триггера
    STATE_DIR            где хранить offset (по умолчанию рядом со скриптом)
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

POLL_TIMEOUT = 50      # длинный опрос: Telegram держит соединение до ответа
HTTP_TIMEOUT = POLL_TIMEOUT + 15
POST_ATTEMPTS = 3
BACKOFF = 5


def env(name, default=None):
    v = os.environ.get(name, default)
    if v is None:
        sys.exit(f"не задана переменная окружения {name}")
    return v


BOT = env("TELEGRAM_BOT_TOKEN")
CHAT_ID = int(env("TELEGRAM_CHAT_ID"))
ROUTINE_URL = env("ROUTINE_URL")
ROUTINE_TOKEN = env("ROUTINE_TOKEN")
STATE = Path(env("STATE_DIR", str(Path(__file__).resolve().parent))) / "offset.json"


def log(msg, **kv):
    tail = " ".join(f"{k}={v}" for k, v in kv.items())
    print(f"{time.strftime('%F %T')} {msg} {tail}".rstrip(), flush=True)


def post_json(url, payload, headers, timeout):
    req = urllib.request.Request(
        url, data=json.dumps(payload).encode(), method="POST",
        headers={"Content-Type": "application/json", **headers})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        body = r.read()
        return json.loads(body) if body else {}


def tg(method, params=None, timeout=HTTP_TIMEOUT):
    url = f"https://api.telegram.org/bot{BOT}/{method}"
    return post_json(url, params or {}, {}, timeout)


def tg_say(text):
    """Ответ пользователю. Молча глотаем сбой: это уведомление, не операция."""
    try:
        tg("sendMessage", {"chat_id": CHAT_ID, "text": text}, timeout=20)
    except Exception as e:
        log("не смог ответить в телегу", err=e)


def load_offset():
    try:
        return json.loads(STATE.read_text())["offset"]
    except (FileNotFoundError, json.JSONDecodeError, KeyError):
        return 0


def save_offset(value):
    tmp = STATE.with_suffix(".tmp")
    tmp.write_text(json.dumps({"offset": value}))
    tmp.replace(STATE)  # атомарно: при падении посреди записи файл не побьётся


def wake_routine(text):
    """Будим рутину. True — приняла, False — не достучались."""
    for attempt in range(1, POST_ATTEMPTS + 1):
        try:
            post_json(ROUTINE_URL, {"text": text},
                      {"Authorization": f"Bearer {ROUTINE_TOKEN}"}, timeout=60)
            return True
        except Exception as e:
            log("рутина не ответила", attempt=attempt, err=e)
            if attempt < POST_ATTEMPTS:
                time.sleep(BACKOFF * attempt)
    return False


def handle(update):
    msg = update.get("message") or update.get("edited_message")
    if not msg:
        return
    chat = (msg.get("chat") or {}).get("id")
    text = (msg.get("text") or "").strip()

    # Боту может написать кто угодно, кто его найдёт. Всё, что не из нашего
    # чата, игнорируем молча: ответ подтвердил бы, что бот живой.
    if chat != CHAT_ID:
        log("чужой чат, игнор", chat=chat)
        return
    if not text:
        tg_say("Пока понимаю только текст.")
        return

    log("команда", text=text[:80])
    if wake_routine(text):
        tg_say("Принял, работаю…")
    else:
        tg_say("Не смог разбудить рутину — попробуй ещё раз через минуту.")


def main():
    log("старт", chat=CHAT_ID, state=STATE)
    offset = load_offset()
    while True:
        try:
            resp = tg("getUpdates", {"offset": offset, "timeout": POLL_TIMEOUT,
                                     "allowed_updates": ["message"]})
        except urllib.error.HTTPError as e:
            # 409 — кто-то ещё опрашивает этого бота; лечится только остановкой
            # второго потребителя, поэтому просто ждём и пробуем снова.
            log("telegram ответил ошибкой", code=e.code)
            time.sleep(BACKOFF)
            continue
        except Exception as e:
            log("сеть недоступна", err=e)
            time.sleep(BACKOFF)
            continue

        for upd in resp.get("result", []):
            handle(upd)
            # Offset двигаем после обработки: падение посреди работы означает
            # повтор команды, а не её потерю.
            offset = upd["update_id"] + 1
            save_offset(offset)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        log("остановлен")
