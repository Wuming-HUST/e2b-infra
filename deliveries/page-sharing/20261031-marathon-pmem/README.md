# SWE-marathon trace 验证续跑记录

2026-10-31：可以复用 agent03 的 trace，但 PMEM/DAX 全量回放尚未执行。本次只读核对因第一跳 H-arch 的 SSH 连接超时而未进入 agent03；没有启动新 VM 或回放任务。当前阻塞是连接，不是 32 GiB 容量。

## 已有结果

本次重新读取了本地保存的 [legacy summary](../20261030-step2/evidence/synthetic-legacy-02/run/summary.json)、[输入配置](../20261030-step2/evidence/synthetic-legacy-02/run/config.json) 和 [命令收据](../20261030-step2/evidence/synthetic-legacy-02/test.json)：

- `synthetic-legacy-02`：退出 0，203.80 秒，1016 turns、571 次合成工具工作、3 次恢复，`verify=true`。
- trace SHA256：`683d7fcc17c4edbd56ec6977293d3ec527a0ed4edc9cb1aa8c0f9935a200dcc5`。
- 最终工作状态 SHA256：`aa44ebe9ce29d4563fabc1f42496ec16f6119b163b1a09d4b81df6f046dfcc62`。
- 使用新 FC、旧 Debian 6.1 guest、派生 r4 rootfs、1536 MiB RAM、2 vCPU、`time_scale=.001`；普通 UFFD 内存恢复，未启用 PMEM/DAX。

这是按轨迹驱动的合成内存/I/O 回放。571 次合成工具工作与 1052 条原始工具调用不是同一计数口径；不能据此声称原始任务执行兼容性已经通过，也不能用缩时结果作性能结论。

独立的 [共享证明](../20261031-sharing-proof/README.md) 已覆盖 File/NBD RAM × 完整/增量快照的共享、COW、PMEM/DAX 应用路径及 A→B→C 恢复，但没有执行上述全量合成 trace。

## 连通后实施与验收

1. 在 agent03 只读核对原 checkout 下 `scripts/memory-lab/{swe-marathon-trace.json,replay.py,mtool.c}` 和 `packages/orchestrator/{cmd/memory-lab,internal/memorylab}`，保存 hash；这些回放器源码当前本地分支不齐全，不能假定已有 PMEM 参数。
2. 在隔离副本中将现有回放协议接入生产 Factory/Pause/upload/restore 路径；保持 trace 和 replay 不变。使用已验证的 Linux 6.6.60 Image、PMEM early init、PFN namespace lower 和独立 NBD upper；不能直接用缺少 DAX 配置的旧 6.1 guest。保留原始 r4 镜像，必要时仅在新副本内更新与协议匹配的 mtool，并重新生成 cold-boot 运行态快照。
3. 先跑一台 VM：共享只读 NBD + MAP_PRIVATE RAM、PMEM/DAX lower、私有 NBD upper，1536 MiB/2 vCPU；完成现有三个边界的保存、销毁与恢复。依次覆盖 `full_logical`、`tracked_delta`，避免同时占用容器内存；使用新运行目录和专用网络，不操作其他实验进程。
4. 每轮必须同时满足：原 trace hash 不变、`replay_done`、完整 turns/tools 计数、3 次恢复后的工作状态校验、进程/文件状态保持、真实应用路径 `STATX_ATTR_DAX`、lower 完整 hash 不变、恢复绑定的 lower hash/GPA 正确。增量模式还须核对同冻结边界的完整逻辑 RAM oracle，并验证历史依赖解析。退出 0 本身不构成通过。
5. 最终工作状态与 legacy 比较前，确认相同 trace/replay/mtool 及确定性输入；如 payload 或 oracle 包含布局相关字段，先解释差异，不能直接忽略 hash 不同。共享 PFN/COW 仍以专项证明和必要的目标采样为准，回放完成不能替代物理共享证据。
6. 保存命令、源码/输入 hash、各代快照 metadata、恢复日志、完整 summary、原始事件、失败收据及清理审计。原始工具执行另做原任务环境验证；本轮不能将 D14 标记为全部通过。

本次连接失败的机器可读记录见 [connectivity.json](connectivity.json)。PMEM 回放适配、编译和目标执行均未在本轮完成；本文件是续跑验收清单。
