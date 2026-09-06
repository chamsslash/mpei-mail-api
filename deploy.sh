#!/usr/bin/env bash
# Разворачивает mpei-mail-api на сервере: обновляет код, пересобирает образ,
# перезапускает контейнер и убеждается, что сервис действительно ответил.
#
# Запускать на самом сервере, из каталога репозитория:
#   ./deploy.sh              — git pull + пересборка + перезапуск
#   ./deploy.sh --no-pull    — то же, но без git pull (деплой локальных правок)
set -euo pipefail

cd "$(dirname "$0")"

PULL=1
[[ "${1:-}" == "--no-pull" ]] && PULL=0

die() { echo "ОШИБКА: $*" >&2; exit 1; }

command -v docker >/dev/null || die "docker не установлен"

# Compose бывает плагином (v2, «docker compose») и отдельным бинарём (v1,
# «docker-compose»). Берём что есть, чтобы скрипт не падал на старом сервере.
if docker compose version >/dev/null 2>&1; then
  DC=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  DC=(docker-compose)
else
  die "не найден docker compose (ни плагин v2, ни docker-compose v1)"
fi

# --- конфиг ---------------------------------------------------------------
if [[ ! -f .env ]]; then
  cp .env.example .env
  die ".env не было — создал из .env.example. Заполни MPEI_USER, MPEI_PASS и API_TOKEN и запусти снова.
Токен: openssl rand -hex 32"
fi

# Пустые обязательные переменные ловим здесь: иначе контейнер молча уйдёт в
# рестарт-петлю, а причина будет видна только в логах.
for v in MPEI_USER MPEI_PASS API_TOKEN; do
  line="$(grep -E "^${v}=" .env || true)"
  [[ -n "$line" ]] || die "в .env нет переменной ${v}"
  [[ -n "${line#*=}" ]] || die "в .env не заполнена переменная ${v}"
done

PORT="$(grep -E '^HOST_PORT=' .env | cut -d= -f2- || true)"; PORT="${PORT:-8080}"

# --- код ------------------------------------------------------------------
if [[ $PULL -eq 1 ]]; then
  if [[ -d .git ]]; then
    echo "==> git pull"
    git pull --ff-only
  else
    echo "==> не git-репозиторий, шаг обновления пропущен"
  fi
fi

# --- сборка и запуск ------------------------------------------------------
echo "==> сборка образа и перезапуск"
"${DC[@]}" up -d --build

# --- проверка -------------------------------------------------------------
# Пока контейнер не ответит на /healthz, деплой не считается удавшимся:
# «docker compose up прошёл» и «сервис работает» — разные события.
echo -n "==> ждём /healthz "
for _ in $(seq 1 30); do
  if out="$(curl -fsS --max-time 3 "http://127.0.0.1:${PORT}/healthz" 2>/dev/null)"; then
    echo
    echo "$out"
    case "$out" in
      *'"upstream":"ok"'*) echo "==> готово: сервис поднят, почта МЭИ отвечает" ;;
      *) echo "==> сервис поднят, но почта МЭИ недоступна — проверь MPEI_USER/MPEI_PASS в .env" ;;
    esac
    exit 0
  fi
  echo -n "."
  sleep 2
done

echo
echo "СЕРВИС НЕ ОТВЕТИЛ за 60 секунд. Последние логи:" >&2
"${DC[@]}" logs --tail=50 api >&2
exit 1
