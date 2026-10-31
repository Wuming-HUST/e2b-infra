# 内网功能验证与端到端超分测试交接

更新：2026-10-31。用户决定不再依赖 agent03，迁入内网先完成功能验证，再做端到端超分实验。本交接不继续连接或启动 agent03 任务。

## 已有结论与证据边界

核心同模板 RAM/rootfs 物理页共享、COW/copy-up 隔离、源模板不变、完整/增量多代恢复已经通过。已归档并离线复核的共享证明与完整合成 trace 分别见 [共享证明](../20261031-sharing-proof/README.md)、[合成 trace](../20261031-marathon-pmem/README.md)。合成 trace 不等于 1052 条原始工具执行；尚无端到端容量或 SLO 结论。

本轮 agent03 已返回通过的兼容性矩阵、活跃 vsock、双 VM HTTP 故障、三 RAM slot、真实 ENOSPC、七包 race，及 musl 构建，详见 [补测状态](status.md)。这些新增收据没有全部取回，迁入内网后应重跑以建立可交付证据；不能仅根据旧的 exit 0 勾选内网验收。

最后观测到 agent03 文件系统可用空间为 0，随后 Coder 数据库 DNS/API 错误和 SSH 超时；未确认宿主是否宕机。不得据此声称远端进程已清理。若节点以后可恢复，先审计旧 PID、退出收据和磁盘，再使用 `reproduce/collect-evidence.py` 取回证据；这不是内网验证的前置依赖。

## 迁移输入

| 输入 | 当前本地定位 / 要求 |
|---|---|
| E2B 源码 | 分支 `feat/virtio-pmem-dax-20261030`，首次交接时 HEAD `efc50ca94529b8161673ffd69dbccf7b4ee8610c`；新增测试与交接文件随后随本分支提交推送。迁移应取远程最新分支并核对这些文件，单独取首次交接的旧 HEAD 会缺少它们 |
| Firecracker 源码 | 本地相邻 `../firecracker-nashzhou`，HEAD `bd2800cac55ea4426f9d337be49d8be58cfaba16`；本轮 split fixture 工具另见下面脚本，不是该提交的生产内存布局 |
| 本轮补充文件 | `migration-test-sources.tar.gz`、其校验收据和 `prepared-source-manifest.json`；叠加在匹配提交的源码上，不能用此小包代替两个完整仓库 |
| 构建依赖 | Go 1.25.4；FC 仓库 `rust-toolchain.toml` 固定 Rust 1.85.0，agent03 musl 构建实际使用系统 Rust 1.86，两者不得混记；按内网最终发布工具链重新构建并验证。另需 Cargo.lock 对应依赖、目标 musl toolchain、目标/host libseccomp 与 Linux UAPI headers、离线缓存。Swagger 固定版本见 F08 |
| guest kernel | 原 ARM64 4 KiB Linux 6.6.60 配置与 hash 见步骤二既有 evidence。Image SHA256 `93e21a97a8e9fdec98462cfaaf14ad13b5721443dedabc8d7508bc0c9157ae79`。旧 6.1 guest 缺少 DAX/ZONE_DEVICE，不能替代新路径验收 |
| rootfs / initrd / 二进制 | 本地旧 evidence 大多只有 hash/收据，不包含大 RAM、rootfs 或 binary。agent03 上的 fixture、early-init/initrd、envd、FC、source-helper 不应假定可取回；在内网重建、记录新 hash 并重新验证 |
| 原始任务 | 路径与 SHA 见 [原 trace 输入清单](../20261030-step1-followup/factory/original-trace-asset-inventory.json)。需要额外迁移原始 trajectory、任务 environment、参考源码、Rust/Java 工具链和离线依赖；这些外部资产不在本次小源码包中 |

宿主/guest 架构、页大小、KVM/NBD、cgroup v2、swap、DAX、OverlayFS、文件系统 xattr/whiteout、磁盘预算均需重新记录。已有原生证据来自 Kunpeng 950、ARM64、4 KiB；若内网为 x86 或其他页大小，重新编译全部 guest/host 输入，并适配 `prepare-compat.py` 中 ARM64 汇编与地址假设后执行，不沿用 ARM64 验收结论。

