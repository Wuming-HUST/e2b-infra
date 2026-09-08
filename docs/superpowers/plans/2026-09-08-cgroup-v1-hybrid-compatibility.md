# cgroup v1/hybrid 兼容修复与验证记录

日期：2026-09-08。适用范围：`mem-overcommit-baseline` 和全部 8 个内存超分实验分支。

## 决策与故障原因

保留内网服务器现有的 cgroup v1/hybrid 布局，不修改内核启动参数、控制器挂载、
Nomad/Docker 配置，也不要求重启宿主机。本次只修复 Orchestrator 对已有可选
cgroup accounting 路径的接线；没有实现新的 v1 每沙箱资源控制器。

旧代码的 `NewManager()` 两个分支均返回非空 manager；`Initialize()` 检测到
v1 后返回成功，但 `main.go` 仍把 manager 传给 sandbox factory，且打印
accounting enabled。fresh create 和 resume 随后都调用 `Create()`，尝试在
只读 `/sys/fs/cgroup` tmpfs 下创建 `e2b/sbx-<id>`，触发 EROFS。配置了 manager
就必须成功创建 cgroup 的规则把该错误转换为创建失败。

新版按 `/sys/fs/cgroup` 的 `statfs` 文件系统类型决定是否创建 manager：

| 条件 | 启动与创建行为 |
|---|---|
| cgroup2 | 返回 v2 manager；初始化、控制器启用、Create 或进程放置失败仍报错，不降级 |
| tmpfs（典型 v1/hybrid 根）或 cgroup v1 | 返回真正的 nil interface；启动打印兼容模式警告；factory 返回 `NoCgroupFD`，子进程继承父进程的既有 cgroup |
| 探测失败或未知文件系统 | 返回错误；启动失败，保留原始错误原因 |

hybrid 中存在 `/sys/fs/cgroup/unified` 并不等价于根挂载和 memory/cpu 控制器
都可作为 v2 使用。本修复不把这个子挂载传给 `CLONE_INTO_CGROUP`，也不在
进程启动后写 `tasks`/`cgroup.procs` 冒充原子放置。

`main.go` 只对非空 manager 初始化并打印 enabled。`Initialize()` 不再自行
跳过检测失败。原有 v2 Create 的 fail-closed 行为、fresh create/resume 的
nil-handle 清理保护、`NoCgroupFD` 启动路径继续生效。Firecracker 无需修改。

## 能力与实验口径

- v1/hybrid 兼容模式不会取消父 cgroup 已有的限制；也不新增每沙箱 v1/v2
  accounting、原子 cgroup 放置或内存/交换限额。历史基线本身也没有实现
  每沙箱 `memory.high`/`memory.max` 保护，本次不宣称补齐了这些能力。
- 现有进程级 CPU 时间、RSS/VMS 采集继续运行；cgroup 专用统计字段在当前
  schema 中保持默认 0，必须标记为“不可用”，不能解读成实际占用为 0。
  共享页实验要另外采集 PSS/Shared_Clean；RSS 不能代替实际物理共享收益。
- API 的 memory admission 使用主机指标、已承诺与待放置内存，不依赖这些
  每沙箱 cgroup 统计字段；仍需核对部署的 BestOfK `CanFit` 配置与主机采集。
- **ETMem 当前实现不适用于 v1/hybrid**：`host-etmem.sh` 明确依赖 v2 的
  `memory.swap.max` 与 cgroup 身份核验。保持拒绝，不删除其 swap 安全边界。
  若后续必须在 v1 测试，另行实现 memory/memsw 语义、控制器路径、PID 迁移
  与回滚验证，再纳入实验。
- File page cache、MGLRU、zswap、KSM、guest DAMON/FPR、guest zram、snapshot
  compression 的机制不因本次 nil-manager 修复而自动失效，但仍受各分支
  原有内核、存储、FPR 正确性、隔离和 p99 门槛约束；不能把“可创建”当作
  “已验证超分收益”。

