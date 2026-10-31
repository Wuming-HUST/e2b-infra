# agent03 迁移前补测

用户要求：完成所有能够在 agent03.coder 上执行的测试，降低迁入内网后的问题风险。只将原始收据和独立 oracle 支持的子项记为通过；失败尝试保留。

**2026-10-31 交接决定：用户判断 agent03 可能无法恢复，转入内网先验证功能、再做端到端超分。本轮不再连接或安排 agent03 测试。未完成项、复现入口、迁移输入和超分验收条件已冻结到 [内网交接清单](intranet-handoff.md)。远端旧进程实际终态仍未知。**

## 功能验收清单（交接时冻结）

- [ ] PMEM 执行页共享、read buffer 私有、truncate/hardlink/目录 rename/打开 FD+mmap 的 unlink/rename、sendfile/splice、锁与解包安装；File/NBD × full/delta，跨 B/C 恢复
- [ ] 真实 guest CPU/raw-block 非法写拒绝，观察 VM 和源完整 hash 不变
- [ ] 双真实 VM 同源 HTTP 延迟/断流，失败广播、停止后 drain、未知设备不复用
- [ ] 实际多 RAM region KVM，逻辑顺序导出及 full/delta 恢复 oracle
- [ ] 多 VM 独立 cgroup 持续压力/真实换出；逐冻结点 RAM oracle、PSS/VmSwap/faults/p99
- [ ] 历史对象删除竞态、运行期 pin、缺失依赖拒绝；明确持久化 GC 保留边界
- [ ] 生产 musl/seccomp/jailer 构建及启动/保存/恢复
- [ ] 固定 go-swagger 生成及 API 回归
- [ ] 历史 FC 快照、新旧 binary 的格式兼容拒绝
- [ ] 原始 1052 工具 trace 的依赖与生命周期补齐及真实回放
- [ ] 真实对象存储环境检查；节点可启动 S3 兼容服务与真实云 S3 分别记录
- [ ] 取回全部小型原始证据、源码/输入 hash、清理审计、离线复核与损坏对照
- [x] 更新步骤一余项及步骤二 D01–D14 验收矩阵，保留“原生通过但待归档”与未验收边界
- [x] 记录内网 F01–F14 功能交接清单与后续端到端超分测试条件

## 已观察的目标节点结果，待取回证据与离线复核

以下 exit/耗时已经通过 SSH 读到目标节点收据；尚未把远端原始证据全部归档到本目录，因此上方最终验收清单暂不勾选。

| 场景 | 远端 campaign | 已读到的结果与边界 |
|---|---|---|
| 完整兼容性，File/NBD × full/delta | `remaining-20261031-05` | full_logical 171.48 秒、tracked_delta 170.43 秒，均 exit 0。exec 同 PFN、恢复后实际 readinto 的 buffer 不同 PFN；truncate/零扩展、hardlink、目录 rename 与普通 OverlayFS 基线、FD/mmap 的 copy-up/rename/unlink、sendfile/splice 本地及 virtio TCP、锁、解包安装、B/C 恢复均通过 |
| guest CPU/raw-block 写尝试 | 同上 | 测试要求拒绝、观察 VM 状态相同、整个 lower SHA256 相同。必须取回 `*-attack.json` 确认具体拒绝位置；在此之前不把 guest 权限拒绝扩大为已执行 CPU store 的证明 |
| 活跃 vsock | `remaining-20261031-01/vsock-01` | exit 0，12.44 秒；File/NBD 各三个冻结点、native RAM oracle、恢复后重连/echo，通过；旧连接按合同重置 |
| 物化器真实 ENOSPC | `remaining-20261031-01/disk-full-01` | exit 0，0.36 秒；私有 mount namespace 的 16 MiB tmpfs，失败时不发布最终/部分镜像 |
| 七包 race | `remaining-20261031-01/race-01` | exit 0；pmem/fc/template/memorybacking/nbd/metadata/early-init |
| 两真实 VM 的 HTTP 短读/延迟 | `runtime-extra-02/http-short`、`http-delay` | exit 0，19.723/20.434 秒；真实 Factory/NBD，失败广播、进程终止、错误终态、停止后 drain。只读 backing 的传输 seam 注入实际 HTTP 故障 |
| 三个真实 RAM slot | `multiregion-02` | exit 0，3.615 秒；File/NBD × full/delta，跨 region 的 marker/dirty/native RAM hash/恢复均通过。fixture 工具将一个连续 GPA 区间拆成三个真实 KVM slot，不制造地址洞；不代表生产默认 cold-boot 布局 |
| 静态 musl release FC | `production-build-05` | exit 0，74.817 秒。只证明构建，实际 seccomp 线程状态、Factory 保存恢复、jailer 还没验证 |

兼容性失败尝试 `remaining-20261031-01..04` 保留。先纠正恢复前 buffer 被允许共享的错误 oracle，再改为 host 发起 TCP 连接以满足原有防火墙规则。最终定位到测试对 OverlayFS FD 语义的错误预期：copy-up 后已打开 FD 重新解析 upper，而已有 mmap 仍保留 lower。新增同 guest kernel、非 DAX tmpfs OverlayFS 基线后通过；没有修改生产 OverlayFS 行为。

`runtime-extra-02/history-and-race` 和 `swagger-01` 的最终状态尚未读到，不记为通过。历史删除测试只验证“祖先被错误删除时明确失败”，不能证明持久化 GC 保留机制已经实现。

## agent03 最后状态与可能的证据补取

最后一次成功读取 `df`：`/home/coder` 所在 2.8 TiB 文件系统为 100%，可用 0。`pressure-01` 尚未启动，避免把宿主盘满误当成受控压力结果。之后连接出现 Coder 服务端数据库 DNS 错误：`lookup database on 127.0.0.11:53: no such host`，随后还有 SSH banner 超时。没有进入节点的 SSH 失败不意味着远端任务退出；不据此重启任何测试。新增连接尝试原文见 `connectivity-attempts.jsonl`。

仅当旧节点以后可恢复、需要补取证据时：

1. 只读核对进程、退出收据、磁盘和本轮目录大小，确认哪些 campaign 已停止。
2. 用 `reproduce/collect-evidence.py` 流式取回小证据；已停止 campaign 可加 `--all-artifacts` 无损归档大文件。脚本逐文件校验 SHA256，不生成远端临时归档，不删除源文件。
3. 只有本地归档逐字节验证通过后，才释放已归档的本轮大文件；保留失败证据和仍需使用的 fixture/输入。不得清理其他任务。
4. 不自动重启旧测试。持续压力、musl Factory/jailer、历史版本兼容、Swagger、对象存储及原 trace 的剩余工作已转入内网交接。

`scripts/memory-lab/page-sharing/snapshot_version_check.py` 已准备旧 binary 冷启动创建快照→新 binary 恢复/修改/保存→新格式恢复→旧 binary 拒绝的新旧版本测试；目前只完成本地语法检查，尚未原生执行。归档脚本的本地有效文件/损坏 hash 对照通过，不代替目标实验的离线复核。

当前远端根：`/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/pmem-dax-01`。不覆盖已有源码、输入、日志、收据和 PID。
