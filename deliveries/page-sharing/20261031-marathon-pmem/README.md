# SWE-marathon trace 验证续跑记录

2026-10-31 续跑：已恢复连接并在 agent03 完成新增集成测试的原生编译，启动 PMEM/DAX + 共享 NBD MAP_PRIVATE RAM 的全量 trace。最近一次成功读取日志确认 初始模板恢复及实际 NBD/DAX 检查通过、turn 10 保存完成并进入恢复；随后 H-arch 的 IPv4/IPv6 SSH 与 Tailscale ping 均超时，最终结果未知。tracked_delta 轮尚未启动，不能据此宣布验收。此前未启动任务的连接失败记录保留在 `connectivity.json`。

## 本轮实现与目标任务

- 新增 `TestPmemMarathonIntegration`，复用生产 Factory/Pause/upload/cache restore 和已有冻结 RAM oracle；1536 MiB RAM、2 vCPU、单 VM 顺序运行。测试核对原 trace hash、全部调用 ID 顺序、turn 10/200/700 三次恢复、相同 PID、文件状态、最终状态，以及每代实际 NBD MAP_PRIVATE 和应用路径 DAX/GPA。
- `inputs/` 保存本次从 agent03 取回的原始 trace、replay、mtool.c；字节未修改。测试把同一静态 mtool 上传至私有 upper，使用已验证 6.6.60 kernel 与 PFN lower，无须改原始 r4 模板。
- `full_logical` 比较同冻结边界生产导出与 FC native full RAM；`tracked_delta` 在 native full 触页前抓候选页，再比较导出和重建后的完整逻辑 RAM，包含历史依赖读取。
- 离线核对器 `scripts/memory-lab/pmem-dax/verify-marathon.py` 可核对完整交付包。它独立按工作负载规则计算各边界及最终匿名状态 hash，并核对原始逐条工具事件、DAX/NBD 收据、目标清理与源码一致性；当前证据尚未取齐，不能运行完整包验收。

远端隔离根：`/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/pmem-dax-01`。

| 项目 | 本轮已确认状态 |
|---|---|
| `marathon-build-01` | 原生 `go test -p 2 -c ./internal/sandbox`，退出 0，7.51 秒 |
| `marathon-full-01` | driver PID `222887`，已启动；最后确认 turn 10 保存完成并进入恢复，最终 `test.json` 未取回 |
| `marathon-tracked-01` | 尚未启动；启动器只在 full 的 `test.json.exit == 0` 后运行，已有目录则拒绝重复启动 |

断线后先读取 `marathon-full-01/{driver.json,test.json,test.log,driver.log}` 及当前进程，不重新启动原目录，不因 SSH 超时结束或重启远端 VM。测试本身设置 17 分钟超时，内部 context 为 15 分钟；保留失败收据与清理结果。当前问题是第一跳连接，不是容器容量。

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

此前连接失败见 [connectivity.json](connectivity.json)。本轮适配及原生编译已完成，完整回放已启动；最终收据、增量回放、清理审计及完整离线核对仍待完成。