`reproduce/run-*.py` 保留了 agent03 的绝对路径、toolchain/cache 路径、campaign 名称和网络 slot。内网使用前必须改为本地路径、全新输出目录与未占用 slot；不得直接执行其中的 SSH launcher。`new-source-sha256.json` 是早期补测输入记录，不是当前源码已原生通过的证明。最后新增的 seccomp 断言与历史版本脚本尚未原生验证；本地 Linux ARM64 交叉编译尝试因依赖下载超时失败，不记编译通过。

## 功能验证清单

以下均待内网验收。每项记录源码/输入 hash、完整命令及环境、退出码、原始 API/guest/host 日志、独立 oracle 与清理结果；失败尝试单独保留。基础回归通过后再叠加故障或压力，避免一次混入多个未验证变量。

| ID | 当前状态与入口 | 内网通过条件 |
|---|---|---|
| F01 | 所有旧测试需迁移回归；`TestPmemFactoryIntegration`、`TestPmemSharingProofIntegration`、`TestPmemMarathonIntegration` | 实际部署内核和发布二进制下，File/NBD × full_logical/tracked_delta；A→B→C/多代依赖恢复；同冻结边界导出/重建 RAM 与 native full dump size+hash 一致。重跑冷页、非零→零→非零、真实私页换出、balloon/FPR、block/net 暂停竞争；PMEM 不进入普通 RAM bitmap/export/balloon |
| F02 | 新增功能原生通过但证据未取齐；`TestPmemCompatibilityIntegration`、`prepare-compat.py` | exec 同 backing 同 PFN，并补 exec 不同 backing 对照；恢复后实际 readinto buffer 内容相同、两 VM PFN 不同。truncate/零扩展、hardlink、symlink、mode/uid/gid/xattr、目录 rename/EXDEV、whiteout/opaque、FD/mmap 后 copy-up/rename/unlink、sendfile/splice loopback+virtio TX、锁与解包安装，在 File/NBD × full/delta 的 B/C 恢复前后通过。补写放大/upper 增长核算 |
| F03 | 写尝试测试 exit 0，拒绝位置未核实；compat 的 `*-attack.json` | 单独记录 `/dev/mem` 打开、映射、实际 CPU store 是否到达，以及 raw `/dev/pmem0` 写的结果。权限拒绝只证明该入口拒绝；若未执行 store，补可控 guest helper 触发真实写入并验证 KVM 只读 slot。攻击者终态/可用性、观察 VM、host lower 全 hash、源模板四方证据齐全；不能把普通命令超时视为拒绝成功 |
| F04 | 通过但证据未取齐；`vsock_check.py`、`TestSharingRemoteShortReadIntegration` / `DelayIntegration` | 活跃 vsock 冻结、恢复连接重置/重连、无阻塞或重复完成；两真实 VM 同源 HTTP 短读/延迟，错误广播、停止 FC、终态和 drain/关闭均验证，未知状态 NBD 不复用 |
| F05 | 三 slot fixture 通过但证据未取齐；`multiregion_check.py`、`split_snapshot.rs` | 实际多个 KVM RAM slot，File/NBD × full/delta 的边界 dirty/零页/逻辑偏移/重建/native oracle/恢复一致。已有 fixture 将连续 GPA 拆成三段，没有地址洞；内网实际 RAM 布局若不同，增加该布局回归 |
| F06 | 未启动；`pressure_check.py` | 独立 cgroup 中两 VM 持续私写，full/delta 各三个冻结代；实际 swapped PTE 证据，完整 RAM oracle、两 VM 私写隔离、最终恢复、源 hash、cgroup/VM/NBD 清理均通过。记录 PSS/VmSwap/faults/PSI 和延迟原始样本。640 MiB max / 192 MiB high / 512 MiB swap 是原脚本诊断参数，需核对内网适用性；此测试不提供生产 p99/SLO 结论 |
| F07 | 只有 musl FC 编译通过；`reproduce/run-production.py`、`sharing_seccomp_integration_linux_test.go` | 重新构建静态 musl FC 和 `cargo build -p jailer`；实际生产 seccomp 下启动/保存/恢复/故障。`E2B_SHARING_REQUIRE_SECCOMP=1` 要求每个 API/VMM/vCPU 线程 `Seccomp=2` 且 filter 数大于继承基线。另补真正 jailer chroot/降权/所用 namespace/cgroup、设备与文件权限、PMEM rebind、退出清理；不能用直接启动 FC 代替 jailer 验收 |
| F08 | `swagger-01` 终态未知 | Go 1.25.4；`packages/shared` 下 `go generate ./pkg/fc`（仓库固定 `v0.33.2-0.20260129162912-5a56fc6f5a75`），审查生成差异，重跑 `go test ./pkg/fc/...` 和 orchestrator/API 合同及编译；不把未取到退出码记通过 |
| F09 | 脚本已准备未运行；`snapshot_version_check.py` | 实际旧 FC binary 冷启动并创建旧快照，新 FC 加载并执行、修改、保存新格式，新 FC 再恢复；旧 FC 明确拒绝有效新格式，缺失文件/权限错误不算格式拒绝。保留旧二进制来源/hash；补实际历史线上快照和旧 guest 默认路径回归 |
| F10 | `runtime-extra-02/history-and-race` 终态未知；`TestHistoryObjectDeletionRejectsUnreadAncestor` | race 重跑；运行期 pin/cache retire 与读取并发正确。祖先删除后未缓存范围与 cold restore 明确失败、不补零。**持久化 GC 保留未实现/未验收**：需实际引用图/保留规则阻止删除仍被已发布后代引用的祖先，再测多代快照发布/恢复/GC 并发；当前删除后失败测试不能代替它。实现前保持历史依赖保留，不启用在线祖先 GC |
| F11 | 真实云对象存储未运行；现有实际 provider 接口 + 内网配置 | 真实目标存储完成 upload、冷/warm restore、跨代依赖、版本/校验、取消/短读/断流、权限/配额/磁盘满及保留。S3 兼容服务与真实云 S3 分别记结果；当前 HTTP seam 与 Local provider 不能代替。`TestPmemMaterializeDiskFullIntegration` 已通过但需内网私有 tmpfs 回归，并补真实部署 cache/upper/上传暂存路径的写满终态 |
| F12 | 1052 条原工具完整执行未运行 | 补齐 `/app`、固定参考源码、Rust 1.93.0/Java/GCC、Cargo.lock/离线缓存、生成 goldens、background task/output/stop 生命周期及原 session 路径依赖；同初始状态做完整 B0/B1，逐工具状态/输出及最终文件 manifest 对照。保持原 call ID/参数/顺序；动态 PID/时间等事先定义语义比较。既有 5 工具 slice、1016 turns 合成 trace 均不能计为本项完成 |
| F13 | 生产集成与回退未完整验收 | 验证实际模板构建→PMEM lower/manifest 发布→冷启动→暂停上传→跨宿主恢复的服务链；目前手动 fixture 成功不代表模板构建服务自动转换已接入。测试功能默认关闭、旧模板/旧 guest 路径、开关回退和错误终态，记录哪些格式只能由新 FC 读取 |
| F14 | 新增证据未全部归档；清理终态不完整 | 内网每项保存源/输入 hash、原始收据和失败记录，离线重算 hash/PFN/依赖链；修改 hash、缺祖先、损坏布局等负对照必须拒绝。审计本轮 PID/namespace/NBD/cgroup/临时存储，仅清理自己资源。agent03 旧记录单独标为未知，不冒充已清理 |

