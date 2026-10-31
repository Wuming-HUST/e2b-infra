# 步骤二实施状态（2026-10-31）

**设备、完整 rootfs DAX、生产 Factory 快照恢复链已实现并在 agent03 原生验证；步骤二整体尚未验收。** 用户已允许保留步骤一待验项后继续，步骤一余项见同目录 `step-1-remaining-tests.md`。

## 代码与格式

两仓均使用 `feat/virtio-pmem-dax-20261030`；Firecracker 在隔离 worktree 实施。包含只读独立 GPA/slot、virtio-pmem/API/设备快照、RAM 与 PMEM 访问能力区分、旧快照显式解码路径；E2B 包含完整物化器、v3 manifest、cold boot early init、私有 upper 与恢复重绑。功能默认关闭。

原模板是 ext4，namespace 镜像是 `[2 MiB PFN 元数据][ext4][padding]`。PFN_MODE_RAM 将 struct pages 放在每 VM 普通 RAM，lower 保持不可变。制作与接口说明见 `scripts/memory-lab/pmem-dax/README.md`。此提交提供离线制作工具和 runtime 集成，现有模板构建服务的自动转换流水线尚未接入。

## agent03 实测

目标为 Kunpeng 950、ARM64 4 KiB、32 GiB 容器；宿主 `6.6.0-95.0.0.99.oe2403sp2.aarch64`。隔离运行根：
`/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/pmem-dax-01`。

- Firecracker 原生编译成功；格式检查成功；格式化后的 `fc-check-06` 七项 PMEM/持久化单测及 API 测试（lib/main 各一次）通过。
- Go 合同测试、完整 orchestrator 编译、整机测试 binary、静态 early init 编译成功；五个相关包 race 通过。Python PFN 包装两项测试通过。
- 新构建 Linux 6.6.60 guest，原生 Image 编译成功，必要驱动内建。官方源码 SHA256 `52f9e32d5082ab94253447fd66670d0c3bb765cfcb99b0bf61d1b8eae25952ef`；Image SHA256 `93e21a97a8e9fdec98462cfaaf14ad13b5721443dedabc8d7508bc0c9157ae79`。
- `factory-05` 的 `TestPmemFactoryIntegration` 通过，45.14 秒。完整 rootfs lower 启用实际 DAX，envd 就绪；真实 Factory/Pause/upload/cache restore 路径中，File RAM 与共享只读 NBD MAP_PRIVATE RAM 均完成 A→B→C。每次冻结的完整逻辑 RAM 与同一冻结边界的 FC full dump SHA/size 相同，排除 PMEM 窗口。
- 两台 NBD RAM VM 共享同一已发布 lower inode，在 guest 实际访问过的相同文件偏移 `35651584` 观察到相同 host PFN `2155170304`，映射权限 `r--s`。不是先由 host 预触页制造证据。此结果未覆盖不同 backing 对照或净内存收益。
- lower unlink/whiteout、opaque directory、chmod、UID/GID、symlink、rename、xattr 跨代保留。关闭一个 VM 后另一个仍可用；上传及恢复使用实际本地对象存储 provider，未验证真实 S3。
- 五条真实 source-edit 工具 payload 的退出码/输出/最终文件结果一致，**不是 1052 条原工具 trace 全量回放**。

## 复用 r4 内核和 trace

旧 Debian `6.1.0-50-arm64` guest 配置缺少 ZONE_DEVICE/FS_DAX/PFN，不能直接验证新 DAX 路径，但可以做 legacy 回归。

原始 r4 三个 manifest hash 均通过。`synthetic-legacy-01` 使用原 r4 时发现 mtool 不输出 JSON，与现有 replay 协议不匹配，失败收据保留。随后仅在新 rootfs 副本内，以远端现有 `mtool.c` 编译的静态工具替换旧 mtool；原始 rootfs 不变，trace/replay 未修改。差异及输入 hash 见 `r4-tool-v1`。

