#!/usr/bin/env bash
# Выкатывает игру на Linux-сервер (systemd) по SSH и открывает её всем.
#
#   deploy/deploy.sh root@194.87.118.85              игра на порту 80: http://194.87.118.85
#   PORT=8080 deploy/deploy.sh root@host             другой порт
#   SKIP_SRC=chgk deploy/deploy.sh root@host         без вопросов ЧГК
#
# Пароль (или ключ) спрашивает сам ssh — один раз, дальше соединение
# переиспользуется. Нигде в файлах пароль не хранится. Повторный запуск —
# обновление: бинарник заменяется, сервис перезапускается.
set -euo pipefail

TARGET=${1:?запуск: deploy/deploy.sh root@хост}
PORT=${PORT:-80}
SKIP_SRC=${SKIP_SRC:-}
HOST=${TARGET#*@}
URL="http://$HOST"
[ "$PORT" = 80 ] || URL="$URL:$PORT"

cd "$(dirname "$0")/.."
work=$(mktemp -d)
sock=/tmp/triviagame-ssh-$$
trap 'ssh -o ControlPath="$sock" -O exit "$TARGET" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
ssh_opts=(-o ControlMaster=auto -o ControlPath="$sock" -o ControlPersist=300 -o ServerAliveInterval=15 -o NumberOfPasswordPrompts=2)

# Без терминала (например, при запуске из чата через «!») ssh не может спросить
# пароль сам — на macOS тогда пароль вводится в обычном окне. Скрипт его не видит:
# окно отдаёт ответ напрямую ssh.
if [ ! -t 0 ] && [ -z "${SSH_ASKPASS:-}" ]; then
  if command -v osascript >/dev/null; then
    cat > "$work/askpass" <<'ASKPASS'
#!/bin/sh
exec osascript \
  -e 'on run argv' \
  -e 'text returned of (display dialog (item 1 of argv) default answer "" with hidden answer buttons {"OK"} default button "OK" with title "Борьба умов: выкатка")' \
  -e 'end run' "$1"
ASKPASS
    chmod +x "$work/askpass"
    export SSH_ASKPASS="$work/askpass" SSH_ASKPASS_REQUIRE=force
  else
    echo "Нет терминала для ввода пароля — запустите скрипт в обычном терминале." >&2
    exit 1
  fi
fi

echo "→ подключаюсь к $TARGET (пароль спросят один раз)"
arch=$(ssh "${ssh_opts[@]}" "$TARGET" 'uname -m; [ "$(id -u)" = 0 ] || echo NOT_ROOT; command -v systemctl >/dev/null || echo NO_SYSTEMD')
case "$arch" in *NOT_ROOT*) echo "нужен root на сервере" >&2; exit 1 ;; esac
case "$arch" in *NO_SYSTEMD*) echo "на сервере нет systemd — нужен ручной запуск (см. README)" >&2; exit 1 ;; esac
case "$(head -n1 <<<"$arch")" in
  x86_64) goarch=amd64 ;;
  aarch64 | arm64) goarch=arm64 ;;
  *) echo "неизвестная архитектура сервера: $arch" >&2; exit 1 ;;
esac

echo "→ собираю linux/$goarch"
CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath -ldflags='-s -w' -o "$work/triviagame" .

echo "→ загружаю бинарник"
scp -q -o ControlPath="$sock" "$work/triviagame" "$TARGET:/tmp/triviagame.new"

echo "→ устанавливаю и запускаю"
ssh "${ssh_opts[@]}" "$TARGET" bash -s -- "$PORT" "$URL" "$SKIP_SRC" <<'REMOTE'
set -euo pipefail
PORT=$1 URL=$2 SKIP_SRC=${3:-} # ssh склеивает аргументы в строку — пустой третий пропадает
DIR=/opt/triviagame

# порт не должен быть занят чем-то другим (нашей прошлой версией — можно)
if command -v ss >/dev/null; then
  busy=$(ss -ltnpH "sport = :$PORT" || true)
  if [ -n "$busy" ] && [[ $busy != *'"triviagame"'* ]]; then
    echo "Порт $PORT уже занят:" >&2
    echo "$busy" >&2
    echo "Запустите с другим портом: PORT=8080 deploy/deploy.sh …" >&2
    exit 1
  fi
fi

id triviagame >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin triviagame
install -d -m 755 "$DIR"
install -m 755 /tmp/triviagame.new "$DIR/triviagame"
rm -f /tmp/triviagame.new

args="-addr :$PORT -public-url $URL"
[ -z "$SKIP_SRC" ] || args="$args -skip-src $SKIP_SRC"
cat > /etc/systemd/system/triviagame.service <<UNIT
[Unit]
Description=Борьба умов (викторина на двоих)
After=network-online.target
Wants=network-online.target

[Service]
User=triviagame
ExecStart=$DIR/triviagame $args
Restart=always
RestartSec=2
LimitNOFILE=65536
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true

[Install]
WantedBy=multi-user.target
UNIT

# файрвол самой машины (если он включён); файрвол в панели хостинга — отдельно
if command -v ufw >/dev/null && [[ $(ufw status) == "Status: active"* ]]; then
  ufw allow "$PORT/tcp" >/dev/null
  echo "ufw: порт $PORT/tcp открыт"
elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
  firewall-cmd --permanent --add-port="$PORT/tcp" >/dev/null && firewall-cmd --reload >/dev/null
  echo "firewalld: порт $PORT/tcp открыт"
fi

systemctl daemon-reload
systemctl enable triviagame >/dev/null 2>&1
systemctl restart triviagame
sleep 1
if ! systemctl is-active --quiet triviagame; then
  journalctl -u triviagame -n 30 --no-pager >&2
  exit 1
fi
if command -v curl >/dev/null; then
  if ! curl -fsS --max-time 5 "http://127.0.0.1:$PORT/api/info" >/dev/null; then
    echo "Сервис запущен, но не отвечает на порту $PORT:" >&2
    journalctl -u triviagame -n 30 --no-pager >&2
    exit 1
  fi
  echo "на сервере игра отвечает"
fi
REMOTE

echo "→ проверяю доступность снаружи: $URL"
if curl -fsS --max-time 10 "$URL/api/info" >/dev/null; then
  echo "✓ Готово. Игра открыта всем: $URL"
else
  echo "✗ Игра запущена, но снаружи $URL не отвечает." >&2
  echo "  Вероятно, порт $PORT закрыт файрволом в панели хостинга (Security Group / «Сетевая безопасность»)." >&2
  exit 1
fi
