# 950-4 内网功能验收记录（F01–F14）

日期：2026-10-31 部署 + 2026-11-01 验收。环境与二进制清单见 [environment.json](environment.json)。
对照清单：[../20261031-remaining-tests/intranet-handoff.md](../20261031-remaining-tests/intranet-handoff.md)。
原则：只将原始收据支持的子项记为 PASS；失败与受阻如实记录，不伪造通过。

## 结论速览

| ID | 项 | 判定 | 说明 |
|---|---|---|---|
| F01① | kvm_check | **PASS** | `runs/f01-kvm.json` |
| F01② | factory full_logical / tracked_delta + proof | **PASS ×3** | 146.5s / 151.7s / 178.3s，`runs/f01-factory-fl.json` 等 |
| F01③ | marathon（biofabric fixture，1536MiB） | **PASS** | 348.6s，`runs/f01-marathon.json` |
| F01⑤⑥ | m10/m11 模块变体 | **FAIL ×2（均已定位）** | =m 加载路径 ✓。m10：balloon 96MiB 目标物理不可达（内核构建差异，非产品 bug）；m11：disk_check 误报——字节级证据（两例 4KiB 对齐 prefix-new/suffix-old、零垃圾）证明为**合法在途分裂写被冻结**，oracle 对 64KiB 应用写假设原子性有误，**非 FC 快照缺陷**；维持 FAIL 不改判，修复方向 = quiesce oracle。详见 m10/m11 节 |
| F02 | compat 全量 | **PASS** | 166.5s，`runs/f02-compat.json`；含 truncate/hardlink/rename/FD+mmap/sendfile/splice/锁/解包 + B0/B/C 恢复 |
| F03 | guest cpu/raw-block 攻击 | **PASS**（测试断言口径） | compat 内 `sharingCompatAttack`：写拒绝 + lower 全 hash 不变 + 观察终态同源；`*-attack.json` 在 run 目录 |
| F04① | vsock | **PASS** | `runs/f04-vsock.json`（注意 guest 二进制须 `vsock_guest`） |
| F04② | HTTP 短读/延迟 | **PASS ×2** | 98.2s / 67.3s，`runs/f04-shortread.json` `runs/f04-delay.json` |
| F05 | multiregion | **PASS** | `runs/f05-multiregion.json` |
| F06 | 双 VM 持续压力 | **受阻** | 950-4 cgroup v1 混合模式：`memory.swap.max`/`memory.pressure` 不存在，pressure_check.py 首步失败；需 cgroup v2 统一层次环境 |
| F07 | seccomp | **部分 PASS** | glibc：`E2B_SHARING_REQUIRE_SECCOMP=1` 叠加 factory PASS（131.1s，`runs/f07-seccomp.json`，断言每线程 Seccomp=2 且 filter>基线）。musl 静态 FC 与 jailer chroot/降权运行态未验收（musl target 未安装） |
| F08 | swagger 生成回归 | **FAIL** | 固定版本 `v0.33.2-0.20260129162912-5a56fc6f5a75` 生成 rc=0，但与提交代码漂移 142 行且重生成树 `go test ./pkg/fc/...` 编译失败——**分支手改的生成代码不可再生**，上游交付缺口。`runs/f08/test.json` + `logs/f08-generate4.log` + `runs/f08/generated-diff4.txt` |
| F09 | 新旧快照格式兼容 | **FAIL（真实发现）** | 旧 FPR fork FC 快照 → 新 FC restore 报 `UnexpectedVariant { type_name: "Option<T>", allowed: Range{0,1}, found: 2 }`（FPR 私有序列化扩展超上游 v8 枚举）→ **old→new 单向不可迁移，模板必须新栈重建（已执行，5/5 PASS）**。`runs/f09-snapshot-version.json` |
| F10 | race + 历史删除 | **PASS** | 六包 `-race` ok + `TestHistoryObjectDeletionRejectsUnreadAncestor` PASS；`logs/f10-race2.log` |
| F11① | ENOSPC 物化 | **PASS** | 16MiB tmpfs 私有 mount ns，失败不发布部分镜像 |
| F11② | 真实云对象存储 | **受阻** | 无凭据/端点，且测试硬编码 `STORAGE_PROVIDER=Local`（需改码才能接 S3） |
| F12 | 1052 原始 trace | **受阻** | 资产（trajectory/claude-code.txt/任务环境）只在本地 inventory，需 trace 正本持有方提供 |
| F13 | PMEM 自动构建链 | **受阻（功能未实现）** | TM 构建→PMEM lower/manifest 自动发布流水线上游未接入；本轮为手动 fixture + 手动模板重建验证 |
| F14 | 证据归档/清理审计 | **本记录** | 收据/日志路径见下；清理审计见文末 |

