#!/usr/bin/env python3
"""supervisord 轻量控制（无 supervisorctl 二进制时经 Unix socket XML-RPC 操作）。
用法: supctl.py {start|stop|status} <program>"""
import os, socket, http.client, xmlrpc.client, sys

SOCK = os.environ.get("SUP_SOCKET", "/workspace/run/supervisor.sock")
RUNNING = 20

class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost")
        self._path = path
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(self._path)

class UnixTransport(xmlrpc.client.Transport):
    def __init__(self, path):
        super().__init__()
        self._path = path
    def make_connection(self, host):
        return UnixHTTPConnection(self._path)

def main():
    action, prog = sys.argv[1], sys.argv[2]
    try:
        srv = xmlrpc.client.ServerProxy("http://x/RPC2", transport=UnixTransport(SOCK))
        info = srv.supervisor.getProcessInfo(prog)
    except Exception as e:
        # 任务结果里只留一行根因，不刷 traceback
        print(f"error: supervisord rpc failed: {e}")
        sys.exit(1)
    state, pid = info["statename"], info["pid"]
    if action == "status":
        print(state)
        sys.exit(0 if info["state"] == RUNNING else 1)
    if action == "stop":
        if info["state"] in (0, 10, 40):  # STOPPED/STOPPING/FATAL
            return
        srv.supervisor.stopProcess(prog)
    elif action == "start":
        if info["state"] == RUNNING:
            return
        srv.supervisor.startProcess(prog)
    else:
        sys.exit(f"unknown action {action}")

if __name__ == "__main__":
    main()
