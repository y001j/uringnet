#!/bin/sh
# 在 Docker 中运行压测（macOS 上也可用）。用法：
#   bench/docker.sh                        # 默认 pingpong 模式
#   MODE=onecore DEPTHS="1 8" bench/docker.sh
# 结果写入 bench/results-<mode>.txt，并打印汇总。
# seccomp=unconfined 是必须的：Docker 默认的 seccomp 策略会拦截 io_uring 系统调用。
set -e
cd "$(dirname "$0")/.."
MODE=${MODE:-pingpong}
OUT=bench/results-$MODE.txt
docker run --rm --security-opt seccomp=unconfined --ulimit nofile=65536:65536 --ulimit memlock=-1 \
  -e GOFLAGS=-buildvcs=false -e MODE -e CONNS -e SIZES -e DEPTHS -e ROUNDS -e DUR -e SERVERS \
  -v "$PWD":/src -v uringnet-gomod:/go/pkg/mod -w /src/bench \
  golang:1.22-bookworm sh -c 'go mod tidy >/dev/null 2>&1; sh scripts/run.sh' | tee "$OUT"
python3 bench/scripts/summarize.py "$OUT"
