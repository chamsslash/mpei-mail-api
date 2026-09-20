#!/usr/bin/env python3
"""Сводка почты МЭИ в Telegram — детерминированная часть шедулерной задачи.

Два шага, между ними модель пишет сводку:
    python3 mail_digest.py fetch            -> JSON с новыми письмами в stdout
    python3 mail_digest.py send summary.txt -> отправка в Telegram + пометка

Состояния на диске нет намеренно: скрипт рассчитан на облачную рутину, где
окружение каждый запуск свежее и файл рядом со скриптом не переживёт перезапуск.
Память о том, что уже отправлено, живёт в самой почте — отправленные письма
помечаются прочитанными, и на следующем запуске unseen=true их не вернёт.

Только стандартная библиотека. Секреты берутся из окружения.
"""
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path
from zoneinfo import ZoneInfo

HERE = Path(__file__).resolve().parent
PENDING = HERE / "pending.json"

# Облачный запуск живёт по UTC, а сводку читает человек в Москве: без явной
# зоны время в шапке отставало бы на три часа.
DEFAULT_TZ = "Europe/Moscow"

DEFAULT_BASE = "https://gyattalert-mpei.duckdns.org"
TIMEOUT = 180  # full=true тянет тела всех писем разом, это небыстро
BODY_LIMIT = 2000
TG_LIMIT = 4096

# Каждая пометка прочитанным — отдельный логин в OWA, а Exchange режет
# аккаунт при частых входах. Отсюда и потолок пачки, и пауза между пометками.
DEFAULT_LIMIT = 10
MARK_PAUSE = 2.0


# ---------- утилиты ----------

def load_env():
    # .env рядом со скриптом — только для локальной отладки; в рутине значения
    # приходят из окружения и имеют приоритет (setdefault не перетирает).
    env_file = HERE / ".env"
    if env_file.exists():
        for line in env_file.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if line and not line.startswith("#") and "=" in line:
                k, v = line.split("=", 1)
                os.environ.setdefault(k.strip(), v.strip())
    missing = [k for k in ("API_TOKEN", "TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID")
               if not os.environ.get(k)]
    if missing:
        die(f"нет переменных окружения: {', '.join(missing)}")


def die(msg, code=2):
    print(json.dumps({"status": "error", "message": msg}, ensure_ascii=False))
    sys.exit(code)


def read_json(path, default):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (FileNotFoundError, json.JSONDecodeError):
        return default


def write_json(path, data):
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(data, ensure_ascii=False, indent=1), encoding="utf-8")
    tmp.replace(path)  # атомарно: файл не побьётся при падении посреди записи


def now_local():
    try:
        return datetime.now(ZoneInfo(os.environ.get("DIGEST_TZ", DEFAULT_TZ)))
    except Exception:
        # Базы зон в образе может не оказаться (zoneinfo читает её из системы,
        # а в урезанных контейнерах /usr/share/zoneinfo пустой). Запасной
        # вариант — фиксированный +03:00: Москва живёт без перехода на летнее
        # время с 2014 года, так что для дефолта смещение постоянное.
        return datetime.now(timezone(timedelta(hours=3)))


def batch_limit():
    raw = os.environ.get("DIGEST_LIMIT", "")
    try:
        return max(1, min(int(raw), 50))  # 50 — потолок full=true в самом сервисе
    except ValueError:
        return DEFAULT_LIMIT


class ApiError(Exception):
    def __init__(self, status, code=""):
        super().__init__(f"{status} {code}")
        self.status, self.code = status, code


def api(path, params=None, auth=True, method="GET"):
    url = os.environ.get("MAIL_API_BASE", DEFAULT_BASE) + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, method=method)
    if auth:
        req.add_header("Authorization", f"Bearer {os.environ['API_TOKEN']}")
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            body = r.read()
            return json.loads(body) if body else {}
    except urllib.error.HTTPError as e:
        code = ""
        try:
            code = json.loads(e.read())["error"]["code"]
        except Exception:
            pass
        raise ApiError(e.code, code)
    except (urllib.error.URLError, TimeoutError) as e:
        raise ApiError(0, f"network: {e}")


def api_retry_504(path, params=None, method="GET"):
    """504 — временная заминка, один ретрай сразу. Остальное пробрасываем."""
    try:
        return api(path, params, method=method)
    except ApiError as e:
        if e.status == 504:
            return api(path, params, method=method)
        raise


