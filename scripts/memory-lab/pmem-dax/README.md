# PMEM/DAX 模板制作

原始模板仍是 ext4。namespace 是 Linux 持久内存设备的存储范围；本实现的 namespace 镜像在 ext4 外增加离线 PFN 元数据和对齐空间，不是另一种文件系统，也不是 Linux 进程 namespace。

```text
原模板 rootfs.ext4: [ext4 文件系统]
新 lower.raw:       [2 MiB 元数据/保留区][ext4 文件系统][补齐至 128 MiB 倍数]
每 VM upper.ext4:   [ext4: upper/ + work/]
应用看到的 /:       OverlayFS(lower + upper)
```

`pack-pfn.py` 使用 Linux 6.6 的 `nd_pfn_sb` v1.4、`PFN_MODE_RAM`。PFN superblock 在 namespace 内偏移 4096；文件系统起点为 2 MiB。`struct page` 在各 VM 的普通 RAM 中分配，普通 RAM 快照负责保存；共享 lower 全程只读。文件系统自身的 block/ inode 偏移不变，只有相对整个 backing 的偏移增加 2 MiB。

裸 ext4 经 virtio-pmem 发现并不保证 DAX 可用：agent03 的首轮启动发现 pmem，但缺少 PFN 映射时报告 `DAX unsupported by block device`。离线准备 PFN namespace 后，实际文件的 `STATX_ATTR_DAX` 校验及整机快照恢复通过。这里不使用 `FS_DAX_LIMITED`。

## 制作

在隔离 Linux ARM64 环境准备 Go、Python 3.11+、e2fsprogs、coreutils。源镜像必须已经停止写入且卸载；工具不能检测其他进程的全部映射。

```sh
cd packages/orchestrator
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /absolute/path/pmem-init ./cmd/pmem-init
cd ../..
scripts/memory-lab/pmem-dax/prepare-rootfs.sh \
  /absolute/path/quiescent-rootfs.ext4 /absolute/path/pmem-init /absolute/path/new-template
```

工具保留源文件，在新副本中离线关闭 journal、设置 lower label，然后封装；`pack-pfn.py` 对输入 ext4 的字节原样复制。`source.json` 记录原模板 hash，`layout.json` 的 hash/size 覆盖完整 namespace（包含元数据及 padding）。不要混用原 ext4 hash 和 namespace hash。`lower.ext4` 是转换后的文件系统中间产物，运行时绑定 `lower.raw`。

输出还包括全新的 128 MiB 私有 `upper.ext4`、包含静态 early init 的 `initrd.gz`。模板空间需求较大时应在制作时调整 upper 大小；不能截断正在使用的 lower 或 upper。可设置 `E2B_PMEM_ADD_FIXTURE=1` 加入集成测试文件，仅供测试副本。

新 kernel 必须启用 4 KiB 页、`VIRTIO_PMEM/ZONE_DEVICE/LIBNVDIMM/NVDIMM_PFN/BLK_DEV_PMEM/FS_DAX/EXT4_FS/OVERLAY_FS`，启动所需驱动须内建。已测试 Linux 6.6.60 的完整配置位于交付证据 `kernel-build/out/.config`。旧 Debian 6.1 r4 配套内核缺少必要配置，不能直接替代。

## 启动与恢复合同

生产功能开关 `PMEM_ROOTFS_ENABLED=true`；`PMEM_ROOTFS_CACHE_DIR` 指定完整物化缓存。通过 `template.NewPmemTemplate` 提供 layout、lower source 和新 upper；cold boot 后生成新的运行态快照。现有模板构建服务尚未自动执行此离线转换流程。

Firecracker preboot `PUT /pmem` 绑定内容 hash、完整 backing 大小、独立 GPA、只读 host path；本格式 GPA 为 `1<<38`，`data_offset=2097152`。PMEM 窗口从 namespace 起点映射，guest 识别 PFN 后的块设备从文件系统起点暴露。early init 校验实际 resource/size，挂载 `ro,noload,dax=always` lower 和独立 upper，组成 OverlayFS 后执行原 init。应用绝对路径保持不变。

快照 metadata v3 关联 lower layout、私有 upper diff、普通 RAM 与设备状态。恢复先校验/完整物化 lower，再按快照 GPA 重绑；不重新挂载文件系统。旧运行态快照不能通过改 backing 路径转换为新格式。R2 直接 NBD-backed PMEM 尚未实现；R1 缓存当前保留文件，不实现在线淘汰。

## 测试

```sh
python3 -B -m unittest discover -s scripts/memory-lab/pmem-dax -p test_pack_pfn.py -v
cd packages/orchestrator
go test -race ./internal/sandbox/pmem ./internal/sandbox/fc ./internal/sandbox/template ./internal/template/metadata ./cmd/pmem-init
```

Linux KVM 整机测试为 `TestPmemFactoryIntegration`，默认跳过；必须显式提供隔离目录、FC、kernel、upper、initrd、lower、layout 与 trace slice。已执行命令和结果见交付目录 `deliveries/page-sharing/20261030-step2`。
