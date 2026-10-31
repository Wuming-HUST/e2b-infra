#!/usr/bin/env python3
"""Offline Linux 6.6 nd_pfn_sb v1.4, PFN_MODE_RAM namespace preparation.

The filesystem starts at 2 MiB. Struct pages are allocated in each guest's
ordinary RAM, never initialized in the shared readonly namespace. Layout:
Linux drivers/nvdimm/pfn.h, pfn_devs.c and nd_sb_checksum (Fletcher64).
"""
import hashlib
import os
from pathlib import Path
import shutil
import struct
import sys

DATA_OFFSET = 2 << 20
SECTION = 128 << 20


def fletcher64(data):
    lo = hi = 0
    for (word,) in struct.iter_unpack("<I", data):
        lo = (lo + word) & 0xffffffff
        hi = (hi + lo) & 0xffffffff
    return (hi << 32) | lo


def pack(source, target):
    with source.open("rb") as src:
        source_size = os.fstat(src.fileno()).st_size
        sb = src.read(2048)
        if len(sb) < 2048 or struct.unpack_from("<H", sb, 1080)[0] != 0xef53 or struct.unpack_from("<I", sb, 1048)[0] != 2:
            raise ValueError("source must be an offline ext4 filesystem with 4K blocks")
        if struct.unpack_from("<I", sb, 1120)[0] & 4:
            raise ValueError("source needs journal recovery")
        src.seek(0)
        identity = hashlib.file_digest(src, "sha256").digest()[:16]
        size = ((source_size + DATA_OFFSET + SECTION - 1) // SECTION) * SECTION
        header = bytearray(4096)
        struct.pack_into("<16s16s16sIHHQQIIIIIH", header, 0,
                         b"NVDIMM_PFN_INFO\0", identity, bytes(16), 0, 1, 4,
                         DATA_OFFSET, (size - DATA_OFFSET) // 4096,
                         1, 0, 0, DATA_OFFSET, 4096, 64)
        struct.pack_into("<Q", header, 4088, fletcher64(header))
        # Refuse to overwrite an image that might already have live mappings.
        with target.open("xb") as dst:
            dst.truncate(size)
            dst.seek(4096)
            dst.write(header)
            dst.seek(DATA_OFFSET)
            src.seek(0)
            shutil.copyfileobj(src, dst, 4 << 20)
            dst.flush()
            os.fsync(dst.fileno())


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: pack-pfn.py OFFLINE_EXT4 NEW_NAMESPACE_IMAGE")
    pack(Path(sys.argv[1]), Path(sys.argv[2]))