`synthetic-legacy-02` 使用新 FC + 旧 guest kernel + 派生 r4 rootfs 完成全量**合成内存**回放，退出 0，203.80 秒；10/200 等三个边界保存恢复；1016 turns、571 次合成工具工作，最终状态校验通过。该结果是 legacy 路径回归，不是 PMEM 全量回放，不是原始 SWE-marathon 工具执行，也不用于性能结论。

## 2026-10-31 追加共享与快照证明

新增 `TestPmemSharingProofIntegration` 已在 agent03 分别以 full_logical 和 tracked_delta 跑通，File/NBD 两后端均通过。补齐固定非零 RAM 页的真实 PFN/COW、应用 OverlayFS 路径 DAX mmap、不同 backing 对照、copy-up 后私有页、已打开 FD/mmap 的跨代恢复、源完整 hash 不变及生命周期检查。两轮共 20 个 VM，全部清理；详细数据与 137 份原始证据见 [共享证明](../20261031-sharing-proof/README.md)。此项证明核心共享/保存恢复链，整体步骤二仍有下表待验项。

## 验收矩阵

“部分”只代表列明子项通过，不等于该 ID 已验收。

| ID | 状态 | 已有证据及剩余工作 |
|---|---|---|
| D01 | 通过 | PMEM/PFN/GPA、完整 rootfs、lower 与最终 OverlayFS 路径 STATX_ATTR_DAX、实际 mmap GPA 落入 PMEM、envd/恢复均通过 |
| D02 | 部分 | 已知文件的应用路径 mmap 同 PFN，内容相同但不同 inode 对照为不同 PFN；指定 exec 页的独立 PFN 矩阵待补 |
| D03 | 部分 | 工具读文件正确；专门 buffer 私有性测试待补 |
| D04 | 部分 | RO FD/map/KVM slot、只读/可写设备内存接口及恶意 flush 描述符单测；真实 guest CPU/raw block 攻击、第二 VM 与源 hash 四方证据待补 |
| D05 | 部分 | 正常私有写、append/fsync/copy-up、另一 VM 不变及恢复通过；truncate 等完整矩阵及写放大核算待补 |
| D06 | 通过 | lower 删除/whiteout、opaque directory 跨代恢复不复活 |
| D07 | 部分 | 文件 rename、symlink、xattr、mode、uid/gid 跨代通过；目录 rename、hardlink、EXDEV 等基线对照待补 |
| D08 | 部分 | 已打开 FD/DAX mmap 后 copy-up、跨 B/C 快照恢复均通过，保留旧 lower 内容；rename/unlink 的打开映射矩阵待补 |
| D09 | 部分 | 只读 DAX 源接口的 IOVec/block 读能力单测；真实 sendfile/splice/TX、锁、安装工具待验 |
| D10 | 通过 | File/NBD RAM × 完整/增量保存 A→B→C；同冻结边界完整 RAM hash、打开 FD/mmap、进程与文件状态、固定 GPA 均通过（256 MiB 单 RAM region）；新增 NBD RAM 1536 MiB 全量合成 trace 的完整/增量三次恢复通过 |
| D11 | 部分 | 缺源/短读/hash/并发取消/损坏发布文件、layout/checkpoint mismatch 合同测试；真实 disk-full、远端故障整机闭环待补 |
| D12 | 部分 | 双 VM 关闭一个不影响另一个；缓存当前不在线淘汰，压力/GC 待验 |
| D13 | 部分 | 旧版序列化形状/CRC 单测、旧 guest legacy 全量合成回放；真实历史 FC snapshot、旧 binary 拒绝新格式待补 |
| D14 | 部分 | 五条真实工具 slice 通过；新增 NBD RAM + PMEM/DAX 的完整/增量全量合成回放均通过，各 1016 turns、571 合成工具事件、3 次恢复；1052 条原工具执行仍待验 |

## 2026-10-31 追加全量合成 trace

