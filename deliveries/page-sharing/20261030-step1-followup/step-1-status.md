# 步骤一接续实施与验收记录

2026-10-30。**步骤一代码、关键设备验证及真实 Factory 集成已完成；完整原 trace 等验收仍未完成，步骤二未开始。** 不需要部署全套 E2B 才能验证本次运行时链路。当前 32 GiB 容器已实际运行 KVM/NBD 和真实 envd，不把容量限制当作未测试的理由。

本记录接续[首轮交付](../20261030-step1/step-1-status.md)，首轮目录原始文件保持不变。最终提交、基线、构建产物和源码哈希见 `revision.json`、`source-sha256.txt`、`source-match.log`；本目录所有制品的校验见 `manifest-sha256.txt`。

## 分支、实现与修复

两个用户指定仓库都在新分支 `feat/shared-nbd-memory-20261030`：

- E2B：首轮核心实现 `fa6e5cedb`，本轮集成/设备测试与可选 initrd 接口 `f70ca4950`。
- Firecracker：首轮冻结修复 `0ce251366`，本轮设备完成边界修复 `a977afa86`。指定原仓库已快进到该提交；隔离工作树分支 `feat/shared-nbd-freeze-20261030` 为同一提交。
- 源码补丁 `infra-followup.patch`、`firecracker-followup.patch` 分别接在首轮提交之后。既有未跟踪研究资料未纳入提交，未修改原实验镜像或二进制。

共享 NBD/source/lease、默认安全 `full_logical` 导出、metadata 最后发布与生命周期合同沿用首轮实现。共享模式仍显式开关、默认关闭；本轮没有开放 File/NBD `tracked_delta`，没有实现 pmem/DAX。

本轮 M11 找到真实错误：旧 FC 先保存 vCPU/GIC，设备序列化阶段才排空异步块完成。后续完成会更新 RAM/used ring/IRQ，造成旧 GIC 与新 RAM 不配对，恢复后块 I/O 停滞。修复把 block drain/flush/completion 和 vsock reset 提前到保存 CPU/GIC 前；设备序列化保持只读。flush、vsock 写入/队列错误向上传播，失败时不发布 state/RAM。同冻结区间 vsock reset 幂等，Resume 后清位。activated vsock 无可用 EVQ 时明确拒绝 checkpoint，不承诺 live 连接跨恢复延续。

E2B 新增可选 `ProcessOptions.InitrdPath`，默认空值保持原 boot-source 请求；目标实际驱动为模块，独立夹具通过 initrd 加载。空值省略与带空格路径的 Unix HTTP 合同通过。只改测试夹具启动方式，生产 rootfs/virtio-blk 布局保持原路径。

## 实测环境与源码对应

以 `setup/environment.log` 为准：host **openEuler 24.03 LTS-SP2** / Linux `6.6.0-95.0.0.99.oe2403sp2.aarch64`，guest **Debian 6.1.0-50-arm64**，host/guest 页均 4096。容器 `memory.max=34359738368`，CPU quota 16 cores。首轮报告的“openEuler 24.04”应理解为原方案目标，不能作为实际 OS 记录。

Go 1.25.4；Rust 1.85.0 GNU ARM64。最终 FC SHA256：

`8830ff065716e073b74bed06fe073356ffff839eef5fe6039f3905b2b5149349`

E2B 65 个变更源码/脚本与目标机逐字节一致。FC 331 个 tracked Rust/Cargo 文件与最终提交逐字节一致，见 `firecracker-source-commit-match.json`；最终实际 release 重编译 1m06s。中间 rustfmt 递归触及远程 baseline、以及旧 mtime 导致 Cargo 复用的记录均保留；这些中间结果不作为最终构建凭据。GNU 构建仍使用 baseline 的 unimplemented seccomp fallback，不替代生产 musl/seccomp 打包验收。ARM snapshot v7 沿用原 FC 基线，并非本次新增格式；本轮生成/恢复均使用配套 FC，不声称旧 ARM v6 跨版本兼容。

没有启用 KSM、修改全局 swap 或执行全机 drop_caches。只创建本轮 VM、动态 NBD 和自有网络 slots。

## 新增目标验证

| 项目 | 最终结果与原始证据 |
| --- | --- |
| 软件回归 | `software/exit-codes.tsv` 七项全部 0：91 项顶层合同、race、header/metadata、orchestrator/helper/test binary/guest 构建 |
| M10 balloon | 192 MiB VM，实际 inflate 96 MiB/deflate；10,239 个非零父页 discard 后未写及实际复用重写均通过 |
| M10 FPR | target/actual 始终 0；7,680 页未写、1,536 页实际复用重写；设备 reporting、source/另一 VM 隔离、全 RAM/native/hash/两次恢复通过 |
| M10 清理 | 4 项失败清理合同本地与 ARM64 通过；未知映射保留 source/runner，不复用设备。最终两 run 清理无错误；含历史 62 个自有 PID 均退出。见 `m10/status.md` |
| M11 并发 | 2 vCPU/128 MiB、4 个 O_DIRECT Async/io_uring worker、持续 TAP RX；B0/File 与 B1/NBD 各 3 轮冻结及恢复后新增完成，全 RAM/native/disk oracle、逐字节磁盘记录通过；注入 16,615 帧，errors=0 |
| FC 合同 | 5 项新合同：Async completion→GIC 边界、真实 flush EINVAL 拒绝发布、vsock 成功幂等/空队列/写失败；另 5 项相关原有回归通过。严格旧版本失败保留于 `m11/target/m11-baseline-strict/` |
| 真实 Factory | `factory-run-02` 15.04s PASS：Create→真实 envd→Pause→Local upload→Cache→Resume；B0/B1 各 A→B→C；5 个完整 256 MiB native-vs-实际 Pause RAM hash 相等 |
| 多 VM/identity | Factory B1 同 A 两 VM 同 NBD path，左编辑/退出后右保持 A；A/B 不同 identity 同时持有不同 path，释放 A 后 B 继续正确；B0 三次实际 regular inode、rw-p 映射凭证齐全 |
| 原工具片段 | 保留原 Write 50/55、Edit 60/63/65 的完整参数/调用 ID/路径；真实 envd 执行，必须成功 End/exit0；每代两个完整文件大小与 SHA 正确。**仅 5 工具片段，非完整原 trace** |

