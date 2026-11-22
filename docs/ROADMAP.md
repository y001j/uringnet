# UringNet 路线图

整理时间：2026-11-22，基于 master `2d4be7d`（核心已重写到 iouring-go 的 `Loop` API 上）。

目标：从实验性项目发展为可以在生产环境使用的网络组件。条目按优先级分组，前两组是生产使用的硬缺口。

**进度**（已完成的标记为 ✅）：
- 第一阶段（第 1、2、3、7 项）：2026-11-22 完成，包含在 `v0.1.0-alpha` 中。
- 第二阶段：第 4、5、8、9、10、11、19 项已完成（2026-11-22，分支 `feat/phase2-api`）；第 5 项中的 epoll 回退已完成（BackendAuto/BackendEpoll）。
- 第 20、21 项：随 v0.1.0 发布完成（iouring-go v0.1.0、x/sys v0.30.0、CHANGELOG）。
- 第 6 项：模糊测试和压力测试已加入；顺带发现并修复了默认 SQ 过大导致多核机器上锁定内存不足的问题。
- 第 12、13、15 项：拨出连接（Dial/DialAsync/NewClient）、UDP（ListenPacket）、分帧（Framed）已完成。
- 第 17 项：`Server.Stats()`；第 22 项：`example_test.go`、代码注释统一为英文、架构文档 `docs/ARCHITECTURE.md`。
- 第 16 项（零拷贝接收）不做：需要数据中心级专用网卡。
- 第 5 项的 epoll 回退、第 14 项 TLS（WithTLS / WithDialTLS）已完成。

优先级：**P0** 上生产前必须完成 · **P1** 发布 v0.1.0 前完成 · **P2** 按需求排期 · **P3** 长期目标

---

## 一、稳定性和安全

### 1. 发送队列上限（背压） · P0 ✅
- **现状**：每个连接的发送队列没有上限。对端一直不读取时，服务端会无限缓存数据，最终可能内存耗尽（README “已知问题”之一）。
- **要做的**：
  - 每个连接设置高低水位（可配置，例如默认 4MB / 1MB）。
  - 超过高水位时暂停该连接的接收（取消 multishot recv），降到低水位以下再恢复。
  - 提供回调或返回错误，让用户可以选择断开连接。
- **验收**：一个只写不读的客户端压测 10 分钟，服务端内存保持平稳。

### 2. accept 失败时的退避 · P0 ✅
- **现状**：`EMFILE`、`ENFILE` 等错误下，multishot accept 会立即重新提交，可能陷入空转、占满 CPU。
- **要做的**：失败后用 io_uring 定时器做指数退避（例如从 1ms 到最多 1s）再重新提交；通过日志或回调通知用户。
- **验收**：把 `ulimit -n` 调小后打满连接，服务端 CPU 不会被占满，连接释放后能自动恢复 accept。

### 3. 连接超时 · P0 ✅
- **现状**：没有空闲超时，也没有读写超时，慢连接和死连接会一直占着资源。
- **要做的**：
  - 空闲超时：一段时间没有收发数据就关闭连接。
  - 写超时：发送队列在规定时间内没有清空就关闭连接。
  - 在环内实现（`IORING_OP_TIMEOUT` 或时间轮），不额外开 goroutine。
- **验收**：单元测试覆盖两种超时；10 万个空闲连接时定时器的开销可以接受。

### 4. 优雅关闭 · P1 ✅
- **现状**：`Shutdown` 会立即关闭所有连接。
- **要做的**：先停止 accept，等在途的发送完成或超时，再关闭连接和环；提供 `Shutdown(ctx context.Context) error`。
- **验收**：关闭过程中已经发出的响应不会丢失。

### 5. 启动检查和回退 · P1 ✅（含 epoll 回退）
- **现状**：io_uring 被禁用（seccomp、`kernel.io_uring_disabled`）、`RLIMIT_MEMLOCK` 不足、内核版本太低时，只返回一个笼统的错误。
- **要做的**：
  - 启动时逐项检测，给出明确的报错和解决办法。
  - 可选：io_uring 不可用时回退到 epoll 实现（README “已知问题”之一）。

### 6. 模糊测试和长时间压力测试 · P1 ✅
- **现状**：只有 16 个单元测试。
- **要做的**：
  - 用 `go test -fuzz` 测试分包、半包、bundle、增量缓冲区等边界情况。
  - 长时间压测，加上随机断连、对端 RST、慢读等情况。
  - 测试结束后检查 fd、内存、接收缓冲区有没有泄漏。

---

## 二、API 整理（发布 v1 前定下来）

### 7. 清理没有实现的接口 · P0 ✅
- **现状**：`ringloop.go` 里从 gnet 照搬的 `Conn`、`Reader`、`Writer`、`AsyncCallback` 等接口（`AsyncWrite`、`Wake`、`Close(callback)` 等）都没有实现，会误导用户。
- **要做的**：删掉，或者在第 9 项中真正实现。

