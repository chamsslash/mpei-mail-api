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
for v in MPEI_USER MPEI_PASS API_TOKEN SITE_ADDR; do
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
# Место проверяем заранее: при нехватке go build падает не сразу, а через
# минуту компиляции, и в логе это выглядит десятком чужих ошибок компилятора
# вместо внятного «диск кончился».
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)"
# Каталог существует не всегда: на macOS докер живёт в своей ВМ, и проверять
# место на хосте бессмысленно — тогда шаг просто пропускается.
AVAIL_MB=""
[[ -d "${DOCKER_ROOT:-}" ]] && AVAIL_MB="$(df -Pm "$DOCKER_ROOT" 2>/dev/null | awk 'NR==2{print $4}')"
if [[ -n "$AVAIL_MB" && "$AVAIL_MB" -lt 2000 ]]; then
  echo "==> ВНИМАНИЕ: на ${DOCKER_ROOT:-/} свободно ${AVAIL_MB} МБ, сборке нужно ~1.5 ГБ."
  echo "    Освободить: docker system prune -af   (удалит неиспользуемые образы и кэш сборки)"
fi

echo "==> сборка образа и перезапуск"
if ! "${DC[@]}" up -d --build; then
  echo >&2
  echo "Сборка или запуск не удались. Если выше есть «no space left on device» —" >&2
  echo "это кончился диск: docker system prune -af, затем ./deploy.sh снова." >&2
  exit 1
fi

# --- проверка -------------------------------------------------------------
# Пока контейнер не ответит на /healthz, деплой не считается удавшимся:
# «compose up прошёл» и «сервис работает» — разные события.
echo -n "==> ждём /healthz "
ok=""
out=""
for _ in $(seq 1 30); do
  if out="$(curl -fsS --max-time 3 "http://127.0.0.1:${PORT}/healthz" 2>/dev/null)"; then
    ok=1
    break
  fi
  echo -n "."
  sleep 2
done
echo

if [[ -z "$ok" ]]; then
  echo "СЕРВИС НЕ ОТВЕТИЛ за 60 секунд. Последние логи:" >&2
  "${DC[@]}" logs --tail=50 api >&2
  exit 1
fi

echo "$out"
case "$out" in
  *'"upstream":"ok"'*) echo "==> сервис поднят, почта МЭИ отвечает" ;;
  *) echo "==> сервис поднят, но почта МЭИ недоступна — проверь MPEI_USER/MPEI_PASS в .env" ;;
esac

# --- TLS ------------------------------------------------------------------
# Смотрим не «поднялся ли Caddy», а добыл ли он сертификат и у кого именно:
# при неудачной выдаче Caddy остаётся жив и продолжает пытаться, а снаружи это
# выглядит как рабочий сервис с непринимаемым сертификатом.
SITE="$(envval SITE_ADDR)"
issuer="$("${DC[@]}" exec -T caddy sh -c 'ls /data/caddy/certificates 2>/dev/null' 2>/dev/null | head -1 || true)"
case "$issuer" in
  *acme-v02.api.letsencrypt.org*)
    echo "==> TLS: сертификат Let's Encrypt получен — адрес доверенный" ;;
  *acme-staging*)
    echo "==> TLS: сертификат из ТЕСТОВОГО контура Let's Encrypt, клиенты ему не поверят" ;;
  *local*)
    echo "==> TLS: самоподписанный (внутренний CA Caddy) — потребителю нужен --insecure" ;;
  "")
    echo "==> TLS: сертификата ещё нет. Выдача занимает до минуты; если не появится —"
    echo "    проверь, что порты 80 и 443 открыты в фаерволе, и смотри логи:"
    echo "    ${DC[*]} logs --tail=30 caddy" ;;
  *)
    echo "==> TLS: сертификат от ${issuer}" ;;
esac

echo "==> ручки: https://${SITE}/messages  (заголовок Authorization: Bearer <API_TOKEN>)"