已验证的功能事实应保留，新增内网结果另起记录。当前 R1 是预物化、不可变 DAX lower + 每 VM 私有 upper；直接 NBD-backed PMEM 的 R2 未实现，不将它混入本轮既定功能验收。

## 可复用执行入口

以下在 Linux 目标节点运行；先编译目标架构 guest-init/source-helper，并提供全新 `RUN` 目录。`FC`、`KERNEL`、`GUEST`、`SOURCE_HELPER`、`OLD_FC` 必须指向本轮已记录 hash 的文件。Go 集成测试还需步骤二文档中的显式 `E2B_SHARING_*` / `E2B_PMEM_*` 输入、兼容性 fixture 与未使用网络 slot。

```sh
# 受控双 VM 压力；脚本创建自己的 cgroup，不依赖完整平台。
PYTHONPATH=scripts/memory-lab/page-sharing python3 scripts/memory-lab/page-sharing/pressure_check.py \
  --firecracker "$FC" --kernel "$KERNEL" --guest-binary "$GUEST" \
  --nbd-helper "$SOURCE_HELPER" --output "$RUN"

# 历史 binary 实际创建快照后的迁移/拒绝。
PYTHONPATH=scripts/memory-lab/page-sharing python3 scripts/memory-lab/page-sharing/snapshot_version_check.py \
  --old-firecracker "$OLD_FC" --firecracker "$FC" --kernel "$KERNEL" \
  --guest-binary "$GUEST" --output "$RUN"
```

