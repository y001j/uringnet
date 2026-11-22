# Echo 压测工具

用同一个客户端，对比多种 echo 服务端实现：

| 服务端 | 实现 |
|---|---|
| `stdcopy` | Go 标准库 `net`，显式 `Read`/`Write`（数据进出用户态）——**公平基线** |
| `std` | Go 标准库 `io.Copy(c, c)`：在 Linux 上走 splice，数据不进用户态，不代表需要处理数据的服务 |
| `epoll` | 手写 epoll 事件循环，每核一个绑定线程 + SO_REUSEPORT |
| `gnet` | [gnet](https://github.com/panjf2000/gnet)（epoll 框架） |
| `rawloop` | 直接用 iouring-go `Loop` 的最简实现，用来衡量 uringnet 封装本身的开销 |
| `uringnet` | 本项目，默认配置 |
| `uringnet-spin` | 本项目，`SpinTime = 50µs` |
| `uringnet-zc` | 本项目，`ZeroCopyThreshold = 16KB` |
| `uringnet-inc` | 本项目，增量消费 buffer ring（实验性，Linux 6.12+），32 × 64KB |
| `uringnet-best` | 本项目推荐配置：增量缓冲区 32 × 64KB + 自适应轮询 50µs |
| `uringnet-best-zc` | 推荐配置 + 零拷贝发送（`ZeroCopyThreshold = 16KB`），用于在真实网卡上测零拷贝 |

服务端的启动参数统一定义在 `scripts/servers.sh`，本机压测和跨机器压测共用。

`servers/uringnet` 支持参数：`-bufs`（接收缓冲区数量）、`-bufsize`、`-spin`（如 `50us`，默认自适应）、
`-spinfixed`（总是轮询）、`-spinbusy` / `-spinidle`（自适应阈值）、`-zc`（零拷贝阈值）、
`-bundle auto|on|off`（recv bundle；`on` 在 6.16 之前的内核上可能导致数据错乱，只用于评估性能）、
`-inc`（增量消费 buffer ring，Linux 6.12+，建议配合 `-bufs 32 -bufsize 65536`）。

客户端（`client/`）：c 个长连接，每个连接循环「写 p 个 s 字节的请求 → 读回全部响应」，
输出 QPS、客户端 CPU 和往返延迟 p50/p99；脚本另外记录服务端 CPU。

## 运行

```shell
bench/docker.sh                                   # pingpong：服务端 4 核 / 客户端 4 核
MODE=onecore DEPTHS="1 8" bench/docker.sh         # 服务端 1 核 / 客户端 8 核，让服务端成为瓶颈
CONNS="64" SIZES="16384" ROUNDS=1 bench/docker.sh # 只跑部分场景
```

在 Linux 真机上可以直接运行 `MODE=onecore sh bench/scripts/run.sh > r.txt && python3 bench/scripts/summarize.py r.txt`
（需要 `taskset`，至少 12 个 CPU；io_uring 需内核 6.0+）。

## 跨机器压测

本机回环测不出零拷贝发送的效果（内核会把发往本机 socket 的数据拷贝一次），也测不到网卡和中断的影响。
要看真实表现，用两台 Linux 机器（客户端也可以是 macOS），走真实网卡：

```shell
# 服务端机器：编译并启动 echo 服务和压测代理（bench/agent），直到 Ctrl-C
ssh -t server-host 'cd uringnet && sh bench/scripts/serve.sh'

# 客户端机器：连到服务端，跑完整个测试矩阵
SERVER=10.0.0.2 sh bench/scripts/remote.sh | tee results-remote.txt
python3 bench/scripts/summarize.py results-remote.txt epoll
```

- **服务端**：`serve.sh` 默认启动 `stdcopy epoll gnet uringnet uringnet-best uringnet-best-zc`，监听 `0.0.0.0:9100` 起的端口，
  绑定到 CPU 0（单核，让服务端成为瓶颈）。可用 `SERVERS`、`SCPU`、`SPROCS`、`LISTEN`、`BASE_PORT`、`AGENT_PORT` 调整。
- **代理**：`bench/agent` 在 `:9099` 上告诉客户端每个服务的端口，以及每次测试期间服务端进程用掉的 CPU，所以不需要 SSH。
- **客户端**：`remote.sh` 默认测 `CONNS="1 16 64 512"`、`SIZES="128 16384 65536"`、`DEPTHS="1 8"`，每项 3 轮、每轮 5 秒。
  输出格式与本机压测相同；`SERVERS` 可以只挑几个服务端测。客户端带每次往返 30 秒的超时（`TIMEOUT`），服务端卡住时不会一直等。
- **清理**：没有终端的 SSH 断开时脚本收不到通知，服务端会留在后台。`serve.sh` 每次启动会先结束上一次的进程
  （记录在 `/tmp/bench-serve.pids`），也可以随时运行 `sh bench/scripts/serve.sh stop`。建议用 `ssh -t` 或在 tmux 里运行。

为了让结果可信：

- 两台机器直连或接在同一台交换机上，确认链路速率（`ethtool <网卡>`），MTU 两端一致；
- `SCPU` 避开处理网卡中断的 CPU（看 `/proc/interrupts`），客户端核数要足够，别让客户端成为瓶颈（看输出里的 `clientcpu`）；
- 零拷贝的注册缓冲区计入 `RLIMIT_MEMLOCK`：普通用户运行时如果 `ulimit -l` 太小，会自动退回较慢的方式，结果仍然正确但测不到最佳效果。
  需要时用 root 运行，或调大 `ulimit -l`；
- 比较 `uringnet-best` 和 `uringnet-best-zc`，重点看 16KB、64KB 的场景：零拷贝只对大块发送有帮助，小消息反而会略慢。

## 读结果时注意

- **一问一答（p=1）测的主要是往返延迟**：CPU 往往大部分空闲，吞吐 ≈ 连接数 / 往返延迟。
  在虚拟机里唤醒空闲 vCPU 很贵，延迟更大，`SpinTime` 在这种场景收益明显（代价是 CPU）。
- **服务端处理能力**要看 `MODE=onecore` 且服务端 CPU 接近 100% 的场景。
- **零拷贝发送在回环上没有收益**：发往本机 socket 的数据内核总会做一次延迟拷贝
  （见内核文档 msg_zerocopy），`uringnet-zc` 在这里只会更慢；要在真实网卡、跨机器测试。
- Docker Desktop 的结果波动很大（同一实现可差 30% 以上），以 3 轮中位数为准，最终请在目标机器上测试。