新增 `TestPmemMarathonIntegration` 原生编译通过，复用生产 Factory/Pause/upload/cache restore。在共享 NBD MAP_PRIVATE RAM + PMEM/DAX lower + 私有 NBD upper 下，1536 MiB/2 vCPU 的 full_logical、tracked_delta 两轮均完成 1016 turns、571 次合成工具事件和 576 个调用 ID，turn 10/200/700 三次恢复通过；最终工作状态 hash 与 legacy 一致。每代核对实际映射与 DAX GPA、同冻结边界 native full RAM oracle，增量轮重建历史依赖，A RAM/lower hash 保持不变。目标收集器确认两轮退出 0、26 个 Go/30 个 FC 文件 hash 匹配，以及 10 个 VM/专用网络清理完成。

本轮测试提交 `35c20c28a`，详情见 [trace 报告](../20261031-marathon-pmem/README.md)。原始证据包已在 agent03 生成，传输连接反复超时，本地完整离线复核仍待完成。它是合成内存/I/O 负载，不等于原始工具/LLM 执行，不用于速度或容量结论。

## 限制与后续

当前 FC 是 debug/glibc 原生构建，使用 empty default seccomp；生产 musl/seccomp/jailer、x86 编译运行、固定版本 Swagger 重新生成未验收。R1 具备完整物化/校验/原子发布及 warm reuse，真实云对象存储仍待验证。缓存采用保留式策略，无在线淘汰，磁盘容量规划与安全 GC 待做。R2 直接 NBD-backed PMEM 未实现。

上述多数功能测试可继续在当前节点执行，不能归因于“只有 32 GiB 所以无法测试”；全量部署、真实服务/对象存储及原任务环境另需资源。尚未开展物理内存净收益、容量或 SLO 实验。

## 收据与可复现输入

同目录 `evidence/` 保存实际收据、日志、失败尝试和完整 guest kernel config；不包含大型 RAM/rootfs/binary。`evidence-sha256.json` 校验本地证据。Factory 的输入命令位于 `factory-05/test.json`，实际输入路径在本节远端运行根；集成测试环境参数须按代码显式提供且使用全新目录。失败的 factory-01..04 分别定位到 iptables PATH、裸 ext4 缺少 PFN DAX、PFN resource 的 data_offset、测试 oracle 误要求首 metadata 页 resident；均保留，未覆盖收据。

已提交 Firecracker：`bd2800cac`（`feat/virtio-pmem-dax-20261030`）。E2B 实现、测试和本报告同提交；对应提交可用 `git log -1 -- packages/orchestrator/internal/sandbox/pmem` 查询。

复跑已建输入（会创建新的 VM/NBD/网络；选择未使用的 slot 和全新目录）：

```sh
b=/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/pmem-dax-01
sudo -n env \
  PATH="/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/factory-deps/usr/sbin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
  GOMAXPROCS=4 \
  E2B_PMEM_INTEGRATION_DIR="$b/factory-review-new/run" \
  E2B_SHARING_FC="$b/cargo-target/debug/firecracker" \
  E2B_SHARING_KERNEL="$b/kernel-build/out/arch/arm64/boot/Image" \
  E2B_SHARING_ROOTFS="$b/fixture-02/upper.ext4" \
  E2B_SHARING_INITRD="$b/go-check-04/initrd.gz" \
  E2B_SHARING_TRACE_FIXTURE="$b/infra/scripts/memory-lab/page-sharing/biofabric-source-edit-slice.json" \
  E2B_PMEM_LAYOUT="$b/fixture-02/layout.json" \
  E2B_PMEM_LOWER="$b/fixture-02/lower.raw" \
  E2B_SHARING_SLOT_BASE=26100 \
  "$b/go-check-05/sandbox.test" -test.run '^TestPmemFactoryIntegration$' -test.v -test.timeout 10m
```

先创建 `factory-review-new` 父目录，`run` 必须不存在。完整源码重建后应替换 test binary/FC/early init 路径，并重新制作 initrd；以上命令明确复用本次已验证二进制。

本地提交与 agent03 实际测试源码已逐文件核对：24 个 Go 文件和 30 个 Firecracker 文件 SHA256 一致，见 `tested-source-sha256.json` 与 `source-comparison.json`。最终审计未发现本运行根下残留进程；未操作其他实验进程。
