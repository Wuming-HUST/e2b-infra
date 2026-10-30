# AgentENV 内存快照实现对照

2026-10-30。只读源码审查；没有执行 AgentENV 构建、单测或 KVM 测试，没有修改 AgentENV、E2B 或 FC 的运行时代码。本记录不改变步骤一未整体验收、步骤二未开始的状态。

## 版本与证据范围

- AgentENV：`/Users/yangshengxin/Documents/Projects/11.AgentSandbox/AgentENV`，HEAD `dc4df4b08ae554f1e29bdffc7cb3715549c5646a`，审查时工作树干净。
- E2B：分支 `feat/shared-nbd-memory-20261030`，审查基线 `b18b15e306297d6e5d9ac5cc53d16e7ae4db7793`。
- E2B 配套 FC：`a977afa86d0da2ad96216b0d9daf6d72428015d1`。
- AgentENV `config/deps_manifest.toml` 固定 KVM FC `1.15.1-patch-v2`，默认 KVM guest kernel `vmlinux-6.18`。其配套 FC 实现源码不在 AgentENV 目录；尝试只读获取固定 tag 的源码遇到 `No route to host`，因此本记录不声称已验证该 FC 的脏位收集、清位、swap 或设备冻结实现。
- AgentENV 中 `thirdparty/firecracker-client` 是 API 模型，不是 FC 实现。模型包含 pmem 也不能作为 pmem 设备已经实现/验收的证据。

## 当前主路径

1. `src/cfg.rs:530`：`memory_snapshot.track_dirty_pages` 默认 true，注释指定 KVM dirty tracking；PVM 会关闭该选项。关闭后的 mincore 路线只在配置/文档中说明，本次未核验配套 FC 的实际算法。
2. `src/sandbox/firecracker/sandbox.rs:1715`：取得共享只读 ublk 内存设备；`:1737` 通过 File backend 加载并传递 `track_dirty_pages`，先保持暂停，再完成启动配置。冷启动同样传入追踪配置（`:2183`）。
3. `sandbox.rs:1000`：pause 后进入 `snapshot_to_dir`；`:1179` 先创建 state-only snapshot，再获取 dirty ranges 并直接打包 RAM。
4. `instance.rs:475`：`PUT /snapshot/create` 使用 Diff 且不传 RAM 输出路径；`:486` 查询 `GET /vm/dirty-memory-ranges`。
5. `thirdparty/firecracker-client/src/models/dirty_memory_range.rs:15`：范围包含 HVA、连续逻辑 RAM image offset、length；image offset 明确不是 GPA。`DirtyMemoryRanges` 同时携带页大小与总逻辑 RAM 大小。
6. `overlaybd_snapshot.rs:692`：强制 4096 页，检查负数、对齐、正长度、逻辑边界、加法溢出及目标重叠；超大范围拆分成可表示的 segment。源地址与目标逻辑偏移分开处理。
7. `process_vm_reader.rs:29`：直接 `process_vm_readv` 到输出缓冲，循环处理短读和 EINTR，零进展/地址溢出报错。源码明确指出同步系统调用仍在 Tokio worker 上执行，不能当作完全异步 I/O 的实现借鉴。
8. `overlaybd_snapshot.rs:652`：候选范围直接压实到临时 OverlayBD 层，成功 rename，失败删除临时文件；局部 raw，压缩延后到 publish。
9. `overlaybd_snapshot.rs:572`：继承已有 memory lowers，追加新层；`:414` 为运行时 suffix 做链接/拷贝或按预算压实。`snapshot/resolver.rs:96` 根据 committed memory layer 列表物化配置并持有缓存 lease。

仓库仍包含 `storage/uffd-core`：`handler.rs:70` 明确没有 WP、把所有 accessed 页保守视为 dirty，维护 Accessed/Removed；`overlaybd.rs:66` 有旧增量层打包。这不是上述当前主恢复/保存链路，不应把它描述为 AgentENV 当前使用 WP_ASYNC 精确追踪。

## 对当前 E2B 的借鉴

| 机制 | 借鉴方式 | 当前 E2B 差异 |
| --- | --- | --- |
| File restore 与 dirty tracking 独立配置 | 映射仍用共享 NBD/MAP_PRIVATE，同时开启有效的 KVM/VMM bitmap | `fc/client.go:62` 的 `EnableDiffSnapshots` 仍为 false；File/NBD 用全量候选 |
| 脏范围收集与输出格式分开 | 复用已有 FC dirty 接口、逻辑 RAM mapping 和 E2B header，不要求迁移 OverlayBD/ublk | E2B 已有 bitmap→逻辑范围→process_vm_readv 的导出基础 |
| CPU 与设备状态先固定，再读 RAM | 保留本轮 FC 的设备 drain/冻结修复，最终写入结束后收集候选 | 不能仅因 AgentENV 调用了 state-only API，就假设其配套 FC 实现与当前 FC 等价 |
| 父层依赖与生命周期显式化 | 保留 E2B header 的 BuildID/offset 继承及 PinnedFile lease | 已有这一基础，无需更换快照格式；存储 GC 仍须保留所有被引用对象 |
| 临时层封存与延后压缩 | 沿用当前 metadata 最后发布与有界缓冲；将压缩作为独立优化 | 文件 rename 成功本身不等于整套 RAM/state/rootfs 已持久发布 |
| 有预算的历史层压实 | 评估依赖对象数、header extent 数与恢复成本后触发 | E2B header 已合并逻辑映射，不宜机械复制 OverlayBD 的 32 层阈值 |