def tg_send(text):
    url = (f"{os.environ.get('TG_API_BASE', 'https://api.telegram.org')}"
           f"/bot{os.environ['TELEGRAM_BOT_TOKEN']}/sendMessage")
    payload = json.dumps({"chat_id": int(os.environ["TELEGRAM_CHAT_ID"]),
                          "text": text,
                          "disable_web_page_preview": True}).encode()
    for _ in range(3):
        req = urllib.request.Request(url, data=payload,
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                body = json.loads(r.read())
        except urllib.error.HTTPError as e:
            body = json.loads(e.read() or b"{}")
        except (urllib.error.URLError, TimeoutError) as e:
            raise RuntimeError(f"telegram недоступен: {e}")
        if body.get("ok"):
            return
        if body.get("error_code") == 429:
            time.sleep(body.get("parameters", {}).get("retry_after", 5) + 1)
            continue
        raise RuntimeError(f"telegram: {body.get('description', body)}")
    raise RuntimeError("telegram: 429 не прошёл после ретраев")


def split_text(text, limit=TG_LIMIT):
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


def emit(obj):
    print(json.dumps(obj, ensure_ascii=False, indent=1))


# ---------- fetch ----------

def cmd_fetch():
    try:
        health = api("/healthz", auth=False)
        up = health.get("upstream") == "ok"
    except ApiError:
        up = False

    if not up:
        # Молча пропускаем: почта временно недоступна, письма никуда не делись
        # и уйдут следующим запуском. Неудачный прогон видно в истории рутины.
        emit({"status": "skip", "reason": "upstream недоступен"})
        return

    # full=true забирает тела всех писем в одной сессии OWA — это один логин
    # на пачку вместо логина на письмо.
    try:
        listing = api_retry_504("/messages", {
            "unseen": "true", "full": "true", "limit": batch_limit(),
        })
    except ApiError as e:
        emit({"status": "error", "reason": f"листинг: {e}"})
        return

    messages = []
    for full in listing.get("messages", []):
        text = (full.get("text") or "").strip()
        if len(text) > BODY_LIMIT:
            text = text[:BODY_LIMIT] + "\n[…обрезано]"
        messages.append({
            "uid": full["uid"],
            "from": (full.get("from") or {}).get("name", ""),
            "subject": full.get("subject", ""),
            "date": full.get("date", ""),
            "attachments": [a["name"] for a in full.get("attachments") or []],
            "text": text,
        })

    failed = listing.get("failed", 0)
    write_json(PENDING, {"uids": [m["uid"] for m in messages], "failed": failed})

    emit({"status": "new" if messages else "none",
          "count": len(messages), "failed": failed,
          "truncated": listing.get("truncated", False),
          "messages": messages})


# ---------- send ----------

def cmd_send(summary_path):
    pending = read_json(PENDING, None)
    if not pending or not pending.get("uids"):
        die("нет pending.json — сначала fetch с новыми письмами")

    summary = Path(summary_path).read_text(encoding="utf-8").strip()
    if not summary:
        die("сводка пустая — письма не помечены, уйдут в следующий раз")

    uids, failed = pending["uids"], pending.get("failed", 0)
    header = f"📬 Почта МЭИ · новых: {len(uids)} · {now_local():%d.%m %H:%M}\n\n"
    footer = f"\n\n❗ Не удалось прочитать писем: {failed}" if failed else ""

    try:
        for i, part in enumerate(split_text(header + summary + footer)):
            if i:
                time.sleep(1)
            tg_send(part)
    except RuntimeError as e:
        die(f"{e} — письма НЕ помечены, сводка уйдёт в следующий раз", code=1)

    # Сначала доставка, потом пометка. Порядок важен: непомеченное письмо
    # придёт повторно, а помеченное без доставки пропадёт навсегда.
    marked, unmarked = 0, []
    for i, uid in enumerate(uids):
        if i:
            time.sleep(MARK_PAUSE)
        try:
            api_retry_504(f"/messages/{uid}/seen", method="POST")
            marked += 1
        except ApiError:
            unmarked.append(uid)  # попадёт в следующую сводку повторно

    PENDING.unlink(missing_ok=True)
    emit({"status": "sent", "count": len(uids),
          "marked": marked, "unmarked": len(unmarked)})


if __name__ == "__main__":
    load_env()
    if len(sys.argv) >= 2 and sys.argv[1] == "fetch":
        cmd_fetch()
    elif len(sys.argv) == 3 and sys.argv[1] == "send":
        cmd_send(sys.argv[2])
    else:
        die("usage: mail_digest.py fetch | send <summary.txt>")
