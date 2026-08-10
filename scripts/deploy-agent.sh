#!/bin/bash
# komari-agent 原子部署(Bohrium 无 systemd,走 watchdog pidfile 换 bin 重启)
# 用法: deploy-agent.sh <host-dir> [new-bin-path]
#   <host-dir>     例 /personal/beszel/tebi 或 /personal/beszel/pxed
#   [new-bin-path] 缺省 /tmp/komari-agent.new(经 scp 上传的新二进制)
set -uo pipefail
DIR="${1:?host-dir required, e.g. /personal/beszel/tebi}"
NEW="${2:-/tmp/komari-agent.new}"
BIN="/usr/local/bin/komari-agent"
LOOP="$DIR/komari-agent-loop.sh"
PIDF="$DIR/komari-agent-loop.pid"
LOGF="$DIR/komari-agent-loop.log"
TS(){ date "+%Y-%m-%d %H:%M:%S"; }
log(){ echo "[$(TS)] $*" >> "$LOGF"; }

[ -f "$NEW" ]  || { echo "FATAL: missing new binary $NEW" >&2; exit 2; }
[ -f "$LOOP" ] || { echo "FATAL: missing watchdog $LOOP" >&2; exit 3; }
[ "$(head -c 4 "$NEW" 2>/dev/null | od -An -tx1 | tr -d ' \n')" = "7f454c46" ] || { echo "FATAL: $NEW not an ELF" >&2; exit 4; }
chmod +x "$NEW"
"$NEW" --help >/dev/null 2>&1 || { echo "FATAL: new binary smoke test failed" >&2; exit 5; }

# 1. 停旧 watchdog(其 TERM trap 会杀 agent + 删 pidfile);再保险清残留 agent
if [ -f "$PIDF" ]; then
  OLD=$(cat "$PIDF" 2>/dev/null || true)
  if [ -n "${OLD:-}" ] && kill -0 "$OLD" 2>/dev/null; then
    log "deploy: stopping watchdog pid=$OLD"
    kill -TERM "$OLD" 2>/dev/null || true
    for _ in $(seq 1 30); do kill -0 "$OLD" 2>/dev/null || break; sleep 1; done
    kill -9 "$OLD" 2>/dev/null || true
  fi
  rm -f "$PIDF"
fi
if pgrep -f '^/usr/local/bin/komari-agent$' >/dev/null; then
  log "deploy: killing stray agent"
  pkill -9 -f '^/usr/local/bin/komari-agent$'
  sleep 1
fi

# 2. 换 bin(留 .old 回滚)
[ -f "$BIN" ] && mv "$BIN" "$BIN.old"
mv "$NEW" "$BIN"
chmod +x "$BIN"

# 3. 重启 watchdog(setsid 脱离 SSH,断开不杀)
setsid nohup "$LOOP" "$DIR" >/dev/null 2>&1 &
sleep 3
NEWPID=$(cat "$PIDF" 2>/dev/null || true)
if [ -n "${NEWPID:-}" ] && kill -0 "$NEWPID" 2>/dev/null; then
  log "deploy: OK bin=$BIN watchdog=$NEWPID"
  echo "DEPLOY_OK watchdog=$NEWPID"
else
  log "deploy: FAIL watchdog did not start"
  echo "FATAL: watchdog did not start" >&2
  exit 6
fi
