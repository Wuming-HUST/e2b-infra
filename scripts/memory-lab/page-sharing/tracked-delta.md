# File / shared NBD RAM 增量快照

`RAM_EXPORT_MODE=tracked_delta` 是实验开关，默认 `full_logical`。本轮只为 File/MAP_PRIVATE（普通文件或共享只读 NBD）、4 KiB host/snapshot 页、有完整父逻辑映射的恢复实例开启追踪。冷启动及不符合条件的恢复保留原导出路线。UFFD 后端不使用这个新增追踪器。

需要本轮配套 FC：恢复前启动参数加入 `--kvm-dirty-bitmap --dirty-export-policy bitmap`，snapshot load 开启 `enable_diff_snapshots`。不依赖 HDBSS 或 UFFD WP_ASYNC。实际 CLI/API 不兼容、追踪不完整、没有成功的 state-only 冻结均明确报错，不保存空增量。

## 保存与恢复合同

CPU 写使用 KVM dirty log；用户态设备写使用 vm-memory 软件 bitmap。异步 block 完成、可写 IOVec、队列以及 balloon/FPR 匿名重映射都属于覆盖范围。discard 在第一次修改前标记；后续 madvise 失败不丢标记。逐 slot 收集立即累计到 ledger，后续 slot 失败不丢之前已清出内核的位；位图不按 present/mincore 筛选。

Pause 先停止 vCPU，再创建 state-only snapshot，排空并冻结设备写入，最后收集位图并只读取候选。首版 `Dirty=候选`、`Empty=空`；零页同样保存字节，其余继承父 header。保存后关闭 FC，不提供保存成功后清位并原地继续运行的接口。多次恢复自然建立新的父快照与追踪实例。

新快照使用 checkpoint version 2 / `tracked_delta`，记录载荷长度/SHA256、追踪合同版本、RAM/rootfs header 摘要和 FC state 摘要；不会把增量摘要当完整逻辑 RAM 摘要。metadata 仍最后持久发布。旧 version 1 full_logical/uffd_delta 保持可读；旧 reader 明确拒绝 version 2。

恢复通过合并后的 header 中的 BuildID/offset 读取历史对象。运行期 pin 不替代持久化 GC 的引用保留。缺失历史对象、源错误或读取失败不能补零。restore 的结构/摘要检查不表示已经扫描校验全部未读取远程 RAM。

## 验证

先在 Linux checkout 用仓库 Go 版本执行（集成测试默认跳过）：

```sh
cd packages/orchestrator
go test ./internal/cfg ./internal/sandbox/fc ./internal/sandbox/block \
  ./internal/sandbox/template ./internal/sandbox ./internal/sandbox/uffd \
  ../shared/pkg/storage/header -run 'Test(Tracked|RAMExport|ProcessMemory|Checkpoint)' -count=1
go test -race ./internal/sandbox ./internal/sandbox/fc ./internal/sandbox/template \
  -run 'Test(Tracked|Checkpoint)' -count=1
go build .
go test -c -o /absolute/new/factory-integration.test ./internal/sandbox
```

FC 使用固定 Rust 工具链，执行 `utils::dirty_tracking`、`devices::virtio::balloon::util`、`devices::virtio::iovec`、API 响应测试，编译 release；有权限的隔离节点还应执行已有 `test_external_snapshot` 设备冻结测试和 block `test_prepare_save`。完整源码命令与退出码应随交付保存，不把 filtered-out 的零测试输出算通过项。

真实 CPU/换出候选验证使用独立 128 MiB VM（guest.go 是单独文件编译，不能将目录中多个 main 一起编译）：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /absolute/new/guest-init \
  scripts/memory-lab/page-sharing/guest.go
sudo python3 scripts/memory-lab/page-sharing/tracked_delta_check.py \
  --firecracker /absolute/matching/firecracker \
  --kernel /absolute/matching/vmlinuz \
  --guest-binary /absolute/new/guest-init --output /absolute/new/native-run
```

它在读取任何候选内容或完整 oracle 前选择位图；验证冷非零父页缺 PTE、未入选仍正确继承，实际 CPU 写/归零入选，重复 harvest 不丢位，增量重建与 FC native full RAM 一致，以及 A→B→C 的 guest 内容。只对本轮冻结子进程的一页非零私有 COW 发起 MADV_PAGEOUT；以 pagemap 的真实 swapped 状态决定是否满足换出前提，不修改全局 swap/cache/pressure 配置。候选路径和完整对照耗时分开记录。

在既有 [M10](m10.md) 命令增加 `--tracked-delta`，分别运行 balloon 与 `--free-page-reporting`，验证实际非零父页 discard 未写、重用后重写、另一 VM/source 隔离及恢复。新的候选捕获在完整 RAM 导出前进行。提供的 `--cleanup-failed` 仅清理本轮自有资源；未知映射退出状态按原隔离/保留合同处理。

在既有 M11 命令增加 `--tracked-delta`，验证实际 Async virtio-blk 与持续 TAP RX 的候选覆盖；每个冻结点先捕获候选，再做三次完整 RAM/disk 稳定性和 native 对照。此 fixture 中同一 FC 的多次采样始终相对最初 backing 累计，属于候选诊断，不代表生产已支持原地 checkpoint epoch 切换。

生产 Factory 验证沿用 [README](README.md) 的显式输入，在全新目录设置：

```sh
E2B_SHARING_RAM_EXPORT_MODE=tracked_delta
```

结合 `E2B_SHARING_INTEGRATION_DIR` 等已有必填参数运行 `TestSharingFactoryIntegration`。B0 普通文件与 B1 共享只读 NBD 使用同一输入和追踪模式；冷启动 A 完整导出，B/C 检查实际候选载荷、上传与历史对象解析后的完整 RAM，与同一冻结点 native oracle 比较，然后通过 envd 恢复验证文件内容。原 trace 五工具片段的通过不等于完整原 trace。

回退时关闭实验开关，后续 File 快照恢复完整导出；已发布的 version 2 增量仍需新版 reader。回退 reader 前必须先恢复并重新导出兼容的完整快照，不能改写 version 字段。

工具生成注意：DirtyMemory 新字段与 `packages/shared/pkg/fc/firecracker.yml` 同步；节点离线缓存没有固定 go-swagger 模块时，`go generate` 会失败。该失败应保留，不能称作生成校验通过；编译、JSON 接口合同及真实 Factory 另行验证。
