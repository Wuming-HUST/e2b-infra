# 同模板 RAM/rootfs 共享、写隔离及快照保存恢复证明

2026-10-31 在 agent03.coder 原生 KVM 实测通过。**同模板沙箱能够物理共享未修改的 RAM 页和不可变 rootfs DAX lower；写入在每 VM 的私有 RAM/upper 中隔离；完整与增量快照均能保存并恢复这些状态。** 本报告限定受测平台与功能，不代表步骤二所有验收项或生产部署已完成。

## 环境与输入

- Kunpeng 950、ARM64；宿主 6.6.0-95.0.0.99.oe2403sp2.aarch64，guest Linux 6.6.60；host/guest 4 KiB 页。
- 容器内存上限 34359738368 字节（32 GiB）；每 VM 256 MiB RAM、1 vCPU。
- 使用完整 envd rootfs 制成的 2281701376 字节不可变 PMEM namespace lower（其中含完整 ext4），每 VM 独立 128 MiB ext4/NBD upper。
- 生产 Factory/Create/Resume/Pause/Upload/TemplateCache 路径，真实 envd 进程接口；对象存储为本地 provider。FC 为 debug/glibc，empty default seccomp。
- FC 提交 `bd2800cac`；E2B 基础提交 `118890bc0` 加本报告同提交的测试。25 个 Go 文件、30 个 FC 文件与 agent03 测试源码逐文件 hash 一致；kernel、FC、initrd、lower、upper、fixture 与测试 binary hash 见 `evidence/sharing-proof-provenance.json`。

两个独立运行：

| 运行 | RAM 保存模式 | 结果 | 用时 |
|---|---|---|---:|
| sharing-proof-full-01 | full_logical | File/NBD RAM 均通过 | 64.35 秒 |
| sharing-proof-tracked-01 | tracked_delta | File/NBD RAM 均通过，未回退成完整导出 | 62.77 秒 |

每轮从同一个冻结快照 A 建立 File 与 NBD 两条独立路径，各自克隆左右两个沙箱，并执行 A→B→C。另有不同 backing 对照。两轮合计启动并清理 20 个 VM；实际测试为 `TestPmemSharingProofIntegration`。

## RAM 物理共享与 COW

在 cold VM 内启动持续运行的进程，分配并锁定三个普通匿名页，分别填满非零字节 `0x31/0x32/0x33`。从进程的 guest pagemap 获取实际 GPA，随 A 一起保存。左右 VM 恢复后先通过真实 guest 读操作触发访问，再将 GPA 经 FC 实际 RAM mapping 转成 host VA，读取对应进程 pagemap。

测试要求：host PFN 可见且非零、映射为 MAP_PRIVATE、两个 VM 对应同一逻辑 offset、初始页为 file-backed 页；每阶段独立采样三次。host 不通过写内存或预触页制造证据。

完整保存模式、共享 NBD RAM 的实际首组 PFN：

| 页 | 写前左 | 写前右 | 左 VM 写后左 | 左 VM 写后右 |
|---|---:|---:|---:|---:|
| 保持 0x31 | 2689450097 | 2689450097 | 2689450097 | 2689450097 |
| 0x32→0xa5 | 2689450098 | 2689450098 | 2706443870 | 2689450098 |
| 0x33→全零 | 2689450099 | 2689450099 | 2730151752 | 2689450099 |

这同时验证了非零源页的物理共享、单边写 COW、写零的正确性及未修改页继续共享。两个被写页变为 host 匿名页；另一 VM 的三个页仍是原内容及原 PFN。File RAM 和增量保存那一轮也通过同样判据。

原始样本：`evidence/sharing-proof-full-01/run/B1-physical-sharing-proof.json`；其中包含全部三次采样、raw pagemap entry、host VA、映射行、guest GPA 与内容 SHA256。

## rootfs 从应用路径直接共享 DAX 页

持续进程从正常应用路径 `/sbin/sharing-init` 打开文件并 mmap，保留 FD 和映射跨越快照。读取后核对 guest pagemap：该映射的 GPA 落在独立 PMEM 窗口，不在普通 RAM。最终 OverlayFS 路径和 lower inode 的 `STATX_ATTR_DAX` 均置位。

同模板两个 NBD RAM VM 的该文件 mmap 对应同一 host PFN **2730629540**，host 映射 `r--s`；测的是这个已知文件的实际 DAX 页，不是随便挑选共同 resident 的镜像页。

左 VM 通过原应用路径写文件并 fsync 后：

- 左 VM 新打开的文件内容改变，所在 GPA 落在普通 guest RAM，DAX 属性取消，符合 upper copy-up。
- 右 VM 的原路径内容、DAX 属性和共享 PFN 保持不变。
- 左 VM 写前已经打开的 lower mmap 保持原内容，继续与右 VM 共享原 PFN。
- 两轮结束后重新校验完整 lower 文件和原输入 SHA256，均为 `da51eb9d5616871823dbc03f924c2b77ca77fd89d73f99240ab20d61a9e87f1e`。

