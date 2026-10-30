# 步骤一实施与验收记录

日期：2026-10-30。**步骤一整体尚未验收；步骤二未开始。** 当前交付包含源码、软件测试和独立真实 KVM/NBD 机制证据，尚不能替代编排器部署与原应用 trace 验收。

## 分支与基线

- E2B：`feat/shared-nbd-memory-20261030`，起点 `d15315d801dbc6e3986a3890751f91aa21ec4aa8`。源码提交见本目录 `revision.json`。
- Firecracker：指定仓库已切换 `feat/shared-nbd-memory-20261030`，提交 `0ce251366`，起点 `8da283e3bfc876167fde8f45e541bec0bfad0e9a`。同一提交也在隔离工作树分支 `feat/shared-nbd-freeze-20261030`。
- 原有实验、模板和未跟踪文件未纳入本次源码提交。远端使用 `/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/` 独立目录，路由为 `ssh H-arch` → `ssh agent03.coder`。

## 实现合同

1. `SHARED_NBD_MEMORY_ENABLED=true` 显式选择共享只读 NBD RAM；默认关闭。要求 host/snapshot 4 KiB、非 hugetlb；获取失败直接报错。保持原 rootfs/virtio-blk 路径。需要配套本次 FC 提交，不可只替换 E2B。
2. `memorybacking` 提供 immutable identity、Local/HTTP range source、registry、独立 lease 和失败广播。RAM identity 使用逻辑 header manifest hash 与 FC state/layout hash；**manifest hash 不等于未读取 RAM 内容的 checksum**。kind/size/layout 均参与 key，设备编号不是持久身份。
3. 同 identity 单次创建设备。kernel readonly、server 拒绝 WRITE/TRIM/WRITE_ZEROES、O_RDONLY 容量检查完成后才 ready。取消单个 Acquire 不终止其他用户。VM 退出后释放 lease，最后用户 drain/断连后关闭 source。不能确认断连时隔离 entry/source/设备池，不复用编号。
4. source 持有分层 backing 的独立 pin；缓存失效/TTL 和原 snapshot cleanup 不能关闭仍在被 NBD 读取的对象。pin 只打开按需源，不完整物化 RAM。未经独立所有权接口支持的 source 拒绝进入共享模式。
5. File/NBD/cold boot 的 `NoopMemory` 导出全部逻辑块为 Dirty、Empty 为空。按 mappings 的逻辑 offset 流式 `process_vm_readv`，每批最多 8 MiB，处理短读、跨 region、取消和错误；hash 与输出使用同一次读取。未驻留、归零、私有换出页均不靠 residency 排除。
6. FC 的无 `mem_file_path` checkpoint 无条件冻结 net/block/balloon/FPR/vsock/rng RAM writer，block prepare_save 排空异步 I/O；resume 解冻。拒绝无法在本 event loop 冻结的 vhost-user block。新增 runtime 标志不进入 snapshot 序列化。
7. checkpoint metadata 为最后的提交标记：RAM/state/rootfs diff 完成并 fsync 后，以无覆盖原子 link 发布；上传同样最后提交 metadata。新增可选 `Checkpoint` format 1，记录 mode、logical size、RAM/header/state/rootfs diff SHA256。restore 核查 state、headers、size 和 checksum 字段格式；不声称已校验所有未读远端 payload。旧模板无该标记仍可恢复。
8. `tracked_delta` 未作为新 File/NBD 选项开放。原 UFFD 路线保留原行为，其 checkpoint 标为 `uffd_delta`。工厂 shutdown 停止 startup、等待清理/FC 退出、drain backing 后才关闭设备池；ForceStop 不能跳过此顺序。

## 当前验证

目标为 openEuler 24.04 / Linux `6.6.0-95.0.0.99.oe2403sp2.aarch64`，host page 4096；Docker cgroup memory.max=32 GiB，CPU quota=16 cores。guest 为已有 kernel，SHA256 见 `environment.log` 和各 KVM `environment.json`；guest 实际 page size 见 `guest-ready.json`。未改系统 swap/cache 配置，未使用 KSM 或全机 drop_caches。