详细解释及复现：[M10](m10/status.md)、[M11](m11/m11.md)、[Factory](factory.md)。首轮 namespace 内 envd 可达而 host 超时，原因是夹具缺 guest 回程路由；未修改运行中 guest 伪造通过，而是在其自然失败清理后用独立 v2 夹具重跑。原失败日志保留。

最终同源 FC 另行复验 M01/M02/M03/M05/M07：冷非零页、三次稳定共享 PFN、写/归零 PFN 分离、lease 释放、native 全 RAM 及恢复均通过，见 `target/kvm-source-matched/`。

**该复验的 M09 严格状态仍是 `unaccepted`**：只观察到两个请求页中的一个实际 swapped，另一页未换出，不能把 MADV_PAGEOUT 成功返回当作双页换出成功。已发生换出的非零页及其余逻辑 RAM 导出、native hash、恢复 oracle 均正确；保留全部样本，不放宽判据或重复运行筛选通过。首轮两个 COW 页均实际 swapped 的机制证据仍在首轮目录，但不是持续压力测试。最终该 run 的八个自有 PID 全部退出，helper 记录 closed。

## M01–M13 当前边界

| ID | 已证实 | 尚未验收 |
| --- | --- | --- |
| M01 | 冷 File 非零父页、absent PTE、全量导出/恢复；Factory full_logical 同冻结 oracle | 更大原应用工作集 |
| M02 | 两真实 FC 三次稳定共享 PFN；Factory 同源同 backing | 长期密度/容量收益 |
| M03 | COW/归零 PFN 分离、另 VM/source 不变；Factory 文件隔离 | 完整原 trace |
| M04 | identity 单测；Factory 两不同 identity 同时持有不同设备且内容正确 | 跨节点/对象存储部署 |
| M05 | race/取消/Drain 合同；Factory 多 holder、释放与 shutdown | 节点崩溃/故障重启 |
| M06 | source 短读/范围/校验、原子发布失败合同；真实 NBD EIO；FC flush 错误拒绝发布 | 真实云对象故障 |
| M07 | File/NBD 及生产 Factory A→B→C、全 RAM/native、文件 oracle | 完整应用状态 |
| M08 | 首轮专属 NBD clean cache 实际驱逐与 refault、私有 COW 保留 | 与持续多 VM 压力组合 |
| M09 | 首轮双页实际换出保存；最终单页真实换出内容与恢复正确 | 最终双页换出判据未满足；持续压力/多 VM swap |
| M10 | 真实 balloon 与 FPR 两种 discard→未写/重写、source/另 VM 隔离 | 生产压力与应用组合 |
| M11 | 真实 block/net 完成竞争、GIC 顺序、稳定完整 RAM/disk、Factory rootfs 多代 | live vsock 流量/连接语义 |
| M12 | HTTP range 延迟/断流/版本/校验/并发及失败广播合同 | 多真实 VM 远端故障停止链路、真实对象存储 |
| M13 | 多 region/顺序/跨界/短读/溢出软件合同 | 实际多 region 大内存 KVM |

## 剩余验收与内网接续

1. 完整原 trace：原素材已找到，1052 个工具仅执行了 5 个。当前 guest 无 `/app`、bash、Rust/Java/GCC/Cargo cache。按现有任务 Dockerfile 构建完整任务 guest、生成 goldens/lock/cache，补齐 session/tool-results 或真正重建工具生命周期，再对 B0/B1 全部工具和最终文件做对照。具体缺项与固定版本见 `factory.md`；不能从 32 GiB 推断它无法运行，也不能将目前片段冒充通过。
2. 在目标部署配置完成持续压力/swap、实际多 region、远端对象存储及多 VM 故障停止验证；生产 musl/seccomp 包装单独验证。当前不报告生产容量或性能收益。
3. 这些验收闭环后才开始步骤二。回退共享功能使用 `SHARED_NBD_MEMORY_ENABLED=false`，保留安全 full_logical 修复与配套 FC；不在活动 VM 上更换 source 或重定向 NBD。

脚本入口 `scripts/memory-lab/page-sharing/README.md`、本目录各专项报告与原始 driver 可直接复现。大 RAM/disk 镜像留在远端独立 run 目录；本地携带状态、日志、哈希、PID、映射、CSV 和命令。设备编号可被其他并行独立 run 复用，不能用某次全局 `/sys/block/nbdX/pid` 观察推断它仍属于先前 lease；保留观察原字节，并以各自 ownership、drain 与进程回执作判断。
