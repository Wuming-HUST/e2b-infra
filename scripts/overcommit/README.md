# 无准入资源超分压测分支

基于 `mem-overcommit-baseline` 的 `177cb41e0`，分支为
`mem-overcommit-exp-unrestricted`。本分支 **默认** 启用
`E2B_UNRESTRICTED_STRESS=true`；不需要提高数据库配额或 LaunchDarkly 的 `R`。
所有压测服务使用同一模式。`false` 可恢复代码中的准入策略用于回归，但不会
撤销宿主 sysctl、systemd 或 Terraform 的变化；正式对照应固定两个分支的提交与配置。

## 实际移除的门槛

| 路径 | 压测行为 |
|---|---|
| BestOfK | 跳过 CPU 额度、驻留/承诺/待放置内存的 CanFit 和 TooManyStarting 过滤；保留评分用于选节点，不按分数拒绝唯一可用节点；K 至少为 1；标为 unhealthy 的节点仍尝试创建，显式 draining 节点除外 |
| API 团队并发 | 向内存/Redis reservation 传现有的 `-1` 无上限语义；保留同一 ID 的去重和结果等待 |
| API 模板规格/构建 | 不检查团队 vCPU/RAM 上限、产品 32-vCPU 上限、团队同时构建数；保留有效 VM 参数校验 |
| API 时长 | 取消团队 lifetime cap 与旧 refresh 接口的 1 小时上限；用户请求的 sandbox TTL 仍决定回收时间 |
| API HTTP | 不安装 16 MiB body 限制；读写请求无服务端固定超时，调用方可取消 |
| Orchestrator | 取消节点运行数限制、30 个并发启动信号量；覆盖 create、resume 和 checkpoint；保留独立 sandbox 生命周期的原子状态操作 |
| 启动耗时 | 取消 Create/Delete 的 300 秒服务端截止、envd 的总启动与单请求截止、网络 slot 分配的 500 ms 截止；进程退出、错误和调用方截止仍生效 |
| HTTP/TCP 代理 | 入站 sandbox 连接和出站 TCP firewall 连接计数保持采集，但不执行连接配额 |
| Guest 随机数设备 | 新建模板不设置原每 100 ms 仅 1 KiB 的 entropy token bucket；已有快照内的设备配置需要重建模板才会改变 |
| gRPC | 取消 1000 个并发 stream 的应用上限，使用协议可表示的上限 |
| NBD | `IndexAny` 让内核选择/按需创建设备，越过预建 `nbds_max`；不再等固定 pool 释放。记录实际返回 ID 和连接归属，从 sysfs 验证/创建 `/dev` block node；关闭旧连接不会清除/断开复用该编号的新连接 |
| NBD I/O | 取消 30 秒请求完成超时，保留断连检测与清理超时；每设备 socket 数是 I/O 并行度，不是 sandbox 总数配额 |
| GCP 入口 | 删除仓库中 API-key、API-IP、sandbox-host、sandbox-IP 四条 Cloud Armor throttle 资源；需要单独部署 Terraform 才影响现有入口 |
| 宿主虚拟承诺 | 两套 host-init 脚本设置 `vm.overcommit_memory=1`；已有节点使用下面的准备脚本 |

压测模式下 placement 不自动换节点重试失败的创建，HTTP 错误包含原始失败原因。
这样 ENOMEM、NBD、磁盘、FD 等错误不会被无限重试或统一的“无可用节点”掩盖。

Redis 的待创建记录不再因超过 90 秒而被清掉，避免慢启动导致重复创建。
正常成功/失败仍执行 finish/release。**如果 API 进程崩溃，先确认旧创建已结束，
再清理该实验遗留的 pending reservation**；不要让另一轮测试复用未清理的 ID。
这项取舍避免用任意固定启动期限限制本实验，不提供新的跨进程崩溃恢复协议。

## 在压测节点启用

1. 固定分支提交，按现有平台/存储配置构建 API、Orchestrator 和 client-proxy。
   使用 Mooncake 的部署仍需原有 build tags、库与构建步骤。
   日志中的 `E2B_UNRESTRICTED_STRESS=true` 证明 API/Orchestrator 已启用模式。
   用该模式重建实验模板，移除旧快照中保存的随机数设备限速；Firecracker 的
   entropy 配置只能在启动前设置，更新 Orchestrator 不会改写已有快照。
2. 在 **Linux 压测宿主** 执行 `sudo scripts/overcommit/prepare-host.sh --apply`。
   它记录旧 `vm.overcommit_memory`、设为 1，并在 NBD 未加载时加载模块。
   已加载模块不卸载、不重载，已有 `nbds_max=4096` 或更小值不妨碍动态扩展。
   保留 baseline 的 HugeTLB-off 配置和现有 cgroup v1/hybrid 布局。
3. 将 `systemd.conf` 合并到压测服务对应的 drop-in，按既有方式重启服务；
   同时检查父 slice、Nomad/Docker 的 CPU、memory、swap、pids 限制。
   父组限额会继承，仅修改子组不能取消父组限制。直接启动程序可用
   `sudo scripts/overcommit/launch.sh /path/to/orchestrator`；它将 FD 提升至
   当前内核 `fs.nr_open` 上限，清除该进程的 NPROC/AS/MEMLOCK 限额。
4. 对 API、Orchestrator、proxy 和代表性 Firecracker PID 分别运行：

   ```bash
   sudo python3 scripts/overcommit/preflight.py --pid "$ORCHESTRATOR_PID" > preflight.json
   ```

   保存结果并检查所有可见祖先 cgroup。对容器化部署，还要在宿主采集，容器内
   看不到宿主隐藏的祖先。脚本为只读；缺失字段标为 unavailable，不当作零。