### 8. 选项模式的构造函数 · P1 ✅
- **现状**：`NewMany(addr, size, sqpoll, num, options, handler)` 参数多，`sqpoll` 已经弃用，其他配置又分散在 `URingNet` 的公开字段里（`BufferSize`、`SpinTime`、`ZeroCopyThreshold` 等），必须在 `SetLoops` 之前设置。
- **要做的**：改成类似下面的写法，旧 API 标记为 Deprecated 并保留一个版本：
  ```go
  srv, err := uringnet.Listen("tcp", ":8080", handler,
      uringnet.WithRings(runtime.NumCPU()),
      uringnet.WithBuffers(256, 16<<10),
      uringnet.WithBusyPoll(50*time.Microsecond),
  )
  ```

### 9. 连接对象 · P1 ✅
- **现状**：`OnTraffic(data *UserData, eng *URingNet)` 把引擎内部暴露给用户；不能在其他 goroutine 里向连接写数据。
- **要做的**：提供 `Conn` 对象（`Write`、`WriteNoCopy`、`Close`、`Context`、`RemoteAddr`、`LocalAddr`），并支持从任意 goroutine 安全地异步写入（内部通过 `Post` 投递到事件循环）。

### 10. 统一动作语义 · P1 ✅
- **现状**：`Echo` 和 `Write`、`None` 和 `Read` 是重复的；`EchoAndClose` 的命名不直观。
- **要做的**：精简为 `None`、`Close`、`Shutdown`，写数据统一通过 `Conn.Write` 完成。

### 11. 日志和钩子 · P1 ✅
- **现状**：没有日志接口，部分内部错误被静默忽略。
- **要做的**：接受用户传入的 `*slog.Logger`；关键事件（accept 失败、缓冲区耗尽、零拷贝回退）记录日志。

---

## 三、功能

### 12. 客户端（Dial） · P2 ✅
目前只能做服务端。网关和代理需要主动发起连接：用 `IORING_OP_CONNECT` 实现，连接建立后和服务端连接走同一套回调。

### 13. UDP · P2 ✅
物联网常用（CoAP、自定义协议）。用 multishot `recvmsg` 和 `sendmsg` 实现，回调中提供对端地址。

### 14. TLS · P2 ✅
可以基于 `crypto/tls` 加内存传输层实现，或者支持内核 TLS（kTLS），以便和零拷贝发送配合使用。

### 15. 编解码框架 · P2 ✅
内置长度前缀、分隔符、固定长度等常用分帧方式，用户不必自己处理粘包和半包。

### 16. 零拷贝接收（zcrx） · P3
`IORING_OP_RECV_ZC`，需要 Linux 6.15+ 和网卡支持（header split、flow steering、专用接收队列）。

---

## 四、可观测性

### 17. 运行指标 · P2 ✅
连接数、收发字节数、`ENOBUFS` 次数、发送队列长度、忙轮询命中率、每轮循环的批量大小；提供 `Stats()` 方法，以及 expvar 或 Prometheus 接口。

### 18. 性能分析文档 · P3
说明如何对锁定在系统线程上的事件循环做 pprof 分析和追踪。

---

## 五、工程和发布

### 19. CI 升级 · P1 ✅
- **现状**：`.github/workflows/go.yml` 使用 Go 1.20 和旧版 action（checkout@v3、setup-go@v4），没有 `-race`，没有 `go vet` 和 staticcheck，没有生成覆盖率文件。
- **要做的**：
  - 升级 Go 版本和 action，加上 `go vet`、staticcheck、`-race`（`byteslice` 测试在 `-race` 下本来就会失败，需要先单独处理）。
  - 确认 GitHub runner 上能否使用 io_uring。
  - 用虚拟机跑多内核版本的矩阵测试（6.1、6.6、6.12、6.17）。
  - 上传覆盖率；有数据后再把 Codecov 徽章加回 README。

### 20. 依赖固定 · P1 ✅
- **现状**：依赖的是 `iouring-go` 的伪版本（一个提交）；`golang.org/x/sys` 是比较旧的 v0.12.0。
- **要做的**：给 iouring-go 打正式 tag，并升级 `x/sys`。

### 21. 版本发布 · P1 ✅
- **现状**：最新 tag 还是 `v0.0.3-alpha`，是重写之前的代码。
- **要做的**：发布 `v0.1.0`，写 CHANGELOG，说明 API 的稳定性承诺和支持的内核版本范围。

### 22. 文档 · P2 ✅
- 完善 godoc 注释（现在中英文混杂，建议统一用英文）。
- 增加 `example_test.go`，示例会出现在 pkg.go.dev 上。
- 写架构说明（事件循环、缓冲区生命周期、零拷贝流程）和贡献指南。

### 23. 跨机器零拷贝实测 · P1
README 结论第 5 条提到的下一步：用两台机器和真实网卡测一次（`bench/scripts/serve.sh` + `remote.sh`），补上零拷贝发送的实际数据，并更新 README 和图表。

---

## 建议顺序

| 阶段 | 内容 | 条目 |
|---|---|---|
| 1 | 能上生产 | 1、2、3、7 |
| 2 | API 定型并发布 v0.1.0 | 8、9、10、11、4、5、19、20、21 |
| 3 | 测试和数据补全 | 6、23 |
| 4 | 按需求扩展功能 | 12–15、17、22 |
| 5 | 长期目标 | 16、18 |