两个命令的 `RUN` 各自独立；不要直接串行使用同一目录。完整测试环境与现有测试方法见 [步骤二状态](../20261030-step2/step-2-status.md)、`reproduce/run-target.py`、`run-compat-inbound.py`、`run-runtime-extra.py`、`run-multiregion.py`、`run-production.py`。这些是复现入口，具体服务端 S3/jailer/完整原 trace 的 driver 尚需补齐。

## 端到端超分测试：功能通过后执行

- [ ] **固定实验配置。** 记录硬件、总物理内存、预留后的节点实验预算 M、CPU/NUMA、host/guest kernel/page size、swap/磁盘/网络/对象存储、FC/jailer/seccomp、模板/源码 hash。预先填写 SLO（成功率、p99、恢复时间等）、预热时间、稳态持续时间、并发阶梯和停止条件，所有对照一致；没有阈值时只报告测量，不宣布容量达标。
- [ ] **建立独立对照。** 现有生产路径；仅 RAM 共享；RAM + PMEM rootfs 共享；再叠加选定的冷页/balloon/FPR 回收策略。每次只改变所比较的机制，保持 workload、模板分布、CPU/内存预算和存储参数一致，分别描述后端 File/UFFD/NBD 的实际配置。基线不能消费优化路径专用的生命周期/等待/退出标记。
- [ ] **覆盖工作负载。** 同模板高复用与多模板低复用；读多与 COW/upper 写多；长等待/短突发、并发启动/恢复和 steady-state；多代快照/跨宿主恢复。真实业务/完整原工具 trace 与合成诊断分别报告。冷/warm cache 各有明确前置状态，不使用周期性全局 drop_caches 制造优势。
- [ ] **逐阶增加超分。** 同一节点预算下先测不超分基线，再增加 VM 并发与配置 RAM 总量；定义配置超分比 `Σ VM configured RAM / M`，同时报告实际工作集和物理占用。每阶均重复验证 guest 状态、私写隔离、快照/恢复与服务成功，不用仅能启动的 VM 数代替可持续容量。
- [ ] **保留资源成本。** 记录节点/实验 cgroup 物理内存时间序列与 DRAM byte-seconds，PSS/RSS/VmSwap、major/minor faults、PSI、swap I/O、NBD/对象读取、恢复/存储放大、CPU、磁盘容量及 I/O。区分源缓存、每 `/dev/nbdX` cache、PMEM lower 与 guest RAM；共享页只计一次，不能把进程 PSS 与包含同一页的 page cache 再相加。记录 pager/缓存/元数据成本，逻辑导出字节不是物理节省。
- [ ] **记录端到端结果。** 成功完成且满足 SLO 的任务数/秒（goodput）、错误/超时/OOM、create/restore/save 延迟与业务 p50/p95/p99、恢复后的首次访问延迟，以及最高满足相同 SLO 的持续并发。给出原始样本、绝对值和重复轮次离散程度；内存节省、吞吐、尾延迟与容量分别报告。
- [ ] **验证压力下的失败与恢复。** 在各策略边界内测试慢对象存储/断流、磁盘配额满、暂停/恢复与并发 I/O、节点服务重启后的状态恢复。重试不能隐藏错误或重复完成；GC 仅在 F10 的持久引用保留合同成立时开启。达到预定停止条件立即停止加压，仍保存失败和清理收据。
- [ ] **形成可审计结论。** 同并发点比较内存/延迟，同 SLO 比较可持续容量；不同并发的内存量不能直接宣称为同负载节省。离线从原始事件重算指标，明确哪些是实测、模型估算或未测；步骤一/二功能验收与超分收益分别签收。

## 新结果填写位置

内网使用新的运行目录和报告，例如 `deliveries/page-sharing/<实际日期>-intranet/`，保存 `environment.json`、源码/输入 hash、逐项退出与 PID/资源收据、原始事件/指标、离线 verifier 及结果。逐项填写 F01–F14 的状态、证据路径、剩余限制，再更新步骤一/二矩阵。当前所有内网检查框均未完成。
