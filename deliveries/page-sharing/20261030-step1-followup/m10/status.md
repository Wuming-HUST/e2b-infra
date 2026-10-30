# M10 真实 KVM balloon/FPR 验证

2026-10-30。**M10 两条 discard 路径的独立目标 KVM 验证通过**；这不是编排器 `Sandbox.Pause` 集成验收。新增 fixture：`scripts/memory-lab/page-sharing/m10_guest.go`、`m10_check.py`；复现解释见同目录 `m10.md`。

| 最终 run | 实际事件 | 未写页 | guest 重用并重写页 | 保存与恢复 |
|---|---|---:|---:|---|
| `m10-balloon-source-matched` | guest virtio-balloon inflate→actual 96 MiB→deflate 0 | 10,239 | 10,239 | 通过 |
| `m10-fpr-source-matched` | guest FPR；balloon target/actual 始终 0 | 7,680 | 1,536 | 通过 |

两 run 均使用 192 MiB RAM、同配套 FC、共享只读 NBD、两个独立 FC VM。候选页来自 guest 分配并填 `0x6b` 的 40 MiB arena，GPA 由 guest pagemap 记录。必须同时观察父源非零、目标 VM 该范围匿名 remap、最终字节归零，不能将 API 成功当 discard 成功。再次分配 100 MiB 填 `0xa5`，筛选实际复用的 discard GPA，不能将其他新分配页当“discard 后重写”。

每种状态均完整导出 192 MiB，与同一冻结 checkpoint 的 FC native full dump 比较整份 SHA256；未写状态重复 native dump 保持稳定。两个 checkpoint 各再次恢复：未写页全零，重写页为 `0xa5`，运行中的 guest 验证整个 100 MiB arena SHA256。另一 VM 对应页保持父 `0x6b`，不可变源完整 SHA256 不变。跨 restore 的整份 RAM hash 不强求与保存前相同，FC 会更新 VMGenID；逐页值与恢复后的 guest hash 才是对应 oracle。

FPR 最终设备 metrics：left reporting count=33、freed=130,023,424 bytes、fails=0；right count=24、freed=98,566,144 bytes、fails=0。`freed` 为累计报告量，**不是唯一物理页节省量**。原始数值见 `m10-fpr-source-matched/metrics-summary.json` 和各 VM `metrics.jsonl`。

实际 guest 为 Debian `6.1.0-50-arm64` / 4 KiB；host 为 openEuler `6.6.0-95.0.0.99.oe2403sp2.aarch64` / 4 KiB。极简初始 initrd 不含 virtio 模块，首次 probe 明确记录无 driver。随后只从已有 `/home/coder/tide-repro/rootfs/lib/modules/6.1.0-50-arm64/kernel/drivers/virtio/` 复制精确匹配的 `virtio_mmio.ko`、`virtio_balloon.ko` 到本次新 initrd，通过 guest `finit_module` 加载；driver 实际绑定及 reporting metrics 证实功能。未改变原 guest 素材或 host 配置。guest `/proc/config.gz` 不存在，保留原始缺失回执，不声称取得完整 guest config。

最终两个 run 共 10 个 VM + 2 个 helper 均退出；guest VM 由 runner 对自己的 FC PID 发 SIGTERM，returncode=-15 为预期退出，NBD helper returncode=0 且原始回执为 `closed`。包括首次 probe、历史复验以及本轮最终同源复验在内的全部 62 个独立 PID 已只读确认 `/proc/PID` 不存在，见 `process-close-source-matched-receipt.json`。最终两 run 的 `cleanup.json` 均为 all_vm_exits_confirmed=true、quarantined=false、errors=[]。未操作其他 VM、NBD、全局 cache 或 swap。

`m10-build/source-matched-sha256.txt` 记录最终四个 runtime 源文件、guest、FC 和 kernel 的哈希；四个本地源文件均与目标执行版本逐字节相符。最终 FC SHA256 为 `8830ff065716e073b74bed06fe073356ffff839eef5fe6039f3905b2b5149349`，来自主线确认与本地 tracked Rust/Cargo 同源的重建。旧 `m10-*-final-fc` 使用的 9dc7 候选结果仍保留，但不作为最终同源交付。沿用 ARM 基线 snapshot v7，仅验证同配套 FC 生成/恢复；不据此声称旧 v6 跨版兼容。Go ARM64 guest 编译、Python语法检查、4 项失败清理合同（local 与 ARM64）、最终两模式实机运行通过。完整命令见 `commands-source-matched.sh`，ARM64 合同日志见 `m10-build/cleanup-contract-source-matched.log`。源码未经本 subagent 暂存或提交，由主 agent 统一集成。原始 state/API/serial/PID/指标/hash 均已拉回；192 MiB RAM 和 initrd 保留远端 `/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/m10-*/`，避免复制大文件进 Git。

失败清理合同覆盖单 VM close 失败但继续检查其他本次 VM、close 报错但确认退出后的安全 drain、构造未完成 PID 不能视为退出以及失败默认保留。未知映射保留 helper/source；runner 留存持续读取 stdout，保存 quarantine 回执，避免断管触发 helper 提前退出。合同只使用 Fake VM/helper，不操作 KVM、信号或 NBD。

当前证据覆盖 M10 的真实 balloon 与 FPR discard 后未写/重写、另一 VM/source 隔离以及保存/恢复；编排器完整调用链、原 trace、M11 设备完成与 pause 竞争不在本 fixture 范围内，不用该结果代替。
