# 共享 NBD / MAP_PRIVATE RAM 的增量保存实施方案

2026-10-30。本文是实施设计，没有实现或验证 tracked_delta；不改变步骤一未整体验收、步骤二未开始的状态。

基线：E2B 分支 `feat/shared-nbd-memory-20261030`，HEAD `22f49c320`；指定 Firecracker 仓库同名分支，HEAD `a977afa86d0da2ad96216b0d9daf6d72428015d1`。沿用任务分支分别提交，不修改 AgentENV。参照 [AgentENV 源码对照](20261030-agentenv-review.md) 与原交接文档执行；后者的阶段验收要求继续有效。

## 1. 首版范围和正确性合同

保留共享只读 NBD + File/MAP_PRIVATE 恢复，独立新增默认关闭的 tracked_delta。借鉴 AgentENV 的“冻结、收集变化范围、直接读取、提交不可变层”，继续使用 E2B 的 bitmap、process_vm_readv 和 header，不迁移 ublk/OverlayBD。

首版仅支持已有的 Pause → 保存 → 关闭 FC 生命周期。每次从父快照恢复时建立新的追踪实例，从恢复后首次运行前一直累计到最终冻结；不引入清位/提交 ACK 协议。冷启动或没有已知父逻辑映射时仍完整导出。原地保存后继续运行暂不开放；后续单独设计 backing 基线、累计 ledger 与 checkpoint epoch。

必须满足：任意 RAM 页如果相对父快照的最终字节不同，则该页一定在候选集中。允许多报，不允许漏报。候选集为：

```text
C = KVM guest CPU 写位图
  ∪ VMM 设备、队列、异步完成的软件写位图
  ∪ balloon/FPR discard、匿名重映射等显式变化范围
  ∪ 经路径审计确定必须保守纳入的范围
```

guest 内部新分配的 heap/stack 仍是 RAM offset，通过实际写入追踪；不是 host 的新 mmap 通知。不能仅根据 MAP_PRIVATE、匿名页、present、exclusive 或 mincore 推断完整候选。swap 不删除候选位，内容读取负责换入；不额外经过 resident 过滤。

首版统一 `Dirty=C`、`Empty=∅`，包括候选中的全零页。未入选页继承父 header；这已经符合 E2B 格式，可避免增加一次 RAM 扫描或改动现有“先构造 header、再导出”的流程。零页压缩后续再做：同一次读取中判零，生成最终 Dirty/Empty 后构造 header，禁止把未读取页当成零。

## 2. 提交一：FC 补全候选覆盖与冻结合同

在指定 FC 仓库实施：

| 文件 / 符号 | 修改及检查 |
| --- | --- |
| `src/vmm/src/vstate/hdbss.rs::configure` | 复用 `--kvm-dirty-bitmap --dirty-export-policy bitmap`，不启用 HDBSS；维持 host 4096 页和 region 软件 bitmap 的显式前提检查 |
| `src/vmm/src/persist.rs` 的 File restore | `enable_diff_snapshots=true` 从恢复、注册 KVM slot 到首次运行前贯通，不允许运行一段时间后才开始追踪 |
| `src/vmm/src/vstate/vm.rs::get_dirty_bitmap_measured` | 保留逐 slot harvest 后立即 OR 到累计 ledger；重复收集不丢已收集位，后续 slot 出错不回滚前面已清出的 KVM 位；不清 live 软件 bitmap |
| `src/vmm/src/devices/virtio/balloon/util.rs::remove_range` | 验证范围后、首次 mmap/madvise 修改之前，将整个范围标为软件 dirty；失败允许多报。尤其覆盖 mmap 已成功、madvise 失败的情况 |
| `src/vmm/src/devices/virtio/` 各写入路径 | 审计 block/net/rng/vsock、队列、原始 HVA/IOVec、异步完成；补齐绕过 vm-memory 写接口的标记。不能用“队列已标记”替代 payload 标记 |
| `src/vmm/src/persist.rs::create_snapshot`、`lib.rs` | 沿用 a977 的设备 drain/冻结顺序；确保所有最终写入结束且队列已标记后才允许外部导出查询，冻结后直到 RAM 读取完毕无新写入 |
| dirty-memory API 的模型及文档 | 扩展响应，使 E2B 能确认 bitmap 策略、追踪有效、逻辑大小、页大小和当前外部导出冻结状态；旧字段继续兼容 |

API 返回的“追踪有效”须来自 FC 实际配置和生命周期状态，不是客户端传回的开关。缺少字段视为不支持，不能把老 FC 的成功 HTTP 响应或空 bitmap 当作完整追踪。API 字段名在实现时统一写入 FC schema 并同步生成 E2B 客户端。