- Go 1.25.4 ARM64：合同测试、race、header/metadata 测试、orchestrator/helper/test binary/guest 构建。最终结果见 `target/software-delivery/exit-codes.tsv`。
- Rust 1.85：8 个修改文件格式检查、balloon/FPR/vsock/rng 三项冻结回归、GNU ARM64 release 构建通过。`firecracker/fc-validation.json` 记录源码和 binary hash；11 个原基线格式违规文件已恢复，未提交无关格式变化。
- GNU FC 构建沿用该 target 的 `unimplemented.json` seccomp 策略；当前 KVM 机制结果不证明生产 musl/seccomp 打包可用。
- 普通 File：M01 冷 source + absent host PTE 的非零页保留；A→B→C 写/零/未访问页恢复成功。每次全量导出与同一冻结状态的 FC native full dump 比较。restore 会更新 VMGenID，因此跨 generation 不错误要求整份 RAM hash 恒等。
- 两个真实 FC：同一 registry/NBD source 的 clean marker PFN 三次稳定相同；私写与归零后 PFN 分离，另一 VM/source 保持不变；先停止 A/release 一份 lease，B 仍可读；最终 drain 成功。最终产物复验见 `target/kvm-delivery/`，首轮见 `target/kvm-nbd-first/`。
- 主动换出：只对本次 VM 的两个匿名 exclusive COW 页请求 MADV_PAGEOUT，页表确认两页实际 swapped；随后完整导出/native hash/恢复 oracle 通过。最终复验见 `target/kvm-delivery/M09-pageout.json`，首轮见 `target/kvm-pageout/M09-pageout.json`。此结果不是压力下的吞吐或尾延迟测试。
- clean cache 驱逐：对本次动态设备的 clean 页先移除两个 private mapping 的 PTE，再只对本设备执行 BLKFLSBUF。mincore 确认缓存从 resident → absent，重读后重新 resident 且该页 source 读取从 1 次增至 2 次，两个私有 COW 页保持不变。见 `target/nbd-cache-eviction.log`。
- 原有可写 NBD 回归：4 MiB、32 MiB O_DIRECT、普通写、偏移写通过，显式动态 IndexAny、串行执行。见 `target/rootfs-nbd-regression.log`。

本次 4 轮 KVM run 的 28 个 VM/helper PID 均已只读确认退出，最终 helper 记录 `closed`；见 `target/process-close-receipt.json`。

首次失败日志保留：NBD 取消竞态已复现修复；全 NBD 包以非 root 运行触发的旧测试权限失败改用受控 root 回归；FC 首轮缺开发链接名 `libseccomp.so`，复用已有只读 toolchain lib 解决。禁止把这些首次日志中的失败或后续单个 exit 0 当作最终验收状态。

## M01–M13 逐项状态

| ID | 当前证据 | 未覆盖范围 |
|---|---|---|
| M01 | 软件回归、真实 KVM 冷 File source/absent PTE/full dump/恢复通过 | 编排器端到端 |
| M02 | 两个真实 FC 同 NBD，三次稳定 host PFN 共享通过 | 大规模密度/长期运行 |
| M03 | 两个 FC 写/归零 PFN 分离、其他 VM/源不变、恢复通过 | 原 trace |
| M04 | registry kind/content/layout/size 区分与单测通过 | 两个不同模板的编排器部署 |
| M05 | Acquire/取消/创建失败/Drain 单测与 race；真实双 VM lease 释放通过 | 完整节点部署/故障重启 |
| M06 | HTTP、逻辑跨层读、短读、校验/越界、导出与提交失败单测；真实 NBD EIO 通过 | 真实对象存储故障 |
| M07 | 三代 header oracle、真实 KVM File 多代及 NBD 私有快照恢复通过 | 编排器三代/应用 trace |
| M08 | 本次 NBD clean cache 已实际驱逐，refault 内容与 source 新读取通过，私有 COW 保留；冷 File source 通过 | 与 VM/编排器压力回收组合 |
| M09 | 真实 FC 两个 COW 页已 swapped，完整导出/native/恢复通过 | 持续压力、多 VM swap 及性能 |
| M10 | FC balloon/FPR 冻结及解冻事件/RAM 单元测试通过 | guest discard→未写/重写与另 VM/source 的完整组合 |
| M11 | FC freeze 覆盖与排空路径审计、三项 device 回归通过 | 真实 block/net/vsock completion 与暂停竞争，state/RAM/rootfs 联合 oracle |
| M12 | HTTP 延迟、断流、版本/范围/校验失败及有界并发单测；lease 失败广播通过 | 多真实 VM 远程故障后的停止策略端到端 |
| M13 | 多 region、逻辑顺序、边界/溢出、短读与 header 映射单测通过 | 实际多 region 大内存 KVM |

