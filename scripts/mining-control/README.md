# 中心端挖矿管控 · 节点侧脚本

hub（mango-hub）通过 `agent.mining.control` 下发 `start|stop`，agent 将 action 替换进
`AGENT_MINER_CONTROL_CMD` 模板（含 `{action}` 占位符）执行本目录脚本。部署位置与
各节点实况见 Obsidian《mango-agent 挖矿上报与多机部署优化》§8.2/§8.3.1。

## 部署位置

| 节点 | 脚本 | 部署路径 | agent 模板（AGENT_MINER_CONTROL_CMD） |
|---|---|---|---|
| jtzval（CloudStudio-IDE） | `jtzval/srb-control.sh` + `jtzval/supctl.py` | `/workspace/srb-control.sh` + `/workspace/tools/supctl.py` | `/workspace/srb-control.sh {action}` |
| csnet（CloudStudio-net-IDE） | `csnet/srb-control.sh` + `csnet/supctl.py` | `/workspace/srb-control.sh` + `/workspace/tools/supctl.py` | `/workspace/srb-control.sh {action}` |

其他节点不经脚本：xinyun=`systemctl {action} srb-xel`；home-win/laptop-win=config.json
`miner_control_cmd`（home-win 用 `nssm {action} srbminer`；laptop 的 nssm 不在 PATH，
必须用全路径 `\"D:\\program\\nssm\\nssm.exe\" {action} srbminer`）。

## 关键约束（踩坑沉淀，改动前必读）

1. **spawn 子进程必须关闭锁 fd**（jtzval 脚本 `9>&-`）：矿机继承 flock 后，脚本退出
   即遗留孤儿持锁者，后续所有管控指令报 "another control action is running"。
   与中枢文档 §17.4 看门狗 `200>&-` 教训同型。
2. **csnet 与 jtzval 的矿机/agent 都由 supervisord 托管**：csnet=`/workspace/tools/supervisord.conf`
   （program `srb-watchdog` / `komari-probe`，socket `/workspace/run/supervisor.sock`）；
   jtzval=`/usr/local/share/supervisor/srb-xel.conf` + `komari-probe.conf`
   （program `srb-xel` / `komari-probe`，socket `/.PlnPyKFp4CRfFtgC1_run/cs-supervisor.sock`，
   用 `SUP_SOCKET` 环境变量切换）。启停必须走 `supctl.py`（Unix socket XML-RPC，
   节点无 supervisorctl 二进制）；直接 pkill 会被 autorestart **6 秒内拉回并与脚本 start
   赛跑产生双实例**（各半核、API 被其中一条绑走，面板上报腰斩——2026-09-19 实测事故）。
3. **supervisord.conf 需含**（根治 stop 后孤儿 `sleep` 继承 flock fd 导致的重启抖动）：

   ```ini
   [program:srb-watchdog]
   command=/bin/bash /workspace/.host/bin/srb-watchdog
   directory=/workspace
   autostart=true
   stopasgroup=true
   killasgroup=true
   autorestart=true
   startsecs=10
   startretries=10
   stdout_logfile=/workspace/logs/srb-watchdog.log
   stderr_logfile=/workspace/logs/srb-watchdog-error.log
   ```

   `[program:komari-probe]`（agent）的 command 不得含 `--disable-web-ssh`，并加
   `environment=AGENT_MINER_CONTROL_CMD="/workspace/srb-control.sh {action}"`。
4. `/workspace` 与 `/workspace/tools` 为 root 属主：部署用 `sudo tee`；容器重建后需按
   上表重新落脚本并核对 supervisord.conf（jtzval 的 agent/miner 均由 supervisord
   `komari-probe`/`srb-xel` 程序托管 + autostart，重建后自动拉起；`komari-probe.conf`
   的 command 内含 `AGENT_MINER_API_URL`、environment 行含 `AGENT_MINER_CONTROL_CMD`，
   缺 MINER_API_URL 会静默丢挖矿上报）。
5. 验收标准：每节点连续两轮 `stop→stop→start→start` 全部 rc=0，且 start 后再次调用
   能正常拿锁（证明无 fd 泄漏）；**重拉 agent 后用 `common:getNodesLatestStatus` 确认
   mining 字段恢复**（本次 §8.3.2 热修时曾因丢 MINER_API_URL 静默且回报）。
6. **改 supervisord program 的 command/environment 后**：XML-RPC `reloadConfig` 对已存在
   program 的修改不生效（返回空 diff）——必须 `stopProcessGroup → removeProcessGroup →
   addProcessGroup → startProcess` 才能应用（jtzval komari-probe 实测）。
