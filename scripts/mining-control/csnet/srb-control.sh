#!/bin/bash
# cloudstudio-net 挖矿管控（hub 下发 {action}=start|stop 调用）。幂等：重复执行同向指令安全。
# v2 (2026-09-19): srb-watchdog 实际由 supervisord 托管（/workspace/tools/supervisord.conf，
#   autorestart=true），直接 pkill 会被自动拉活——启停必须走 supervisor XML-RPC；
#   停看门狗后仍需清理其孤儿 sleep 继承的 flock fd 与矿机进程。
SUPY="/workspace/tools/supctl.py"
MINER="/workspace/srbminer-364/SRBMiner-MULTI"
WDL="/workspace/run/srb-watchdog.lock"
LOCK="/workspace/run/srb-control.lock"

mkdir -p /workspace/run
exec 9>"$LOCK" || exit 1
flock -n 9 || { echo "another control action is running"; exit 1; }

miner_alive(){ pgrep -f "$MINER" >/dev/null 2>&1; }
wd_state(){ python3 "$SUPY" status srb-watchdog 2>/dev/null; }
lock_holders(){ for p in /proc/[0-9]*/fd; do for f in "$p"/*; do [ "$(readlink "$f" 2>/dev/null)" = "$WDL" ] && basename "$(dirname "$f")" && break; done; done; }

case "$1" in
  stop)
    if ! miner_alive && ! wd_state | grep -q RUNNING; then
      echo "already stopped"; exit 0
    fi
    python3 "$SUPY" stop srb-watchdog >/dev/null 2>&1
    for h in $(lock_holders); do kill -9 "$h" 2>/dev/null; done
    pkill -f "$MINER" 2>/dev/null; sleep 2
    pgrep -f "$MINER" >/dev/null 2>&1 && pkill -9 -f "$MINER"
    sleep 1
    if miner_alive; then echo "failed to stop miner"; exit 1; fi
    wd_state | grep -q RUNNING && { echo "failed to stop watchdog"; exit 1; }
    echo "stopped"
    ;;
  start)
    if wd_state | grep -q RUNNING; then echo "already running"; exit 0; fi
    for h in $(lock_holders); do kill -9 "$h" 2>/dev/null; done
    python3 "$SUPY" start srb-watchdog || { echo "failed to start watchdog"; exit 1; }
    echo "started (watchdog will knock + spawn miner)"
    ;;
  *)
    echo "usage: $0 {start|stop}"; exit 1
    ;;
esac
