#!/bin/sh
# 在 Linux（容器）内运行的 echo 压测。由 bench/docker.sh 调用，也可在真机上直接运行（需 taskset）。
#
# 环境变量：
#   MODE    pingpong：服务端 4 核(CPU 0-3)/客户端 4 核(CPU 4-7)（默认）
#           onecore ：服务端 1 核(CPU 0)/客户端 8 核(CPU 4-11)，让服务端成为瓶颈
#   CONNS   连接数列表，默认 "1 64 512"
#   SIZES   请求大小列表，默认 "128 16384"
#   DEPTHS  每连接在途请求数列表（pipelining），默认 "1"
#   ROUNDS  轮数（结果取中位数），默认 3
#   DUR     每次测试时长，默认 3s
#   SERVERS 参与测试的服务端，默认全部：stdcopy std epoll gnet rawloop uringnet uringnet-spin uringnet-zc uringnet-inc
set -e
MODE=${MODE:-pingpong}; CONNS=${CONNS:-"1 64 512"}; SIZES=${SIZES:-"128 16384"}; DEPTHS=${DEPTHS:-1}
ROUNDS=${ROUNDS:-3}; DUR=${DUR:-3s}
SERVERS=${SERVERS:-"stdcopy std epoll gnet rawloop uringnet uringnet-spin uringnet-zc uringnet-inc"}
BIN=/tmp/bench-bin
cd "$(dirname "$0")/.."
mkdir -p $BIN
go build -o $BIN/client ./client
for s in std stdcopy epoll gnet rawloop uringnet; do go build -o $BIN/$s ./servers/$s; done

if [ "$MODE" = onecore ]; then
  SCPU=0; SPROCS=1; CCPU=4-11; CPROCS=8
else
  SCPU=0-3; SPROCS=4; CCPU=4-7; CPROCS=4
fi

port=9100
pids=""
for name in $SERVERS; do
  port=$((port+1))
  case $name in
    uringnet)      cmd="$BIN/uringnet" ;;
    uringnet-spin) cmd="$BIN/uringnet -spin 50us" ;;
    uringnet-zc)   cmd="$BIN/uringnet -zc 16384" ;;
    uringnet-inc)  cmd="$BIN/uringnet -inc -bufs 32 -bufsize 65536" ;;
    rawloop)       cmd="env BUFS=256 $BIN/rawloop" ;;
    *)             cmd="$BIN/$name" ;;
  esac
  GOMAXPROCS=$SPROCS taskset -c $SCPU $cmd 127.0.0.1:$port >/dev/null 2>&1 &
  eval "PID_$(echo $name | tr - _)=$!; PORT_$(echo $name | tr - _)=$port"
  pids="$pids $!"
done
trap 'kill $pids 2>/dev/null' EXIT
sleep 2

ticks() { awk '{print $14+$15}' /proc/$1/stat; }
echo "# mode=$MODE kernel=$(uname -r) cpus=$(nproc)"
for r in $(seq $ROUNDS); do for c in $CONNS; do for p in $DEPTHS; do for s in $SIZES; do
  for name in $SERVERS; do
    v=$(echo $name | tr - _); eval "pid=\$PID_$v; port=\$PORT_$v"
    t0=$(ticks $pid); s0=$(date +%s.%N)
    out=$(GOMAXPROCS=$CPROCS taskset -c $CCPU $BIN/client -addr 127.0.0.1:$port -c $c -p $p -s $s -d $DUR)
    t1=$(ticks $pid); s1=$(date +%s.%N)
    echo "$name $out servercpu=$(awk -v a=$t0 -v b=$t1 -v x=$s0 -v y=$s1 'BEGIN{printf "%.0f", (b-a)/(y-x)}')%"
  done
done; done; done; done