**进入第二步的门槛尚未满足**：M10/M11 联合目标测试、编排器集成、B0/B1 原 trace 对照仍待内网验收；远程对象存储也未实测。virtio-pmem、DAX、lower/upper 布局和新运行态模板未实现，不应将其列为本次可用能力。

## 复现与内网接续

源码可分别用 `git am infra-step1.patch`、`git am firecracker-step1.patch` 应用到相应基线；补丁、最终二进制哈希及每个证据文件的 SHA256 均随本目录交付。128 MiB RAM 镜像留在远端独立 run 目录，未重复纳入 Git；本地保留 state、原始日志、oracle/hash、PIDs 与指标 CSV。

源码测试入口：`scripts/memory-lab/page-sharing/check-software.sh NEW_RESULT_DIR`（显式设置 Go caches）；native 入口与依赖见该目录 README。远端采用 jobs=4/p=2，不启动全量平台。

```sh
# 单独内核合同，动态分配本次设备；不要运行未核查的全包 privileged 测试。
sudo env E2B_SHARED_NBD_KERNEL_TEST=1 E2B_SHARED_NBD_REQUIRE_PFN=1 \
  ./nbd.test -test.run '^TestReadonlyNBDKernelSharing$' -test.v -test.timeout 120s

# 必须观察实际 clean cache eviction，不能把单纯 PTE refault 当成功。
sudo env E2B_SHARED_NBD_KERNEL_TEST=1 E2B_SHARED_NBD_REQUIRE_PFN=1 \
  E2B_SHARED_NBD_REQUIRE_EVICTION=1 \
  ./nbd.test -test.run '^TestReadonlyNBDKernelCacheEviction$' -test.v -test.timeout 120s

# 独立 128 MiB VM。所有输入均为绝对路径，OUTPUT 必须不存在。
sudo python3 scripts/memory-lab/page-sharing/kvm_check.py \
  --firecracker FC --kernel KERNEL --guest-binary GUEST_INIT \
  --nbd-helper SOURCE_HELPER --attempt-pageout --output OUTPUT --cleanup-failed
```

内网先部署配套 FC/E2B 到隔离节点，执行 M10/M11、两不同模板/多 region、远端对象 storage 和 B0/B1 原 trace 逐工具退出码/最终文件 manifest 对照。收集 PSS/RSS/Private_Dirty/VmSwap/faults、创建/导出延迟与读取量；当前 `memory-metrics.csv` 与 snapshot receipt 仅为测试观测，不是生产监控或容量收益。

通过步骤一验收后再按 handoff 执行 pmem/DAX 步骤二。回退共享模式使用 `SHARED_NBD_MEMORY_ENABLED=false`；保留安全 full_logical 修复及配套 FC，继续原 rootfs 路径。不要在活动 VM 上 retarget NBD 或替换模板对象。

Linux NBD netlink 断连接口按数字 ID 操作；本实现检查 socket 健康、隔离未知 transport，并保护本 pool 的 ownership。其他进程强制断连/重绑与最后健康检查的竞争，仍需节点级独占管理；当前接口没有条件 cookie 断连能力。