历史结果不删除，也不改写原先的 commit、cgroup 模式与测量环境。新增一个
固定为 v1/hybrid 的实验环境清单，在 control 和 treatment 上都叠加同一修复。
补做创建/恢复 smoke 与短基线校准；只有核验工作负载、指标口径及 SLO 等价后
才沿用可比较的历史对照。v1 与 v2 的 memcg 字段不混合，修复前后的提交也需
分别标记。这样不要求因 cgroup 迁移而重建全部实验环境。

## 本地检查与 ARM 预验证

本地仅做源码、分支对象一致性检查、`gofmt` 和 `git diff --check`，没有执行
测试或编译。修复前 9 个分支的 `main.go`、manager、manager tests 和
create-cgroup tests 的 Git blob 完全相同，适合共同应用兼容补丁。

远端验证限定在已授权的 `coder ssh agent03/agent03`，所有文件均位于：

```text
/home/coder/projects/yangshengxin/e2b/src/cgroup-v1-compat-20260908
/home/coder/projects/yangshengxin/e2b/logs/2026-09-08-cgroup-v1-compat
/home/coder/projects/yangshengxin/e2b/artifacts/cgroup-v1-compat-20260908
```

该容器于本次只读核对仍为 ARM64、openEuler Linux
`6.6.0-95.0.0.99.oe2403sp2.aarch64`，uid 1000，cgroup2；因此它不是用户的
v1/hybrid 内网部署环境。v1/hybrid 模式用注入的 statfs 结果覆盖，不把单测
记为真实 v1 服务器上线验证。Go 工具链为已有的独立 Go 1.25.4 linux/arm64。

| 验证 | 当前状态 | 范围 |
|---|---|---|
| 模式回归：v2、只读 tmpfs/v1、探测 EACCES/ENOENT/EIO、未知 fs | 通过，7 个子用例 | 无需挂载权限；v2 测试同时模拟只读标志，仍返回 manager |
| nil manager 创建、configured manager fail-closed、无 FD 启动 | 通过 | 纯用户态测试，不启动 VM |
| cgroup/fc/sandbox 包测试、Orchestrator ARM build | 通过 | 8 个需要 root 的真实 cgroup 用例跳过；不带 Mooncake 标签；不替代部署构建 |
| v1/hybrid 内网创建/恢复、父 cgroup 继承、指标连续性 | TODO P0 | 需要内网已有 E2B 部署 |
| 可写 v2 的真实进程放置、只读/未委派 v2 失败保护 | TODO P1 | 需要独立可控环境；不在共享容器改挂载 |

本次远端命令整体退出码为 0。`source.log` 记录工具链、内核、基底提交
`2425bbcfdcf7e6056bfe599c568070a425b5626c` 加修复文件的 SHA256；`tests.log`
记录通过和跳过的全部用例，`build.log` 记录 `go build ./...`。独立产物
`orchestrator-no-mooncake` 已生成但未部署，其 SHA256 为：

```text
5e0253fbd05a806d51c5e41cf24f1b4b6f0d809eadeeef6ba9a4160e8c7c042a
```

源码校验值（各分支共同修复文件；与远端 source.log 对应）：

```text
721dbd037fdd018a8b2306d3edb9ceb681c9e8929117b7396935a264daf64ba1  main.go
3073e0bb7948be0ba41ec857d4217752c8eac74cd0002a47fcd72d383ea28084  internal/sandbox/cgroup/manager.go
7e160f566cb299a323ca790352d4604a472401b51d4d28e423420104ae071ece  internal/sandbox/cgroup/manager_linux.go
ec8b1a01e378e62cf94eb5eab19eb41cc3110075521e8316db91665249377c44  internal/sandbox/cgroup/manager_linux_test.go
38da8dd4957d5535469c1447da84dc5aa8db45ceb835c9de0286d6c38bdf796d  internal/sandbox/cgroup/manager_other.go
729d5bc7f2a757f86bfd95a840d35c31cb47ffa9db2ccb003b0df7e6ddfdf7e2  internal/sandbox/cgroup/manager_test.go
59e7693141c43b9243ee0f9438c3d2e0798a45b705ee36d44c3f06d154b73847  internal/sandbox/create_cgroup_test.go
```

