# tracked_delta 实施与 agent03 验证交付

本轮完成 File / 共享只读 NBD + MAP_PRIVATE 的候选页增量 RAM 保存与历史依赖恢复，作为独立实验开关交付。默认仍为 full_logical。下列通过项是本轮源码及二进制的实测结果，不代表原步骤一所有验收项或生产部署已完成；未开展步骤二 pmem/DAX。

## 代码与使用

两个仓库均新建分支 `feat/tracked-ram-delta-20261030`：

| 仓库 | 基线 | 实现提交 |
|---|---|---|
| e2b-infra-nashzhou | ca1fc09b4 | 07ba5a4de |
| firecracker-nashzhou | a977afa86d0da2ad96216b0d9daf6d72428015d1 | 87112f6e0 |

同目录两个 format-patch 仅包含本轮实现，必须先具备所列基线（尤其先前的共享 NBD、完整导出与设备冻结修复），不能直接应用到原版 upstream。完整运行说明：[tracked-delta.md](../../../scripts/memory-lab/page-sharing/tracked-delta.md)。

配置 `RAM_EXPORT_MODE=tracked_delta`；要求配套 FC、File 后端、非 hugepage、host/snapshot 4 KiB、已知父逻辑 RAM。冷启动完整导出；其他不符合条件的路径保留原路线。CLI 使用 `--kvm-dirty-bitmap --dirty-export-policy bitmap`，load 开启 `enable_diff_snapshots`。不依赖 HDBSS/WP_ASYNC。

KVM CPU dirty、VMM 用户态设备写及 balloon/FPR discard 一并追踪，逐 slot 收集立即累计到 ledger；失败重试不丢已收集位。discard 修改前先标记，后续失败也保留。位图不按 present/mincore 过滤。Pause 成功完成 state-only 冻结后才允许收集；恢复/重新运行会撤销可捕获状态。只读取候选页，零内容也按 Dirty 保存，其余继承父 header，Empty 不推断。

version 2 metadata 区分载荷 SHA256/长度与完整逻辑 RAM 摘要；metadata 最后发布。恢复利用合并 header 的 BuildID/offset 解析历史对象，不扫描全 RAM 来生成增量。历史对象必须保留；缺失或读取失败报错，不能补零。结构/摘要合同检查不等于预先校验所有尚未读取的远程 RAM 内容。原 version 1 full_logical/uffd_delta 可读；旧 reader 拒绝 version 2。关闭开关只改变后续保存，回退旧 reader 前须重新导出兼容完整快照。

首版沿用 Pause 后关闭 FC 的生命周期，不实现同一运行实例保存后清位、切换父代并继续运行。M11 多冻结点是相对同一初始 backing 的累计候选诊断。

## 实测环境与结果

隔离目录：`agent03.coder:/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01`，经 H-arch 两跳。AArch64 / Kunpeng 950，容器 cgroup 限制 32 GiB，host 6.6 / guest 6.1，均 4 KiB 页。Go 1.25.4、Rust 1.85.0。使用独立 source/build/output，不替换旧实验二进制；既有依赖缓存复用。

