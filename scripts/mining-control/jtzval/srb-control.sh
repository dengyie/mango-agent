#!/bin/sh
# jtzval 挖矿管控（hub 下发 {action}=start|stop 调用）。幂等：重复执行同向指令安全。
MINER="/opt/srbminer-364/SRBMiner-MULTI"
ARGS="--algorithm-cpu xelishashv3 --pool stratum+tcp://104.208.65.233:7019 --wallet krxXGNKMD4/vps-cloudstudio-1 --cpu-threads 1 --api-enable --api-port 21551 --log-file /workspace/srb-xel.log --log-file-level 1 --no-color"
LOCK="/workspace/.srb-control.lock"

exec 9>"$LOCK" || exit 1
flock -n 9 || { echo "another control action is running"; exit 1; }

case "$1" in
  stop)
    if ! pgrep -f "$MINER" >/dev/null 2>&1; then
      echo "already stopped"; exit 0
    fi
    pkill -f "cpulimit -l 95 -- $MINER" 2>/dev/null
    pkill -f "$MINER" 2>/dev/null
    sleep 2
    if pgrep -f "$MINER" >/dev/null 2>&1; then
      pkill -9 -f "$MINER"; sleep 1
    fi
    pgrep -f "$MINER" >/dev/null 2>&1 && { echo "failed to stop"; exit 1; }
    echo "stopped"
    ;;
  start)
    if pgrep -f "$MINER" >/dev/null 2>&1; then
      echo "already running"; exit 0
    fi
    # 2026-09-19 热修：spawn 必须关闭锁 fd(9>&-)，否则矿机继承 flock，
    # 脚本退出后孤儿矿机永久持锁，后续所有管控指令报 "another control action is running"。
    # 2026-09-19 定版：必须用 cpulimit -l 95 包装启动（定制版 SRBMiner 的既定限速形态）。
    # 实测定案：cpulimit 链 = 2 进程、~600 H/s 稳定；裸/nice 启动会触发矿机自包装出
    # 第二套 cpulimit+worker，双实例争抢 1 核配额，API 口被管理进程绑走后上报仅 ~65 H/s。
    setsid nohup cpulimit -l 95 -- "$MINER" $ARGS >/dev/null 2>&1 9>&- &
    sleep 3
    pgrep -f "$MINER" >/dev/null 2>&1 || { echo "failed to start"; exit 1; }
    echo "started"
    ;;
  *)
    echo "usage: $0 {start|stop}"; exit 1
    ;;
esac
