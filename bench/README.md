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

## 读结果时注意

- **一问一答（p=1）测的主要是往返延迟**：CPU 往往大部分空闲，吞吐 ≈ 连接数 / 往返延迟。
  在虚拟机里唤醒空闲 vCPU 很贵，延迟更大，`SpinTime` 在这种场景收益明显（代价是 CPU）。
- **服务端处理能力**要看 `MODE=onecore` 且服务端 CPU 接近 100% 的场景。
- **零拷贝发送在回环上没有收益**：发往本机 socket 的数据内核总会做一次延迟拷贝
  （见内核文档 msg_zerocopy），`uringnet-zc` 在这里只会更慢；要在真实网卡、跨机器测试。
- Docker Desktop 的结果波动很大（同一实现可差 30% 以上），以 3 轮中位数为准，最终请在目标机器上测试。