bitmap 的位置必须对应快照逻辑 RAM 流；不能直接把 GPA、slot 内偏移或按 HVA 排序后的编号作为逻辑 offset。多 region 与布局边界单独测试。

提交前完成 FC 单测、格式检查和可用目标的编译。模拟 dirty log 失败只能证明 ledger 算法；CPU 写、设备 DMA 和 swap 覆盖仍需真实 KVM 验证。

## 3. 提交二：E2B 接入及持久化

建议新增配置 `RAM_EXPORT_MODE=full_logical|tracked_delta`，默认 `full_logical`。这是拟新增配置，不是当前已有环境变量。

| 文件 | 修改 |
| --- | --- |
| `packages/orchestrator/internal/cfg/model.go` | 校验枚举；限定首版 File/NBD、4096 页、已知父快照；不影响原 UFFD 路线 |
| `sandbox/fc/script_builder.go` | 仅符合条件的 tracked_delta restore 加上述 FC flags，保留普通文件 B0 与共享 NBD B1 的相同追踪配置 |
| `sandbox/fc/snapshot_memory_backend.go`、`client.go`、`process.go` | 显式传递追踪选项；将当前硬编码 false 的 `EnableDiffSnapshots` 改成按恢复策略设置；接入并验证 FC 响应的追踪/冻结合同 |
| `sandbox/uffd/` 的 MemoryBackend 实现 | 新增 File 专用追踪 backend，复用无 UFFD 的就绪/退出行为；`DiffMetadata` 调用 `DirtyMemory`，校验后返回候选。`NoopMemory` 保持安全全量，不修改旧 UFFD 的语义 |
| `sandbox/sandbox.go` | 恢复时选择 backend；保留 shared backing lease 包装；Pause 后保存设备状态、收集候选、读取 RAM、保存 rootfs，最后发布 metadata |
| `sandbox/fc/memory.go`、`block/cache.go` | 复用逻辑范围导出及有界读取；只合并相邻候选，不能因扩大跨空洞范围而暗中读全部 RAM；完善空候选、短读/EINTR、取消、溢出和失败清理 |
| `sandbox/checkpoint.go`、`internal/template/metadata/template_metadata.go` | 显式记录实际导出模式；tracked_delta 新格式版本附载荷长度、载荷 SHA256 与追踪合同版本，仍绑定 RAM/rootfs header 和 FC state |
| `sandbox/template/restore_memory.go` | 兼容旧格式；明确接受新 tracked_delta 格式并验证字段。不能把增量载荷 SHA256 填入 `LogicalRAMSHA256`，它不是完整逻辑 RAM 校验 |
| `packages/shared/pkg/storage/header/metadata.go` | 沿用 ToDiffHeader；新增相对父映射的增量、零覆盖、空候选和多代测试，不更换存储格式 |

表中 `sandbox/` 相对 `packages/orchestrator/internal/`。新 checkpoint 建议用版本 2，旧版本 1 的 full_logical/uffd_delta 继续可读；旧 reader 拒绝新版增量，避免误解释。restore 不应为了校验而强制读取全部远程 RAM；载荷摘要不代表未读取远程字节已被验证，继续遵守存储不可变对象合同。

执行顺序固定为：

```text
恢复父 header + 所需 BuildID 的依赖 lease
 → 配置追踪并加载 File backend（尚未运行 vCPU）
 → 运行
 → pause vCPU
 → state-only snapshot：drain/freeze 设备、保存最终 CPU/设备状态
 → 校验追踪合同，harvest C
 → process_vm_readv 只读 C，按逻辑顺序打包并在同一遍计算载荷摘要
 → ToDiffHeader 合并父映射；保存 rootfs diff
 → fsync RAM/state/rootfs/header，最后发布 metadata
 → 成功关闭或失败清理；本轮不清追踪位后原地 resume
```

首版现有代码在 RAM 导出前构造 header 也正确，因为 Dirty=C、Empty=∅ 已固定。上图表示发布依赖，不要求无意义地移动这段代码。

缺少能力、冷启动等已知不支持场景，在导出前选择 full_logical 并记录实际模式与原因；不得伪称 tracked_delta。bitmap 获取错误、非法范围、RAM 短读、源故障、输出失败时不发布 checkpoint；保留原父快照和累计 ledger，遵循当前 Pause 失败关闭 FC 的行为。不要新增没有实现安全恢复边界的原地重试/resume。冻结区间内的导出重试不能主动清 ledger。

