#!/bin/sh
# 跨机器压测 · 服务端机器：启动选定的 echo 服务和压测代理（bench/agent），直到 Ctrl-C。
# 再在客户端机器上运行 remote.sh。需要 Go 和 Linux；绑核需要 taskset（util-linux）。
#
#   sh bench/scripts/serve.sh
#   SCPU=2 SERVERS="epoll gnet uringnet-best uringnet-best-zc" sh bench/scripts/serve.sh
#   sh bench/scripts/serve.sh stop      # 结束上一次留下的服务端和代理
#
# 通过 SSH 运行时建议用 ssh -t（或在 tmux/screen 里）：没有终端时断开连接不会通知到脚本，
# 服务端会留在后台。下次启动时脚本会先结束上一次的进程（记录在 $PIDFILE），也可以随时 stop。
#
# 环境变量：
#   LISTEN     监听地址，默认 0.0.0.0
#   BASE_PORT  第一个服务的端口，默认 9100，之后依次 +1
#   AGENT_PORT 代理端口，默认 9099
#   SCPU       服务端绑定的 CPU（taskset -c 的格式），默认 0：单核，让服务端成为瓶颈。
#              建议避开处理网卡中断的核（见 /proc/interrupts）
#   SPROCS     服务端的 GOMAXPROCS，默认 1（多核时设为核数）
#   SERVERS    默认 "stdcopy epoll gnet uringnet uringnet-best uringnet-best-zc"
#
# 测零拷贝（uringnet-*-zc）时，注册缓冲区计入 RLIMIT_MEMLOCK：普通用户可能需要提高 ulimit -l，
# 否则会自动退回不注册的零拷贝或普通发送（结果仍然正确，只是测不到最佳效果）。
set -e
cd "$(dirname "$0")/.."
. scripts/servers.sh
LISTEN=${LISTEN:-0.0.0.0}; BASE_PORT=${BASE_PORT:-9100}; AGENT_PORT=${AGENT_PORT:-9099}
SCPU=${SCPU:-0}; SPROCS=${SPROCS:-1}
SERVERS=${SERVERS:-"stdcopy epoll gnet uringnet uringnet-best uringnet-best-zc"}
BIN=${BIN:-/tmp/bench-bin}
PIDFILE=${PIDFILE:-/tmp/bench-serve.pids}
export GOFLAGS=${GOFLAGS:--buildvcs=false}

# 结束上一次启动的服务端：残留的进程会和新进程共用端口（SO_REUSEPORT），悄悄分走连接，让结果失真
stop_old() {
  [ -f "$PIDFILE" ] || return 0
  for p in $(cat "$PIDFILE"); do kill "$p" 2>/dev/null && echo "stopped leftover process $p"; done
  rm -f "$PIDFILE"
  sleep 1
}
stop_old
[ "$1" = stop ] && exit 0

ulimit -n 65536 2>/dev/null || echo "warning: could not raise the open-file limit (ulimit -n)" >&2
build_servers "$BIN"

PIN=""
if command -v taskset >/dev/null 2>&1; then PIN="taskset -c $SCPU"; else echo "warning: taskset not found, servers are not pinned" >&2; fi

pids=""; agent_args=""; port=$BASE_PORT
for name in $SERVERS; do
  cmd=$(server_cmd "$name" "$BIN")
  GOMAXPROCS=$SPROCS $PIN $cmd "$LISTEN:$port" >/tmp/bench-$name.log 2>&1 &
  pids="$pids $!"; agent_args="$agent_args $name=$!:$port"
  echo "$name listening on $LISTEN:$port (pid $!)"
  port=$((port + 1))
done
"$BIN/agent" -addr ":$AGENT_PORT" $agent_args >/tmp/bench-agent.log 2>&1 &
pids="$pids $!"
echo $pids > "$PIDFILE"
# SSH 会话断开时收到的是 HUP：同样要结束子进程，否则服务端会留在后台占着端口
trap 'kill $pids 2>/dev/null; rm -f "$PIDFILE"' EXIT
trap 'kill $pids 2>/dev/null; rm -f "$PIDFILE"; exit 1' INT TERM HUP
sleep 1
echo "agent on :$AGENT_PORT — now run on the client machine:"
echo "  SERVER=<this machine's IP> AGENT_PORT=$AGENT_PORT sh bench/scripts/remote.sh > results-remote.txt"
echo "press Ctrl-C to stop"
wait
