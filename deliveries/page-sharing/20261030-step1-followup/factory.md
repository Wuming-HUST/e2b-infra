# Factory、真实 envd 与原 trace 文件编辑片段

2026-10-30 在 `H-arch -> agent03.coder` 的独立目录完成真实 Factory 集成：`Factory.CreateSandbox -> WaitForEnvd -> Pause -> Snapshot.Upload(Local) -> Cache.GetTemplate -> Factory.ResumeSandbox`。正式轮次 `factory-run-02` **PASS，15.04 秒，exit 0**。这是运行时集成及五个原始工具调用的验证；完整平台部署、完整原 trace 和对象存储服务仍未验收。

原始凭证位于 [factory/](factory/)，包括失败轮次、启动命令、输入 SHA、原始 serial、FC 状态、PID、File 实际映射及检查点哈希。拉回的 61 个文件已通过 `shasum -a 256 -c SHA256SUMS`。大型 RAM dump 继续保存在远端，下文给出路径和独立复算凭证。

## 真实执行与 oracle

- 同一个冷启动 checkpoint A：真实 envd 执行原 Write 步骤 50、55，保留调用 ID、绝对路径、完整 UTF-8 正文。B0/B1 均由该 A 恢复，再执行原 Edit 步骤 60、63、65。每次通过真实 envd 读回两个完整文件，断言长度与 SHA。原始 `Read` 参数保存在 fixture 中，历史 Read 结果没有用作当前文件 oracle。
- B0 通过 `GetTemplate(..., false, false)` 等待 File RAM 完整物化。三次恢复均验证实际 `/proc/<FC PID>/maps` 的 `rw-p`、路径及 inode 对应普通文件，不能仅凭“不持有 NBD lease”判定 File 模式。
- B1 两个 A VM 同时持有 `/dev/nbd0`。左 VM 编辑和 Pause 后，右 VM 仍读到 A 的原内容；恢复 B 时右 A 继续存活，A/B 使用 `/dev/nbd0`、`/dev/nbd1` 两个不同 backing，并各自通过文件 oracle。释放右 A 后 B 仍正常。
- 两模式分别完成 A→B→C 的生产 Pause、Local upload 和 cache restore。每个检查点先 Pause 并冻结设备写入，再在相同冻结状态取得 FC native full RAM dump，与实际生产 Pause 导出的完整逻辑 RAM 比较 SHA 和 268435456 字节长度。独立 postrun 再读 native 文件复算五个哈希，均与导出 receipt 相同。
- envd 启动 `/bin/sh`，Python 仅执行原 Write/Edit 的精确字节语义，完整参数经 base64 传入。每次 RPC 必须收到 `End`、`exited=true`、`exit_code=0`，流提前结束不能算成功。

| Checkpoint | native 与实际 Pause 全 RAM SHA256 |
| --- | --- |
| A | `aad989f4efa7fd6c2b388bfaaca154384bf9f1f554674150ed38c871d65c7ec8` |
| B0-B | `c9417993264690ccfa843caa5d3d845f31a57dacc157949d6c35683f679ae9b2` |
| B0-C | `dbc665c18836723a96d92a2ea69237656a170f69e9802e58093c6714b50c4e9b` |
| B1-B | `0bd9203d2c17f40583ec4e3c80ebdb260415c9b9e9b81d136b6e9598cfb6eb55` |
| B1-C | `65c1de2a6ce0c0a2e82e4ca7e2bab3f88f980f78a23f4683baa9a56cc911fa5e` |

native RAM 文件位于远端 `/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/factory-run-02/{A,B0-B,B0-C,B1-B,B1-C}-native.ram`。对应 `.snap` 状态已拉回。两个模式的 RAM 在恢复运行后会变化，因此 B0-B 与 B1-B 无须具有同一 SHA；各自的 native-vs-export 与文件内容才是本轮 oracle。

## 输入与复现

正式输入、完整命令、启动/结束时间及测试 binary SHA 见 [driver](factory/factory-run-02-driver.json)。FC 使用最终同源二进制 `m11-tools/firecracker-source-matched`，SHA `8830ff065716e073b74bed06fe073356ffff839eef5fe6039f3905b2b5149349`。测试源码 SHA 为 `8d1ef92951ae6405496fc5deab1f949ede6a6671037a094132970b88d0c8a66d`，其快照位于 `factory/inputs/sharing_integration_linux_test.go`。

本轮 1 vCPU、256 MiB RAM、4 KiB 页、关闭 hugepages，使用真实 ARM64 envd。guest init `/sbin/sharing-init` 来自复制的 fixture-v2，加载匹配内核模块并配置 guest 默认路由。iptables 从独立任务目录解析，未安装全局包或清空全局规则。

在已准备 fixture 与依赖的同一节点复现时，为每轮选择全新目录；确认自身高位 slots 未占用，不能因 SSH 超时直接重启旧轮次：

