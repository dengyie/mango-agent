#!/bin/bash
# jtzval 挖矿管控（hub 下发 {action}=start|stop 调用）。幂等：重复执行同向指令安全。
# 2026-09-19 定版：矿机由平台 supervisord 托管（/usr/local/share/supervisor/srb-xel.conf，
# program=srb-xel, autorestart=true），直接 pkill 会被 6 秒内拉回并与 supervisor 重启赛跑
# 产生双实例（各半核，面板上报失真一半）——启停必须走 supervisord XML-RPC。
export SUP_SOCKET=/.PlnPyKFp4CRfFtgC1_run/cs-supervisor.sock
SUPY=/workspace/tools/supctl.py
PROG=srb-xel
MINER=/opt/srbminer-364/SRBMiner-MULTI
LOCK=/workspace/.srb-control.lock

exec 9>"$LOCK" || exit 1
flock -n 9 || { echo "another control action is running"; exit 1; }

miner_alive(){ pgrep -f "$MINER" >/dev/null 2>&1; }

case "$1" in
  stop)
    if ! miner_alive && ! python3 "$SUPY" status "$PROG" >/dev/null 2>&1; then
      echo "already stopped"; exit 0
    fi
    python3 "$SUPY" stop "$PROG" >/dev/null 2>&1
    pkill -f "$MINER" 2>/dev/null; sleep 1
    if miner_alive; then echo "failed to stop miner"; exit 1; fi
    echo "stopped"
    ;;
  start)
    if miner_alive; then echo "already running"; exit 0; fi
    python3 "$SUPY" start "$PROG" || { echo "failed to start via supervisord"; exit 1; }
    echo "started"
    ;;
  *)
    echo "usage: $0 {start|stop}"; exit 1
    ;;
esac