## 真实场景验证（部署闭环）

1. **mem-oc 5 模板新栈重建**：5/5 BUILD SUCCESS，metadata 全部可解析，5/5 切片快照，migrate-06 verify 5/5 PASS（`/home/y00960968/e2b-mem-oc/logs/migrate-06b.log`）
2. **模板必须重建的根因**：F09（上表）——旧 FPR 快照与新 FC 序列化不互认

## 产品 bug 证据链（本轮发现→修复→生产验证）

### Bug 1：fsObject.Put/StoreFile 无 O_TRUNC

- **发现**：deploy-02 后重建模板，5/5 报 `error unmarshaling template version: invalid character ',' after top-level value`
- **根因**：`packages/shared/pkg/storage/storage_fs.go` Put/StoreFile 对已存在文件用 O_WRONLY|O_CREATE 打开，第二次上传更短 JSON 留脏尾
- **修复**：新增 `getWriteHandle()`（O_RDWR|O_CREATE|O_TRUNC），Put/StoreFile 改用（分支 `feat/virtio-pmem-dax-20261030` 提交 `eb976079c`）
- **验证**：修复后 5/5 模板 metadata 可解析 + F10 race 包含 metadata 包单跑 ok

### Bug 2：AcquireMemoryView typed-nil 接口

- **发现**：F10 `TestHistoryObjectDeletionRejectsUnreadAncestor` nil receiver Close panic
- **根因**：`packages/orchestrator/internal/sandbox/template/storage.go:164` 错误路径 `return d.source.Pin(ctx)` 把 typed-nil `(*PinnedFile)(nil)` 包进 `block.ReadonlyDevice`，调用方 `view != nil` 判真
- **修复**：错误路径返回字面 nil（同提交）
- **验证**：该测试 PASS

### Bug 3：NBDProvider.Close 死锁（验收发现，**已修复并验证**，提交 `89db9866a` + `738be04c5`）

- **发现**：f02-compat 首跑（layout/lower sha 错配输入）CreateSandbox 出错后挂死 15 分钟
- **根因**：`sandbox.go` 创建错误路径在 `go Start(...)` 启动**之前**调 `rootfsProvider.Close`；Close→sync→`Path()`→`SetOnce.Wait` 在 NBD slot 从未赋值时永久阻塞，真实错误被死锁吞掉
- **修复**：① `NBDProvider` 增加 `startAttempted`（Start 首行置位）；`sync()` 对 never-started provider 短路返回（设备未起来、guest I/O 不可能存在，无物可 flush）② `DirectPathMount.close()` 对 nil devicePool 加守卫（测试构造暴露的解引用）。生产语义不变：started 的正常 teardown 路径 flush 照旧
- **验证**：单元测试 `rootfs/nbd_test.go`（Close/sync 不再死锁）2/2 PASS；race 包含 rootfs PASS；**端到端回归 `runs/b03-regression`**——同错配输入（compat layout + pmem lower）从 15m 挂死变为 **4.67s 干净失败**（`pmem lower SHA256 mismatch`，rc=1）；正常路径 f02-compat 重跑 PASS（166.5s，rc=0，`logs/faccept-03-f02-fixverify.log`）

### Bug 4：模板 metadata 发布竞争 + 并发 Put 无锁（用户复核提出，代码证实，**已修复并验证**，提交 `89db9866a`）

