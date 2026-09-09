# File page cache 物化失败：复现与修复

检查起点：`mem-overcommit-exp-file-page-cache` 的 `2cf47d871`，包含公共无准入基线
`72f23a1f4`。以下为 Linux ARM64 Go 1.25.4 容器内的代码回归，未部署目标宿主。

## 精确复现

`TestMaterializeMemoryImageLayeredRanges` 使用真实的 `MaterializeMemoryImage →
template.Storage → build.File → StorageDiff → FullFetchChunker → Cache` 路径，
上游为临时目录中的本地存储；不依赖 NBD、Firecracker 或对象存储服务。

构造 4 KiB header、8 MiB 逻辑镜像和差分层映射，使逻辑偏移 `4194304`
映射为存储偏移 `3956736`、长度 `790528`。原版得到：

```text
read memory image at offset 4194304: failed to read from source:
failed to slice cache at 3956736-4747264:
failed to read from cache after ensuring data at 3956736-4747264:
The requested bytes are not available on the device
```

## 两个不同的范围问题

1. **FullFetchChunker 漏取尾部 chunk。** 旧代码按 `ceil(length / 4MiB)` 计算取块数，
   未计入请求起点在 chunk 内的偏移。本例长度不足 4 MiB，但范围横跨两个 chunk；
   原版只读偏移 `0`，还应读取偏移 `4194304`。改为从向下对齐的起点遍历到
   `min(off + length, size)`，并保持最后一个 chunk 的文件尾裁剪。
2. **Cache 查询键使用了请求原点。** `isCached` 原先查 `off + relativeBlockOff`，
   当 off 未对齐到 `cache.blockSize` 时无法匹配已发布的绝对块起点。改为遍历
   请求覆盖的所有绝对块起点，同时处理零长度与 EOF 裁剪。

必须区分 4 MiB 传输 chunk 与 header 的缓存块大小。本例 `3956736 = 966 × 4096`，
若 header 是 4 KiB，写入首个 4 MiB 会发布 `{0, 4096, ..., 4190208}`，包括
`3956736`，并非只有 `{0}`。因此这组有效 4 KiB 输入的直接失败原因是第 1 项。
将缓存块改为 4 MiB，或请求任意块内偏移，则可以独立复现第 2 项。

只修复第 1 项时，本例通过，而两个块内未对齐用例仍失败，证明两者相互独立。
流式 chunker 已按首尾覆盖范围取 chunk，但共享同一个 Cache，也受第 2 项影响。

`setIsCached` 保留按完整块发布的契约；不能简单把部分写入向外对齐后标成已缓存，
否则会暴露尚未写入的字节。该修复规范读取覆盖范围，不改变写入发布语义。

这不是函数内部的无限循环：`Slice` 在 ensure 后再检查一次并返回错误，物化函数
也立即返回。外层重复尝试可能让同一错误持续出现。

## 同一调用链发现的零页问题

`build.File.ReadAt` 遇到 `uuid.Nil` 映射会跳过写入，要求调用方提供零填充缓冲区。
物化代码原先循环复用 4 MiB 缓冲区；第二轮的零页范围会残留第一轮的数据。
完整回归中的 sparse-tail 用例验证了这一内容错误。

现在每轮读取前 `clear(chunk)`。同时将物化引用从 `<key>.ref` 切换为
`<key>.v2.ref`，让旧缓存通过正确路径重新物化。旧错误内容也可能有自洽的 SHA256，
仅检验旧文件自身哈希不能发现逻辑内容错误。旧引用和 `.raw` 文件保留，避免影响
仍映射这些文件的 VM；现存 VM 不会自动切换到新镜像。

## 验证与目标宿主复测

原版最小范围测试和完整物化测试均先失败；修复后相关 race 回归通过：

```bash
go test -race -count=1 \
  ./packages/orchestrator/internal/sandbox/block \
  ./packages/orchestrator/internal/sandbox/template \
  -run 'TestFullFetchChunker_UnalignedRange|TestCache_Unaligned|TestStreamingChunker_|TestMaterializeMemoryImage|TestFileMemoryBackendPath' \
  -timeout 2m
```

覆盖本例跨 chunk 读取、块内读取、跨缓存块时不能提前命中、EOF、重复读取复用
缓存、原流式读取回归、多层镜像的完整内容哈希、零页尾部和旧引用迁移。
Orchestrator 的 Linux ARM64 构建通过（不带 Mooncake 标签）。

目标宿主仍需用原失败模板重新物化，记录实际 header blockSize、chunker 模式、
成功生成的 `.v2.ref` 和内容验证，再执行 Firecracker 创建与工作负载回归。
此处回归不构成目标宿主 OOM、密度或 SLA 测量结果。