5. 压测通过已取消节流的内网入口发起，检查外部 ingress/LB、SDK 的重试和请求
   deadline。仓库外的代理配置不会因更新 Go 二进制而改变。
6. 选择一个足够覆盖整轮加压、测量和清理的显式 sandbox TTL；旧的默认 TTL
   仍然存在，不能让前面创建的 sandbox 自动过期，抵消后面施加的压力。

没有无限的内核地址/设备/FD 空间。本分支移除 E2B 的容量策略门槛，真实系统
调用错误继续返回。以下不是内存 OOM，出现时应标记为本轮实验被其它资源截断：

- `EMFILE/ENFILE`、PID/线程、conntrack/端口、磁盘空间或 I/O 错误；记录实际
  上限与错误，调整压测宿主后复测。`ENOMEM` 自身也不等价于 OOM kill 证据。
- 网络地址池耗尽：当前默认两个 `/16` 网段，约 32766 个 slot；按计划规模
  配置现有 `SANDBOXES_HOST_NETWORK_CIDR` 和 `SANDBOXES_VRT_NETWORK_CIDR`，
  使用不与部署网络重叠、足够大的网段。不能删掉地址唯一性校验来绕过耗尽。
- kernel 不支持 NBD netlink、无权限创建设备、KVM/镜像/认证/CPU 架构不兼容。

队列、缓存和 NBD/prefetch worker 数继续作为实现参数保留，它们不限制已运行
sandbox 总数，其吞吐/延迟影响由实测体现。列表分页同样不是运行数量配额。

## 压测与结果口径

固定目标节点、CPU/内存规格、镜像、工作负载、swap/NUMA、存储和观测窗口。
按 N 阶梯增加 **实际同时存活并执行目标工作负载** 的 sandbox，记录：

- attempted / created / running / workload-completed、创建错误原文和每次尝试耗时；
- create、运行操作和 pause/resume 的 p50/p95/p99、吞吐与错误率；
- host MemAvailable、RSS/PSS、cgroup memory（共享内存避免重复相加）、swap、
  CPU、PSI、I/O、NBD 设备数、FD、pids；
- `/proc/vmstat` 的 `oom_kill` 增量、内核 OOM 日志、可用的 cgroup
  `memory.events`，以及被杀进程身份。区分宿主 OOM、父组 OOM 与 guest OOM。

在每个阶梯计算 `退化比例 = 本阶梯指标 / 低压力对照指标 - 1`。
SLA 采用实验前定义的绝对阈值，不临时以“未 OOM”替代 SLA。
首次 SLA 超限后继续记录更高阶梯，直到目标内存 OOM 或其它实际错误截断。
同时报告 **最后满足 SLA 的 N** 和 **首次内存 OOM 的 N**；两者可能不同。
发生其它错误/取消/超时的请求保留在分母中，缺失指标标 unknown。

比较 File/KSM/压缩等机制时，两组必须都叠加这一取消准入的公共改动，使用同一
配置。不要拿有准入 baseline 与无准入 treatment 的 sandbox 数量直接归因于机制。

## 验证记录（2026-09-09）

- macOS Go 1.26.8：相关 API 调度、资源规格、TTL、共享模式回归及 race 检查通过。
- Redis 8 独立容器：并发同 ID 去重、限额旧语义、失败释放、旧模式过期清理、
  压测模式下超过 90 秒的 live reservation 不被清理，均通过 race 检查。
- Linux ARM64 Go 1.25.4：启动 semaphore 绕过、NBD 动态设备池和关闭路径
  回归通过 race 检查。
- 本地隔离 Linux VM 内核 NBD 实测：`nbds_max=4`，同时连接 6 个设备
  `[0 1 2 3 4 5]`，逐设备不同内容写入/读取一致，所有拥有的连接成功断开；
  `go test -race` 通过。这证明越过预建设备数，不是 sandbox 密度结果。
- 动态 NBD 并发复用：4 个 worker 共执行 32 次创建、读写、关闭，race 检查通过。
  归属单测覆盖旧连接关闭/释放不能影响复用同一编号的新连接。回收按归属核验，
  不要求动态编号永远空闲；测试断言失败也继续清理其它连接。
  开发中一次旧 teardown 断言中止清理留下的 4 个连接已按原 owner ID 清除。
- `E2B_UNRESTRICTED_STRESS=false`：原固定池的 `TestPathDirect_Write` 在 Linux
  内核上通过 race 检查；测试结束后 sysfs 中没有遗留 NBD 连接 owner。
- Linux ARM64 Go 1.25.4：`go build ./packages/api/... ./packages/orchestrator/...
  ./packages/client-proxy/...` 全部通过（无 Mooncake 标签）；共享模式、API schema、
  资源规格、调度、Orchestrator/NBD 的最终针对性 race 回归通过。
- shell 语法、Python 编译检查和 `git diff --check` 通过；未运行 Terraform apply。
- 未部署内网目标宿主，未运行 Firecracker E2E、压力/OOM 或 SLA 容量实验。

Linux 内核小规模 NBD 回归可在隔离测试环境执行（预建设备数须不大于 16）：

```bash
E2B_NBD_KERNEL_TEST=1 go test -race -count=1 -v \
  ./packages/orchestrator/internal/sandbox/nbd -run TestStressDynamicNBDKernel
```

还原时切回固定 baseline 构建；恢复记录的 sysctl 与原服务 drop-in，重新部署
原入口策略。不会自动改动宿主 cgroup 模式或重启宿主。