这证明共享的是只读 lower 页。应用 read 缓冲、heap、copy-up 后文件页及可写 upper 不计作 rootfs 物理共享。

## 内容相同而 backing 不同的对照

第三个沙箱恢复相同快照、读取相同内容，但使用独立 RAM backing 和独立物化的 lower inode。完整模式下 lower inode 为 `10794774` 与 `11712278`，文件内容 digest 相同，而 mmap 对应 PFN 为 `2730629540` 与 `2731898276`。三个 RAM marker 的 PFN 也全部不同。

因此“相同内容/相同模板名称”本身没有被当作共享证明；同节点共用已发布 inode/offset 才得到上述物理共享结果。原始对照见每轮的 `run/different-backing-control.json`。

## 完整及增量快照保存恢复

A 保存初始进程、匿名 marker、打开的 FD/DAX mmap 和磁盘状态。B 保存单边写非零、写零、rootfs copy-up 及五条 source-edit slice 工具操作后的状态；恢复 B 后再次写 RAM marker、append 文件并 fsync，然后保存 C、恢复 C。

每次保存均在同一个冻结区间获取 FC 原生 full RAM oracle，再走生产 Pause/upload：

- full_logical：导出大小为完整 256 MiB，所有字节的 SHA256 与同冻结边界的原生导出相同。
- tracked_delta：先于原生 full oracle 读取候选页；生产导出的 packed payload 与事先捕获的候选字节完全一致。测试通过生产 layered source 自动读取历史 BuildID，将父快照与增量重建成完整逻辑 RAM，再逐字节计算 SHA256，与原生 full oracle 相同。不是仅检查脏页数量。
- B/C 恢复后通过 envd 对原持续进程发命令，核对进程 PID、epoch、三个完整页 hash、原 DAX mmap 内容/GPA、新文件内容/append、lower 内容和 copy-up 页所在地址区间。
- whiteout/opaque、mode、UID/GID、symlink、rename、xattr 及原 source-edit slice 最终文件内容仍被原测试核对。
- 暂停关闭左 VM 后，右 VM 仍能读出 A 的 RAM 和文件原内容；B 使用新的 RAM backing，右 VM 保持原 A，不发生串模板。
- 最终通过生产 source 重新解析 A，完整 256 MiB SHA256 保持不变。

实际保存量（不是容量或性能收益结论）：

| RAM 后端 | 保存模式 | B payload | C payload | 每代完整逻辑 RAM |
|---|---|---:|---:|---:|
| File | full_logical | 268435456 | 268435456 | 268435456 |
| 共享 NBD | full_logical | 268435456 | 268435456 | 268435456 |
| File | tracked_delta | 14336000 | 12763136 | 268435456 |
| 共享 NBD | tracked_delta | 15626240 | 13033472 | 268435456 |

保存量单位为字节，含测试进程、envd、内核和设备活动造成的变化，不只三个 marker。PMEM 设备窗口 2281701376 字节未混入普通 RAM dump。

## 复查与复跑

本地复查原始收据，重新核对 raw pagemap entry、GPA→VA→file offset、三次 PFN 关系、非零 marker 内容 hash、B/C 恢复状态与证据 hash：

```sh
python3 -B scripts/memory-lab/pmem-dax/verify-sharing-proof.py \
  deliveries/page-sharing/20261031-sharing-proof
```

预期 `result: passed`，137 个证据文件，20 个已清理 VM；结果另存 `verification.json`。该离线复查不重新读取大型 RAM 镜像；完整 RAM 重建/原生 SHA256 对比是在目标 Go 测试里完成的，相关大型文件保留在 agent03 独立测试目录。

目标节点的精确输入与命令见每轮 `inputs.json` / `test.json`。测试源码位于 `packages/orchestrator/internal/sandbox/sharing_proof_integration_linux_test.go`，沿用 step2 制作的完整 rootfs 输入。可按 step2 状态报告的环境参数，将 test binary 改为 `sharing-proof-build-01/sandbox.test`、测试名改为 `TestPmemSharingProofIntegration`，分别设置 `E2B_SHARING_RAM_EXPORT_MODE=full_logical/tracked_delta`；每次使用全新运行目录和未占用 slot。

远端运行根：`/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/pmem-dax-01`。本次未重启或清理其他任务；20 个 VM 均已退出、专属网络 namespace 均已清理，见 `evidence/sharing-proof-cleanup.json`。

## 证据边界

以上证明当前 ARM64/4 KiB、256 MiB 单 RAM region 配置中的共享/正常写隔离/多代完整与增量保存恢复。尚未证明所有极端场景无缺陷，也未完成：真实 guest 原始 PMEM 写攻击、恶意设备整机矩阵、全部 sendfile/splice 等 DAX I/O、长期压力与 GC、真实 S3、生产 musl/seccomp/jailer、完整原业务工具 trace。不同 backing 对照与最终 OverlayFS 路径 DAX、已打开 mmap 的 copy-up/恢复子项已由本次补齐；其余步骤二待验项继续保留。
