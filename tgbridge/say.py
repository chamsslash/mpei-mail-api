#!/usr/bin/env python3
"""Отправка сообщения в Telegram одной командой.

    python3 tgbridge/say.py "короткий ответ"
    python3 tgbridge/say.py < ответ.txt

Нужен, чтобы рутине не приходилось собирать JSON внутри shell-строки: кавычки
и переводы строк в тексте ответа ломают такую сборку тихо и по-разному.
Берёт TELEGRAM_BOT_TOKEN и TELEGRAM_CHAT_ID из окружения.
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request

LIMIT = 4096


def split_text(text, limit=LIMIT):
    """Режем по границам строк; строку длиннее лимита — жёстко."""
    parts, cur = [], ""
    for line in text.splitlines(keepends=True):
        while len(line) > limit:
            if cur:
                parts.append(cur)
                cur = ""
            parts.append(line[:limit])
            line = line[limit:]
        if len(cur) + len(line) > limit:
            parts.append(cur)
            cur = ""
        cur += line
    if cur.strip():
        parts.append(cur)
    return parts


def send(text):
    url = (f"https://api.telegram.org/bot{os.environ['TELEGRAM_BOT_TOKEN']}"
           f"/sendMessage")
    payload = json.dumps({"chat_id": int(os.environ["TELEGRAM_CHAT_ID"]),
                          "text": text,
                          "disable_web_page_preview": True}).encode()
    req = urllib.request.Request(url, data=payload,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            body = json.loads(r.read())
    except urllib.error.HTTPError as e:
        body = json.loads(e.read() or b"{}")
    if not body.get("ok"):
        sys.exit(f"telegram: {body.get('description', body)}")


def main():
    text = " ".join(sys.argv[1:]).strip() or sys.stdin.read().strip()
    if not text:
        sys.exit("нечего отправлять")
    for i, part in enumerate(split_text(text)):
        if i:
            time.sleep(1)
        send(part)
    print("отправлено")


if __name__ == "__main__":
    main()