```sh
task_root=/home/coder/projects/yangshengxin/e2b/page-sharing-20261030
cd "$task_root/infra/packages/orchestrator"
GOMAXPROCS=4 \
GOCACHE="$task_root/go-build" \
GOMODCACHE=/home/coder/projects/yangshengxin/e2b/cache/go-mod \
  /home/coder/projects/yangshengxin/e2b/toolchains/go1.25.4/bin/go \
  test -p 2 -c -o "$task_root/factory-integration-repro.test" ./internal/sandbox

sudo -n env \
  PATH="$task_root/factory-deps/usr/sbin:$PATH" \
  E2B_SHARING_INTEGRATION_DIR="$task_root/factory-run-NEW" \
  E2B_SHARING_FC="$task_root/m11-tools/firecracker-source-matched" \
  E2B_SHARING_KERNEL=/home/coder/tide-repro/assets/vmlinuz \
  E2B_SHARING_ROOTFS="$task_root/factory-fixture-v2/rootfs.ext4" \
  E2B_SHARING_INITRD="$task_root/factory-fixture-v2/initrd.gz" \
  E2B_SHARING_TRACE_FIXTURE="$task_root/infra/scripts/memory-lab/page-sharing/biofabric-source-edit-slice.json" \
  E2B_SHARING_INIT=/sbin/sharing-init E2B_SHARING_SLOT_BASE=25000 \
  "$task_root/factory-integration-repro.test" \
  -test.run '^TestSharingFactoryIntegration$' -test.v -test.timeout 10m
```

所有输入必须是绝对路径；默认跳过该测试。fixture 制作脚本、网络依赖脚本、trace 提取器及 fixture 本体均在 `factory/inputs/` 留存；准备步骤说明在 `scripts/memory-lab/page-sharing/README.md`。

## 失败轮次与清理

`factory-run-01` 在真实 envd 初始化等待处 480.46 秒超时，原始日志和 exit 1 已保留。namespace 内 guest `/health` 返回 204，host 到 slot IP 超时；namespace DNAT 有 3 个包而返回方向 SNAT 为 0。fixture-v1 init 只有地址，没有 guest 默认路由。没有修改运行中的 guest 令该轮“通过”；等待自然失败并确认其 PID 与 namespace 清理，之后用独立 fixture-v2 重跑。两份原 init 脚本均已归档。

正式轮的八个自有 FC PID、`ns-25000..25004` 与 `veth-25000..25004` 全部消失；只读 rootfs 输入 SHA 未改变。首次独立 postrun 观察到 `/sys/block/nbd0/pid=1387949`，故该次全局 observer 的立即-detach 断言失败。同期还有独立 KVM/M10 测试动态分配 NBD；释放后的编号可以被复用，且 sysfs 数值属于内核侧 PID，**不能据此认定它仍属于原 Factory lease 或断言本轮断连延迟**。随后只读枚举和最终核查均无 NBD PID。本轮归属与顺序以 Factory drain、各自 PID 和 API 回执为依据；没有手工 disconnect、kill 或修改非本轮设备。[首次观察](factory/factory-run-02-postrun-initial.json) 与 [最终核查](factory/factory-run-02-postrun.json) 保留原字节，不把全局设备编号当稳定身份。

## 完整原 trace 的实际缺项

原始 `trajectory.json` 与 `claude-code.txt` 均已找到。前者 19706437 字节、1028 steps、1052 tools，SHA `6345988d0e84ca897b54a875fa6b4a4fe879b620ad095d3a61bfde825490d9e2`。1052 个工具中仅上述 5 个被本轮执行，其余包括 Bash 575、Read 269，以及 TaskCreate/TaskUpdate/TaskOutput/TaskStop；不能将五工具片段算作完整 trace 通过。详见 [本地素材清单](factory/original-trace-asset-inventory.json)。

完整任务 Dockerfile、starter、hidden 和 golden-generation 素材存在于本机 `SWE-bench/swe-marathon/tasks/biofabric-rust-rewrite/environment/`。Dockerfile 指定 Ubuntu 24.04、Rust 1.93.0、Java 11、编译依赖和三个固定上游版本：BioFabric-rs `2e4f581ffb16bae300e5e9dcf23d22b3f740e108`、BioFabric `5fa975ce2d5309c56749f2970959516832acaa8f`、AlignmentPlugin `2bb7931eae9adcc287a2e9f30bd2e1402e9a5836`。完整环境需要执行其中的 golden 生成、starter 替换、Cargo 依赖抓取与离线缓存准备；当前 starter 无 Cargo.lock，public goldens 目录仅占位文件，没有等价已构建任务 guest 的验收凭证。

当前实际 Factory rootfs 的只读 `debugfs` 预检确认 `/app`、bash、cargo、rustc、java、javac、gcc 和 `/root/.cargo/registry` 不存在；Python3 与真实 envd 存在，见 [当前 guest 预检](factory/factory-full-trace-preflight.json)。因此下一项具体工作是制作并核验完整任务 guest，而非依据 32 GiB 容量宣布全 trace 无法运行。

原 trace 还引用 `/logs/agent/sessions/projects/-app/6160c7d1-61f9-4524-82ab-90ee7be654a4/tool-results/{bng4mw15j,byokxoyzf}.txt` 及 session memory 文件。当前该任务 `agent/` 目录仅两个原始大日志，没有 session/tool-results 目录。需要从原任务完整工件恢复这些运行时路径，或用保留 TaskCreate/后台 Bash/TaskOutput 身份关系的真正 replay 重新产生，并对行为定义 oracle；不能将历史文本或合成任务输出冒充重新执行。当前五工具 driver 尚未实现这些完整工具生命周期、原始任务编译与结果判定。完成这些准备后才能在当前节点实际尝试完整回放并测定其资源边界。
