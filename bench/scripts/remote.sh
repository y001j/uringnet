#!/bin/sh
# 跨机器压测 · 客户端机器：连到运行 serve.sh 的服务端机器，跑完整个测试矩阵。
# 输出格式与 run.sh 相同，可用 summarize.py 汇总。需要 Go、curl、awk，Linux 或 macOS 均可。
#
#   SERVER=10.0.0.2 sh bench/scripts/remote.sh | tee results-remote.txt
#   python3 bench/scripts/summarize.py results-remote.txt epoll
#
# 环境变量：
#   SERVER      服务端机器的地址（必填）
#   AGENT_PORT  代理端口，默认 9099
#   SERVERS     只测其中几个（默认测服务端启动的全部）
#   CONNS       连接数列表，默认 "1 16 64 512"
#   SIZES       请求大小列表，默认 "128 16384 65536"
#   DEPTHS      每连接在途请求数列表，默认 "1 8"
#   ROUNDS      轮数（结果取中位数），默认 3
#   DUR         每次测试时长，默认 5s
#   CCPU        客户端绑定的 CPU（taskset 格式，可选；macOS 无 taskset 时忽略）
#   CPROCS      客户端的 GOMAXPROCS（可选）
#   TIMEOUT     单次往返的超时，默认 30s；服务端卡住时该连接停止，测试照常结束
set -e
[ -n "$SERVER" ] || { echo "usage: SERVER=<server address> sh bench/scripts/remote.sh" >&2; exit 2; }
cd "$(dirname "$0")/.."
AGENT_PORT=${AGENT_PORT:-9099}; CONNS=${CONNS:-"1 16 64 512"}; SIZES=${SIZES:-"128 16384 65536"}
DEPTHS=${DEPTHS:-"1 8"}; ROUNDS=${ROUNDS:-3}; DUR=${DUR:-5s}; TIMEOUT=${TIMEOUT:-30s}
BIN=${BIN:-/tmp/bench-client-bin}
AGENT="http://$SERVER:$AGENT_PORT"

ulimit -n 65536 2>/dev/null || echo "warning: could not raise the open-file limit (ulimit -n)" >&2
mkdir -p "$BIN"
GOFLAGS=${GOFLAGS:--buildvcs=false} go build -o "$BIN/client" ./client

PIN=""
if [ -n "$CCPU" ] && command -v taskset >/dev/null 2>&1; then PIN="taskset -c $CCPU"; fi
ENVS=""
[ -n "$CPROCS" ] && ENVS="GOMAXPROCS=$CPROCS"

LIST=$(curl -fsS "$AGENT/servers") || { echo "cannot reach the agent at $AGENT (is serve.sh running, port open?)" >&2; exit 1; }
if [ -n "$SERVERS" ]; then
  LIST=$(echo "$LIST" | awk -v want=" $SERVERS " 'index(want, " " $1 " ")')
fi
[ -n "$LIST" ] || { echo "no matching servers" >&2; exit 1; }

echo "# mode=remote server=$SERVER $(curl -fsS "$AGENT/info") client=$(uname -sr)"
for r in $(seq "$ROUNDS"); do for c in $CONNS; do for p in $DEPTHS; do for s in $SIZES; do
  echo "$LIST" | while read -r name port; do
    a=$(curl -fsS "$AGENT/cpu?name=$name")
    out=$(env $ENVS $PIN "$BIN/client" -addr "$SERVER:$port" -c "$c" -p "$p" -s "$s" -d "$DUR" -timeout "$TIMEOUT")
    b=$(curl -fsS "$AGENT/cpu?name=$name")
    cpu=$(echo "$a $b" | awk '{ dt = $4 - $2; v = 0; if (dt > 0) v = ($3 - $1) / dt * 100; printf "%.0f", v }')
    echo "$name $out servercpu=$cpu%"
  done
done; done; done; done
