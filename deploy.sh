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
envval() {
  local line
  line="$(grep -E "^$1=" .env | head -1 || true)"
  printf '%s' "${line#*=}"
}

# Кредов в .env.example достаточно для запуска, поэтому первый прогон не
# требует ручной правки: копируем и идём дальше.
if [[ ! -f .env ]]; then
  cp .env.example .env
  echo "==> .env не было — создал из .env.example"
fi

# Токен генерируем сами: заставлять человека придумывать его руками незачем,
# а пустой API_TOKEN уронил бы сервис (он намеренно не стартует без токена).
if [[ -z "$(envval API_TOKEN)" ]]; then
  tok="$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  [[ -n "$tok" ]] || die "не удалось сгенерировать API_TOKEN (нет ни openssl, ни /dev/urandom)"
  if grep -qE '^API_TOKEN=' .env; then
    sed -i.bak "s|^API_TOKEN=.*|API_TOKEN=${tok}|" .env && rm -f .env.bak
  else
    printf 'API_TOKEN=%s\n' "$tok" >> .env
  fi
  echo "==> API_TOKEN не был задан, сгенерировал: ${tok}"
fi

# Пустые обязательные переменные ловим здесь: иначе контейнер молча уйдёт в
# рестарт-петлю, а причина будет видна только в логах.
for v in MPEI_USER MPEI_PASS API_TOKEN; do
  grep -qE "^${v}=" .env || die "в .env нет переменной ${v}"
  [[ -n "$(envval "$v")" ]] || die "в .env не заполнена переменная ${v}"
done

PORT="$(envval HOST_PORT)"; PORT="${PORT:-8080}"

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
