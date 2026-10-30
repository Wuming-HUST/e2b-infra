# 步骤一遗留验收项

2026-10-30：用户决定“步骤一先这样，把需要测试的记录下来，开始步骤二”。这是允许进入步骤二的范围调整，不将尚未完成的验收改写为通过。步骤一原始证据保持不变，最新实现与结果见 ../20261030-tracked-delta/status.md。

| 待测项 | 已有证据 | 补测方法与通过条件 |
|---|---|---|
| 活跃 vsock 与暂停竞争 | reset/冻结/失败传播单测 | 真实 guest/host 双向流量期间冻结；RAM/队列/IRQ 同边界，恢复行为符合连接重置合同，无重复完成或阻塞 |
| 多真实 VM 远端故障停止链路 | HTTP range 延迟、断流、版本/校验/并发合同与失败广播 | 两 VM 同 source，注入延迟/断流；均观察终态，停止 FC 后 drain；不补零、不永久阻塞、不复用未知映射设备 |
| 多 VM 受控持续压力/换出 | clean 实际驱逐、真实 COW 单页换出及完整导出/增量恢复 | 独立 cgroup 内施压，确认实际 swap；每冻结点完整 RAM oracle、私写隔离、恢复正确；记录 PSS/VmSwap/faults/p99。旧双页判据未通过的回执不覆盖 |
| 完整原 trace B0/B1 | 原 trace 1052 工具中的 5 工具片段通过 | 补齐 /app、bash、Rust/Java/GCC/Cargo 固定版本与缓存、工具生命周期；同初始状态逐工具状态/输出与最终 manifest 对照，不更改 trace 或扩大超时掩盖失败 |
| 实际多 region KVM | 顺序/跨 region/边界/短读/溢出软件测试 | 实际多 region 布局逐块导出与 restore oracle；不得为制造地址洞混淆 RAM 逻辑偏移 |
| 真实对象存储 | 本地 HTTP 与 production storage 接口复用 | 固定对象版本，cold/warm 读取、校验/取消/断流与引用保留；无环境时保持待验 |
| production musl/seccomp 发布物 | GNU release、真实 KVM/Factory 通过 | 实际生产打包构建和过滤策略下启动/恢复/保存，不使用 unimplemented seccomp 代替 |
| go-swagger 生成复跑 | YAML/Go 模型同步，接口合同和真实 Factory 通过 | 准备固定 v0.33.2-0.20260129162912-5a56fc6f5a75 依赖，运行仓库生成命令、检查生成差异、重跑接口测试 |
| 持久化 GC 历史依赖 | header 历史解析、多代恢复、运行期 pin | GC 压力与对象删除竞态下保留所有仍被已发布快照引用的祖先；缺失依赖明确失败，不补零 |

agent03 上的轻量设备/网络/HTTP 故障与受控压力不要求先部署完整平台；完整 trace 需要补齐 guest 工具链，不能仅因 32 GiB 容器而判定无法执行。真实云存储及生产发布物需要对应依赖/配置。

步骤二验证不得把 pmem 窗口纳入普通 RAM dirty bitmap、导出或 balloon；保留步骤一 full_logical 与默认关闭的 tracked_delta 合同。