恢复继续使用合并后的 header 按 BuildID/offset 定位历史对象，不逐代递归套用全部快照。运行期 pin 与持久化 GC 是两件事：发布方必须保留所有被 header 引用的历史对象；缺失对象明确报错，不能回退为零。

## 4. 提交三：测试、目标证据与启用门槛

新增 Linux KVM 集成模式，不覆盖已有完整导出证据。在同一冻结点先取得 C，再读取候选，最后读取完整逻辑 RAM 作为 oracle；完整读取不能提前影响候选选择。将父逻辑映射叠加增量后的每个字节与该 oracle 比较，恢复后再验证一次。物理共享采样在完整 oracle 读取之前完成。

| 用例 | 必须验证 |
| --- | --- |
| 冷非零父页未访问 | 不入选仍继承非零字节，不能变 Empty |
| guest CPU 写、私有 COW | 变化页入选，其他 VM 与父 backing 不变 |
| VMM payload / 队列 / 异步 block、net | 最终完成写入均入选，state 与 RAM 同一冻结点 |
| 非零改零 | 首版以 Dirty 零载荷覆盖父内容，恢复为零 |
| balloon/FPR discard 后不写 | 父非零被零覆盖，即使最终没有 resident PTE |
| discard 后重写 | 导出最终内容；含 mmap 后 madvise 失败的保守标记测试 |
| 真实 swap | 目标页确实已换出，仍在 C 且能读回；请求换出不算成功证据 |
| 多 region、跨 region、非连续 GPA | 逻辑 offset 与数据顺序准确；越界 bitmap 拒绝 |
| A→B→C、分叉 | 每代父映射正确；历史对象存在时可恢复，缺失时明确失败 |
| 没有应用修改的轮次 | 无真实变化时可零候选；设备维护产生的候选允许存在，不能强求零 |
| 重复 harvest / 中途 slot 失败 | 已 harvest 位不丢；同冻结点重复查询不漏页 |
| 短读、EINTR、零进展、取消、磁盘失败 | 不发布 metadata、不破坏父快照、资源清理可证明 |
| 老 FC / 未追踪 / 未冻结 / 非 4096 页 | 拒绝增量或显式 full_logical；不静默输出不完整增量 |
| 多 VM lease、NBD source 故障、设备复用 | 导出期间 backing 仍有效；故障不补零，退出后正确回收 |

编译与软件合同测试先本地完成，可用 `H-arch → agent03.coder` 完成受限目标的 FC 构建及 256–512 MiB 小 VM KVM 测试，无需全量部署。真实 swap 若仍不能满足目标页换出的前提，应如实保留待验收状态，不用模拟替代。

至少记录：C 页数、实际读取字节、载荷字节、harvest/读取/保存耗时、全量 oracle 耗时、父及最终 header 摘要、恢复全逻辑 RAM 摘要、目标 kernel/page-size/FC 源码与二进制 hash。性能统计将 oracle 成本单列；正常增量路径不读取全部 RAM。由于保守标记和写后还原，C 可以大于最终与父不同的页集合。

tracked_delta 先实验开关启用，完成候选完备性与恢复验证后再评估默认策略。该增量优化的通过不能替代原步骤一的全 trace、swap/pressure、多 VM 故障等未完成门槛，也不自动允许开始步骤二。

改造难度中等，正确性验证难度较高；最大风险是漏掉设备/重映射写，表现为快照可读但字节错误。性能风险包括 KVM 写追踪开销、保守候选过多与碎片化范围；必须实测，不能保证所有负载均受益。配置与读取器先升级，再部署配套 FC，最后打开实验开关。回退时关闭 tracked_delta，让后续保存恢复 full_logical；已经生成的版本 2 checkpoint 仍需新版 reader，若要回退 reader，先用新版恢复并导出兼容的完整快照，不能直接修改版本字段。

## 5. 预取独立实施

候选增量与预取分别提交、分别验收。后者参考 AgentENV，在取得共享设备后对实际 `/dev/nbdX` 做有预算的 buffered read；任务 key 包含内容身份、设备 generation 和热点 manifest，worker 持 lease，最后订阅者退出后取消并等待在途读结束。源端 4 MiB chunk 不要求预取系统调用也是 4 MiB；批量大小、并发数和总字节预算分别配置。

首轮增量正确性测试关闭预取；通过后再验证打开预取不会改变候选完备性、共享/COW 与恢复结果。pmem/DAX 继续留在步骤二，不混入本轮 RAM 布局。