| 检查 | 最终结果 | 原始证据 |
|---|---|---|
| Go 合同与回归 | 90 个顶层测试 PASS；包括新合同、空增量、捕获失败、metadata 发布、12 代继承与既有共享生命周期 | receipts/final-validation/go-tests.log |
| Go race / orchestrator 编译 / Factory test 编译 | 均 exit 0 | go-race.log、go-exit.tsv |
| 完整 header / metadata 测试 | 22 个顶层测试 PASS；零测试包不计数 | headers-full.log、headers-full-exit.json |
| FC dirty ledger / discard / IOVec | 4 / 5 / 5 项通过 | fc-bitmap.log、fc-discard.log、fc-iovec.log |
| FC API parser | 36 项通过 | api-complete.log、api-complete-exit.json |
| FC 设备冻结 / block prepare-save | 3 / 1 项通过 | freeze-regressions.log、prepare-save.log |
| FC release 编译 / rustfmt check | 均 exit 0 | fc-build.log、fmt-check-exit.json |
| 真实 CPU / cold / zero / swap / A→B→C | 全部通过；确有一页非零私有 COW 被换出后再捕获、重建与恢复 | native-final/result.json、swap.json |
| 真实 Factory B0 普通文件 / B1 共享 NBD | 创建→恢复→Pause→上传→缓存与历史映射→恢复通过；四个增量完整 RAM 与同冻结点 FC native 一致 | factory-tracked-final/*checkpoint.json、result.json |
| 真实 balloon 与 FPR | 非零父页 discard 未重写、重用重写、同源另一 VM 隔离与恢复均通过 | m10-*-tracked-final/result.json |
| 真实 Async block / 持续 TAP RX | B0/B1 各 3 次冻结与恢复进展通过；候选重建匹配 native，对照 RAM/disk 三次稳定 | m11-tracked-final/result.json、各冻结点回执 |

Factory 每次 RAM 256 MiB；B0 的 B/C 载荷分别 11,948,032 / 6,139,904 bytes，B1 为 13,541,376 / 6,447,104 bytes。实际生产捕获载荷摘要与完整对照前选出的候选载荷一致；经 production header/cache 历史对象解析后，整个 RAM SHA256 与同冻结点 native full 一致。不是只校验几个 marker，也不是先读完整 RAM 再挑页。

独立 128 MiB CPU fixture：B 候选 4 KiB，C 候选 483,328 bytes。候选捕获约 1.0 / 2.1 ms，完整 oracle 各约 270 ms。此数字只描述小 fixture 的读取阶段，不是生产总 Pause 延迟或通用性能承诺。

balloon 实际非零候选 10,240 页中 discard 10,239 页，重写同样 10,239 页；FPR discard 7,680 页、重写其中 1,536 页。discard 的大范围零值仍保存字节，保证正确性，首版没有零页压缩。M11 持续发送 20,137 个包；不把发包量作为 guest 收到全部包的证明。

源码核对：本地提交与远端 28 个 E2B 变更文件、352 个 FC 源码/配置文件逐文件 SHA256 相同；见 source-sha256.json。实际二进制摘要见 input-sha256.json。最终 audit 记录本轮 30 个 VM PID 均不存在、namespace 列表为空，M10/M11 cleanup 成功；原只读 rootfs SHA256 仍为 `c5779c5de7bcf5e5d98e0bfc36b857166b47ee7f7d810f4c23c6260f191fa99c`。未修改全局 swap、cache 或压力配置。

## 失败保留与待验边界

attempt-01 保留初始编译/测试错误，修复后重新编译、重新执行。attempt-02 是最终重构前的通过记录；本报告以 final-validation 与 *-final 为准。初次 API 测试 exit 101 源于离线缓存缺少测试依赖；后来仅补入与 Cargo.lock SHA256 一致的公共 crate，最终 api-complete 36 项通过，旧失败日志不覆盖。

固定版本 go-swagger 不在离线缓存，生成命令失败保留在 swagger-generate.log。YAML 与 Go DirtyMemory 模型已同步，编译、JSON 合同和真实 Factory 接口通过；不声称生成器复跑通过。

尚未验证：完整原 trace（这里只回放同一原 trace 的五工具片段）、完整生产部署/E2E、真实活跃 vsock 流量、production musl/seccomp 发布产物、大规模多租户压力、生产持久化 GC 的历史依赖保留策略。已有设备冻结单测不替代真实 vsock 流量验收。4 KiB 之外及 hugepage 增量不在本轮启用范围。以上项目保留为内网验收项，不将本轮结果提升为步骤一全部验收或步骤二开工依据。

## 证据布局与校验

`receipts/` 保存原始日志、命令、PID/退出码、候选/冻结/恢复结果、历史失败记录与 SHA256；大 RAM/disk/rootfs/二进制保留在 agent03，不加入 Git，路径/大小/摘要列于 retained-large-artifacts.json。`launchers/` 保存本轮启动、补充回归、审计及收集脚本，不自动重启未知状态远程作业。

`receipt-sha256.json` 校验原始回执；`SHA256SUMS` 校验本次本地交付所有文件。patch 的基线见上表，带入内网时须将此前基线一并准备。

取回归档 SHA256 与 453 个回执文件摘要均已验证。离线复核执行 `python3 deliveries/page-sharing/20261030-tracked-delta/verify-delivery.py`，已通过交付摘要、源码匹配、最终退出码及 21 组独立重算的 RAM oracle 摘要核对；它只复核保留证据，不重跑 VM。