- **竞争窗口**：`layer_executor.go:286` finalize 阶段 snapshot.Upload（含原始 metadata Put）在 `UploadErrGroup` 上**异步**执行；`optimize/builder.go` updateMetadata（带 prefetch mapping 的新 metadata Put）**同步**执行；等待点 `builder.go:362 bc.UploadErrGroup.Wait()` 在 `phases.Run`（含 optimize）**之后**——旧 metadata 可晚于新版本落盘，**覆盖掉 prefetch mapping**。第三条写路径 `server/sandboxes.go:738` 运行期 UploadMetadata 同样无定序
- **并发写损坏**：`storage_fs.go` Put 原地 open+write **无锁**。O_TRUNC 只保证单写者干净覆盖；两个并发 Put 各持独立 fd、open 时各自 truncate、独立 offset 交错写 → **torn JSON**。O_TRUNC 修复不解决此问题
- **修复**：① 定序——`OptimizeBuilder.Build` 开头先 `bc.UploadErrGroup.Wait()`，保证所有层上传（含原始 metadata）完成后 optimize 才写终版 metadata（终版必然最后落盘）② 原子写——`fsObject.Put/StoreFile` 改为 temp 文件 + rename（同目录 CreateTemp→write→sync→chmod 0644→rename），并发写者不再可能交错撕裂，且取代原 O_TRUNC 修复；`getWriteHandle` 删除。`resume-build` 工具为同步上传无并发 optimize，不受影响
- **验证**：单元测试 `storage_fs_test.go`（50 轮双写者并发 Put 后 JSON 完整、短写无脏尾）PASS；race 含 storage 包 PASS；`compile-04` 重编 orchestrator-pmdax OK（sha256 `ecdf6490e1f2…`，未替换现役组件）
- **遗留**：`server/sandboxes.go:738` 运行期 metadata 更新与 build pipeline 的跨流程定序仍无世代号守卫（原子写已消除撕裂，但 last-writer-wins 语义层面依赖上游加版本号，暂记录不实施）

## m10/m11 模块变体（F01⑤⑥，2026-11-01）——m10 FAIL / m11 FAIL（真实发现）

- **内核变体**：归档 config（vmlinux-6.6.60-e2b-pmem 同款）基础上把 `VIRTIO_MMIO/VIRTIO_BALLOON/VIRTIO_BLK/VIRTIO_NET/NET_FAILOVER(+FAILOVER core)` 改 `=m`，`make Image modules`（fprep-07 + 全量重链修 modpost 双导出）。产物 `kernel/mvar/Image`（sha256 `aecc9c3e4e8a7b2fb5c9dc118aa44be24e87a15b2e828a154fe3f91ca43efc71`）+ 810 `.ko`，模块 sha 记录于 `runs/f01-m10/module-hashes.json`
- **重跑验证（f01-m11-r2，2026-11-01）**：2/2 复现但失败点不同——首跑在 B1-nbd-restored（m11_check.py:216），r2 在 B1-nbd 第 2 轮迭代冻结（:208）；撕裂记录位不同（worker=0/slot=21/seq=149 vs worker=1/slot=54/seq=118）。B0-file 两次全过。收据 `runs/f01-m11-r2/` + `logs/f01-m11-r2.log`
- **m11 根因定位（2026-11-01 晚，字节级证据闭环）**：利用 `deliveries/20261101-nbd-m11-review/`（外部复核 + `inspect_record.py` 只读槽分析器）分析两张真实失败盘，**两例同指纹**——worker=0/slot=21：前 8KiB payload=seq 149（新记录完整正确），后 56KiB payload=seq 85（上一代记录完整正确），分割点精确 4KiB 对齐；worker=1/slot=54：前 24KiB=seq 118，后 40KiB=seq 54，同样 4KiB 对齐。**零垃圾字节，两段分别是完整合法的新旧两代记录** → 这是**在途应用写被冻结的确定性指纹**：guest 64KiB O_DIRECT Pwrite 无 SEG_MAX 时被块层分割为多个 4KiB 子请求，冻结时部分子请求已完成落盘、其余尚在 guest/avail 队列（合法 checkpoint 中间态；PAUSED 下采样"稳定"只证明不再变化）。**m11 disk_check 对未完成的 64KiB 应用写假设原子性 → 误报，非 FC 快照/持久化缺陷**。与 `prepare_save` 有 `drain_and_flush` 的源码事实自洽；恢复后迭代继续全检通过（B0/B1 iteration 0/1）隐含 pending 子请求恢复后正常完成。分析收据 `runs/*/inspect-*.json`（inspect_record.py sha256 `5491c924…`，公式与 m11_guest.go:39-41 逐项核对一致）
- **m11 处置**：F01⑥ 维持 FAIL **不改判 PASS**（按复核要求：字节指纹支持误报解释，但缺"恢复后补齐+读回正确"的直接演示）；正确修复 = oracle 加 quiesce 握手（静止验收：worker 完成本次 Pwrite/Pread 后停发新请求、全 ACK、同步再冻结，此时全记录 disk_check 必须全过）+ 并发模式区分 active/committed 槽。**性能影响结论修正**：对压测无阻碍——快照/恢复机制本身有效，此前"B1-nbd 恢复路径影响磁盘一致性"的怀疑撤回；后续 NBD 后端压测数据可用
- **m10（balloon discard/oracle）FAIL**：模块加载成功（`/proc/modules`：virtio_balloon/virtio_mmio Live，virtio0 绑定 ✓），但 `PATCH /balloon amount_mib=96` 30s 超时——**本内核构建 guest 可见内存仅 88.8MiB/192MiB**（agent03 对照为发行内核 6.6.0-95 的 159.3MiB/192MiB），96MiB 目标物理不可达，根因是该 config 下 ~103MiB boot 保留。收据 `runs/f01-m10/result.json` + `runs/f01-m10/balloon-initial.json`
- **m11（模块顺序+冻结/恢复 oracle）FAIL → oracle 误报（见上"根因定位"）**：模块链加载与前序 B0 冻结/采样全通过；`disk_check` 报 torn 的记录实为合法在途分裂写。收据 `runs/f01-m11/` + `logs/f01-m11.log`。另注：m11 复核材料同时指出 FC `process_async_completion_queue` 部分错误只记日志不上抛（快照准备路径的错误传播缺口）——**源码可见缺口，无证据触发本次失败**，记录为上游改进项
- 两项定性更新：m10 = 内核构建差异（非产品 bug）；m11 = oracle 假设错误（非产品 bug，处置见上）。=m 加载路径本身可行

