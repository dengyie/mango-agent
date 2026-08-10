#!/bin/bash
# 经 cloudflared tunnel 部署 komari-agent 二进制到指定节点(tebi/pxed)
# 用法: remote-deploy.sh <node> <new-bin-path>
#   <node>         tebi | pxed
#   <new-bin-path> 新 linux/amd64 二进制在本机路径
# 认证:
#   有 TUNNEL_SERVICE_TOKEN_ID + TUNNEL_SERVICE_TOKEN_SECRET(CI secret)→ 用 service token
#   否则用本机 cloudflared 登录态(手动 Mac 调试)
# 密钥: BOHRIUM_SSH_KEY_FILE(缺省 ~/.ssh/bohrium_ed25519)
set -euo pipefail
NODE="${1:?node required: tebi|pxed}"
BIN="${2:?new binary path required}"
KEY="${BOHRIUM_SSH_KEY_FILE:-$HOME/.ssh/bohrium_ed25519}"

case "$NODE" in
  tebi) DIR=/personal/beszel/tebi ;;
  pxed) DIR=/personal/beszel/pxed ;;
  *) echo "unknown node $NODE (tebi|pxed)" >&2; exit 2 ;;
esac
HOST="ssh-${NODE}.mangoqwq.com"

[ -f "$BIN" ] || { echo "missing new binary $BIN" >&2; exit 3; }
[ -f "$KEY" ] || { echo "missing ssh key $KEY" >&2; exit 4; }

# 解析 cloudflared 路径(CI 装到 /usr/local/bin,Mac 在 homebrew)
CF=$(command -v cloudflared || echo /opt/homebrew/bin/cloudflared)
[ -x "$CF" ] || { echo "cloudflared not found" >&2; exit 5; }

PROXYCMD="'$CF' access ssh --hostname $HOST"
if [ -n "${TUNNEL_SERVICE_TOKEN_ID:-}" ] && [ -n "${TUNNEL_SERVICE_TOKEN_SECRET:-}" ]; then
  PROXYCMD="'$CF' access ssh --hostname $HOST --service-token-id '$TUNNEL_SERVICE_TOKEN_ID' --service-token-secret '$TUNNEL_SERVICE_TOKEN_SECRET'"
fi
SSH_OPTS=(-o "ProxyCommand=$PROXYCMD" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=30 -i "$KEY")

echo "==> deploy to ${NODE} (${HOST})"
# 1. 上传新二进制 + 部署脚本
scp -q "${SSH_OPTS[@]}" "$BIN" "root@${HOST}:/tmp/komari-agent.new"
scp -q "${SSH_OPTS[@]}" "$(dirname "$0")/deploy-agent.sh" "root@${HOST}:/tmp/deploy-agent.sh"
# 2. 远端执行换 bin + 重启 watchdog
ssh "${SSH_OPTS[@]}" "root@${HOST}" "bash /tmp/deploy-agent.sh '$DIR' /tmp/komari-agent.new"
echo "==> ${NODE} done"
