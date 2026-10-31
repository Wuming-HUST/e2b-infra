# SWE-marathon trace：PMEM/DAX + NBD RAM 验证

2026-10-31：agent03 上 `TestPmemMarathonIntegration` 的 `full_logical`、`tracked_delta` 两轮目标测试均通过；源码核对和 10 个 VM 的清理审计通过。原始证据包已在目标端生成，下载未完成，本地完整离线复核尚未执行。最新续跑已确认 H-arch 第一跳可达；第二跳依赖的 Coder 服务 `http://192.168.90.48:80` 从 H-arch 直接连接超时，workspace 查询也超时，见 `connectivity-resume.json`。不能将目标通过与离线复核混为一项。

## 本轮结果

| 模式 | 目标结果 | turns / 合成工具事件 / 调用 ID | 快照恢复边界 |
|---|---|---|---|
| `full_logical` | PASS；Go test 124.26 秒，进程收据 124.30 秒 | 1016 / 571 / 576 | turn 10、200、700 均通过 |
| `tracked_delta` | PASS；最终退出码已由目标收集器核对 | 1016 / 571 / 576 | turn 10、200、700 均通过 |

两轮使用同一份未修改 trace/replay、同一静态 mtool、1536 MiB RAM、2 vCPU、64 MiB 持续匿名工作集、16 MiB I/O corpus，工具瞬时分配最高 600 MiB；单 VM 顺序回放。`time_scale=.001`，此处时间仅记录测试耗时，不用于性能或容量结论。

最终工作状态 SHA256 均为 `aa44ebe9ce29d4563fabc1f42496ec16f6119b163b1a09d4b81df6f046dfcc62`，与已有 legacy 回放一致，且本地已按确定性工作负载规则独立计算得到相同值。

这是一份完整的 **trace 驱动合成内存/I/O 回放**，没有执行 LLM 或原始工具命令。571 次合成工具事件、576 个 Bash 调用 ID 与 1052 条原始工具调用不是同一口径；原始完整任务环境的执行兼容性仍待验证。

## 实际覆盖

- 生产 Factory 创建、Pause、upload、本地对象存储及 template cache restore；独立只读 PMEM/DAX lower、每 VM 私有 NBD upper、共享只读 NBD + MAP_PRIVATE RAM。旧运行态快照未被直接换 backing，先用新设备冷启动生成新快照。
- 每代读取 FC `/proc/PID/maps`，断言 RAM 实际为 leased `/dev/nbdX` 上的 `rw-p` 映射；在应用 OverlayFS 路径和实际 lower 检查 `STATX_ATTR_DAX`，读取真实 mmap GPA 并断言位于独立 PMEM 窗口。
- turn 10 写非零页、turn 200 改为全零、turn 700 再写非零，跨三次保存恢复校验匿名 RAM、相同 guest PID、boundary 文件、zero/rewrite 文件及最终状态；核对全部合成工具事件和调用 ID 顺序，必须出现唯一 `replay_done`。
- 每次冻结的完整逻辑 RAM 都与同一边界的 FC native full RAM 对比，大小固定 1536 MiB，排除 PMEM 窗口。增量模式在 full oracle 触页前抓候选页，核对生产候选导出，再通过生产 layered source 读取历史依赖，重建完整 RAM 并比较 hash。
- 原始 A RAM 模板、运行态 lower 和输入 lower 的完整 hash 不变。白化、opaque、权限、UID/GID、symlink、xattr 状态也由原有 PMEM fixture 检查跨代保留。
- 目标收集器核对 26 个 Go 文件、30 个 Firecracker 文件与本地预期 hash，无差异；两轮共 10 个 VM PID 均不再存活，专用网络无残留。

本轮未重复物理 PFN/COW 矩阵；同模板双 VM RAM/rootfs 物理共享、不同 backing 对照等证据见 [共享证明](../20261031-sharing-proof/README.md)。回放完成不能替代物理共享证据。

## 输入与复现

`inputs/` 保存从 agent03 取回的原始字节：

| 文件 | SHA256 |
|---|---|
| `swe-marathon-trace.json` | `683d7fcc17c4edbd56ec6977293d3ec527a0ed4edc9cb1aa8c0f9935a200dcc5` |
| `replay.py` | `3403029dffe5f00a226a6b74c7d0ff8ff423e7d3c48f5e5d77811e0421931add` |
| `mtool.c` | `a2a4425d7b26b5e5ad8779f572927f07f3b9fad97c05d787599eac38e89afa53` |

静态 mtool 沿用已校验的 `r4-tool-v1/mtool`（SHA256 `ad240e9ef888b431b48f14c1c4ac0e3bc16f7c5c519c03994b123d26bc32b8f5`），上传至私有 upper；不修改原始 r4 镜像。guest 使用已验证 Linux 6.6.60 Image 和 PMEM initrd，旧 Debian 6.1 guest 缺少 DAX 配置，不能用于新路径。

远端隔离根：`/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/pmem-dax-01`。

- `marathon-build-01`：原生编译退出 0，7.51 秒；测试源码提交 `35c20c28a`，Firecracker `bd2800cac`。
- `marathon-full-01`：driver PID `222887`；`marathon-tracked-01`：driver PID `223177`。均已结束，原运行目录不可复用。
- 证据包：`marathon-evidence-20261031.tgz`，131904 字节、75 个文件，SHA256 `3fbe41ed57369a05478c089fa87fe531a732e36f8eee99d5a9fd06631964ebc6`。不包含大型 RAM/rootfs/binary，原生 RAM oracle 保留在目标端。

新测试沿用 [PMEM 测试输入说明](../../../scripts/memory-lab/pmem-dax/README.md)，额外提供绝对路径 `E2B_MARATHON_TRACE`、`E2B_MARATHON_REPLAY`、`E2B_MARATHON_MTOOL`，以全新运行目录、未占用 slot 和每轮对应的 `E2B_SHARING_RAM_EXPORT_MODE` 执行 `sandbox.test -test.run '^TestPmemMarathonIntegration$' -test.v -test.timeout 17m`。目标命令、输入和 hash 由每轮 `inputs.json`、`input-sha256.json`、`test.json` 保存。

完整证据取回后可离线运行：

```sh
python3 -B scripts/memory-lab/pmem-dax/verify-marathon.py \
  deliveries/page-sharing/20261031-marathon-pmem
```

离线核对器检查原始逐条工具事件、各边界及最终状态的独立 hash、实际 NBD/DAX 收据、增量尺寸/候选数、源码一致性与清理。完整 RAM 的重建和 hash 比较由目标 Go 测试执行，大型 RAM 不在小型证据包内。

目标收集器的本轮执行源码保存在 `reproduce/collect-target.py`，可审阅退出码、源码 hash 和清理断言；该版本拒绝覆盖已存在的 archive，再次执行时需使用新的归档文件名。

中途 H-arch IPv4/IPv6 SSH 与 Tailscale ping 曾超时，未重启或结束远端任务。早期连接失败保留在 `connectivity.json`，本轮观测状态保留在 `observations.json`。步骤二的原始全工具执行、完整文件/I/O/隔离攻击矩阵、真实 S3、生产 seccomp/jailer、压力/GC 等仍未验收，见 [总矩阵](../20261030-step2/step-2-status.md)。

本次续跑未重启已完成测试，也未更改凭据或网络配置。本地 trace/replay/mtool.c hash 再次核对一致；恢复 H-arch 到上述 Coder 服务的连接后，继续取回原证据包、比对归档 SHA256，再运行完整离线核对器。