## 收据与日志索引（服务器 950-4）

- 收据 JSON：`/home/y00960968/pmem-accept/runs/`（每用例一份：rc、时间、日志路径）
- 日志：`/home/y00960968/pmem-accept/logs/`（f01-* f02-* f04-* f05-* f07-* f08-* f09-* f10-*）
- 测试原始 run 目录：`/home/y00960968/pmem-accept/runs/<case>/`（checkpoint/attack/seccomp/proof receipt 由测试写入）
- fixture：`/home/y00960968/pmem-accept/fixture-find/`（pmem + compat）、`fixture-biofabric/`（pmem）
- 复现入口：`/home/y00960968/e2b-compile/faccept-0{1,2,3}-*.sh`、`fprep-0{1..7}-*.sh`（本地正本 `enviroments/950-4/e2b-compile/`）

## 已知边界（如实声明）

1. sandbox.test 为 compat 超时预算做了一行 harness 改动：`sharing_integration_linux_test.go:102` `if marathonMode || compatMode { duration = 15 * time.Minute }`（950-4 较慢，8 分钟预算不够 B0 链 + compat 阶段；重建后 sha256 前 16 位 `c929ce75d8a3a3c0`）。VM 配置/marathon 分支未动。
2. F03 的通过口径是测试断言（写拒绝 + hash 不变 + 终态同源）；"实际 CPU store 到达 KVM 只读 slot" 的 guest helper 证据链仍属交接要求，未另做。
3. 远端故障（F04②）是 httptest seam 注入，非真实对象存储。
4. F08 重生成后服务器 FC 源码树已从 `/tmp/fc-pre.tar` 恢复原状。

## 清理审计

- 集成测试自建 `ns-2500x`/`veth-2500x`：faccept-03 每用例前置清理 + 收尾核查 0 残留（panic 场景已手工清）
- NBD 设备：测试经 kernel IndexAny 分配并随用例 Close 释放；无跨用例复用
- 本轮创建目录：`pmem-accept/`（验收产物，保留）、`e2b-compile/src/*-pmdax`（源码树，保留）、`e2b-mem-oc/`（模板重建产物，保留）；未触碰 `/opt/e2b-infra` 现役部署之外的系统资源
- 生产沙箱全部经 E2B API kill，无 pkill firecracker