最值得优先复用的是 **KVM/VMM 候选 bitmap → 冻结后候选读取 → E2B 增量 header**。当前 FC 已有 `--kvm-dirty-bitmap`、`--dirty-export-policy bitmap`、累计 harvest ledger 和软件 bitmap 合并；该路线不要求 HDBSS capability，也不要求重新实现 UFFD missing pager。但存在这些接口不等于全路径已正确接通。

候选至少覆盖：

```text
KVM guest CPU 写
∪ VMM 设备/队列/异步完成写
∪ balloon/FPR discard、匿名重映射范围
∪ 经验证需要保守纳入的其他变化
```

已换出的候选页必须保留并读回；不能再经过 present/mincore 筛选丢弃。私有匿名页/swap 的 pagemap 扫描可以作为补充候选或独立交叉校验，不能仅看 resident 或 exclusive 位。新 guest heap/stack 页仍是普通 guest RAM offset，guest 内部的分配并不自动产生 host 用户态通知。

`Dirty` 输出最终内容，允许包含全零页；`Empty` 则无需内容载荷，直接覆盖父数据为零；其余继承。首版可把所有候选写入 `Dirty`、保持 `Empty` 为空，先不做零页压缩。候选不要求每一页最终字节都与父不同，允许保守多导出。失败时保留 ledger，只有建立了正确的新基线后才清位。同一 FC 原地继续运行时，必须区分“相对启动 backing 的累计变化”和“相对最新 checkpoint 的本轮变化”。

## 可直接借鉴的共享块设备预取

这部分与前一轮用户询问的“4 MiB + 预取器”直接相关：

- `storage/overlaybd/src/startup_manifest.rs:77`：从 first-touch 页列表生成按首次需求排序的连续范围。当前 producer 的 `prefix_pages` 为空；不能把注释中的 exact prefix 当作已实际产出。
- `src/sandbox/firecracker/startup_pack.rs:89`：用独立 VM/专用设备记录，`:229` 关闭 recording image 的 background download，避免共享缓存命中或批量下载影响观测。其块设备 first-read 记录仍不等于精确 guest 每页访问次数。
- `sandbox.rs:1992`：本地预取在取得实际共享设备后启动；只预热 lower 文件不能替代设备 page cache。
- `startup_pack.rs:471`：按 live device generation + manifest SHA 去重；每 VM 是 subscriber，最后一个退出才取消共同 worker，避免设备号复用误命中旧任务。
- `startup_pack.rs:279`、`:588`、`:690`：全局最多 4 个读 worker，单次缓冲 1 MiB，设总字节预算/截止时间，在有界读取间检查取消。
- worker 持有设备 lease；timeout 不能撤销已进入内核的读，必须等待实际读结束再完成设备释放。预取失败降级按需恢复，不把可选优化变成必需依赖。

E2B 应新增读取 **实际共享 `/dev/nbdX` 的 buffered-read 预取器**，复用 metadata 的热点顺序与共享 lease；4 MiB 是存储 chunk 粒度，预取 syscall 批量可以独立设置。现有 UFFD copy 阶段不能直接接过来，否则会改变物理共享路径。NBD 编号必须与 live lease generation 绑定，不能仅按路径作任务身份。

## 需要保留的验收边界

本次只发现并审阅了 AgentENV 的测试源码，没有运行：dirty range 对齐/重叠/大范围拆分、临时输出失败保留原文件、连续 capture/分叉/删除快照后原实例恢复、预取取消/超时保留 lease 等合同，均可借鉴为 E2B 用例。

启用 E2B `tracked_delta` 前，在同一冻结区间做“父映射 + 增量”与 `full_logical` 逐块一致比较，覆盖冷非零未访问页、CPU/设备写、归零、真实 swap、discard 后未写/重写、多 region、A→B→C、无修改轮次、失败重试和原地继续运行。AgentENV 配套 FC 的具体 dirty selection/清位尚未核验，尤其不能从接口名称推断已覆盖 swap 或 discard。

结论：借鉴 AgentENV 的数据流、范围合同、依赖管理和预取生命周期，保留 E2B 的存储格式、NBD 和已修复的冻结边界。当前不需要先迁移 ublk/OverlayBD，也不宜直接替换 FC 二进制。