共同文件的测试通过不等价于重新验证 8 种 treatment 的运行机制；原分支的
已验证/待验证清单继续保留，内网 P0 项完成前不宣称生产故障已消除。

复现远端用户态验证（以实际修复提交或记录的源文件 SHA256 为准）：

```bash
export PATH=/home/coder/projects/yangshengxin/e2b/toolchains/go1.25.4/bin:$PATH
export GOTOOLCHAIN=local GOPROXY=https://goproxy.cn,direct
export GOCACHE=/home/coder/projects/yangshengxin/e2b/cache/go-build
export GOMODCACHE=/home/coder/projects/yangshengxin/e2b/cache/go-mod
export TMPDIR=/home/coder/projects/yangshengxin/e2b/cache/tmp
cd /home/coder/projects/yangshengxin/e2b/src/cgroup-v1-compat-20260908/packages/orchestrator
go test -count=1 -v ./internal/sandbox/cgroup ./internal/sandbox/fc ./internal/sandbox
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build ./...
```

## 内网剩余 TODO（按优先级）

1. **P0：部署构建与一次可回退更新。** 固定修复后的分支 SHA，复用内网原有
   build tags、Mooncake 库及发布方法构建 Orchestrator，保留上一二进制与配置。
   ARM 容器的 no-Mooncake 构建不能直接替代使用 Mooncake 的部署。
   更新 Orchestrator 服务即可，不变更宿主机 cgroup 布局。按现有服务机制
   排空/滚动更新，避免直接中断运行中的沙箱。
2. **P0：fresh create + pause/resume + delete。** 保存启动兼容模式警告；
   确认不再出现 `/sys/fs/cgroup/e2b` EROFS、`CLONE_INTO_CGROUP` 错误或
   新的创建 500。使用已知正常模板创建一个沙箱，执行读写校验；暂停/恢复后
   再校验同一内容，最后删除并检查进程/网络/磁盘清理。
3. **P0：继承与指标。** 保存以下只读输出，确认 Firecracker 与其启动父进程
   （含 wrapper，如有）各 v1 控制器路径的继承关系符合原部署设计；如果由
   外部管理器迁移，记录该既有策略。核对 CPU、RSS/VMS、host memory admission
   和原有监控连续性，屏蔽不可用的 per-sandbox cgroup 字段。

   ```bash
   findmnt -R -o TARGET,FSTYPE,OPTIONS /sys/fs/cgroup
   cat /proc/self/cgroup
   cat /proc/$ORCHESTRATOR_PID/cgroup
   cat /proc/$FIRECRACKER_PID/cgroup
   cat /proc/$FIRECRACKER_PID/smaps_rollup
   ```

4. **P1：短基线回归。** 在原 v1/hybrid 布局与既有参数下跑一轮已知工作负载，
   检查创建成功率、runtime/resume p99、OOM、主机内存和可用的压力指标；
   对比历史同口径数据。通过后再继续原有消融优先级；ETMem 标记 ineligible。
5. **P1：v2 分支回归。** 在独立 v2 测试环境确认 writable v2 的从出生放置
   正常；只读根、缺少 cpu/memory 控制器或 Create 权限失败必须报错，不能
   回落为 nil manager。这项不要求迁移现有内网服务器。
6. **P2：需要时再补原生 v1 每沙箱 accounting。** memory 与 cpuacct 分挂
   路径、ns 路径映射、memory/memsw 限额差异、进程出生放置与清理都是独立
   设计问题，不纳入本次紧急兼容修复。
