import importlib.util
from pathlib import Path
import struct
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("pack_pfn", Path(__file__).with_name("pack-pfn.py"))
packer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packer)


class PfnNamespaceTest(unittest.TestCase):
    def test_preserves_filesystem_and_uses_private_ram_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            src, dst = Path(directory)/"fs", Path(directory)/"namespace"
            data = bytearray(8192)
            struct.pack_into("<H", data, 1080, 0xef53)
            struct.pack_into("<I", data, 1048, 2)
            data[4096:4111] = b"filesystem-data"
            src.write_bytes(data)
            packer.pack(src, dst)
            self.assertEqual(dst.stat().st_size, 128 << 20)
            with dst.open("rb") as f:
                f.seek(2 << 20)
                self.assertEqual(f.read(len(data)), data)
                f.seek(4096)
                header = bytearray(f.read(4096))
            self.assertEqual(header[:16], b"NVDIMM_PFN_INFO\0")
            self.assertEqual(header[32:48], bytes(16))
            self.assertEqual(struct.unpack_from("<HHQQI", header, 52),
                             (1, 4, 2 << 20, ((128 << 20)-(2 << 20))//4096, 1))
            checksum, = struct.unpack_from("<Q", header, 4088)
            header[4088:] = bytes(8)
            # Independent Fletcher64 formulation over little-endian words.
            words = struct.unpack("<1024I", header)
            expected = (sum((1024-i)*w for i,w in enumerate(words)) & 0xffffffff) << 32
            expected |= sum(words) & 0xffffffff
            self.assertEqual(checksum, expected)
            self.assertEqual(src.read_bytes(), data)
            with self.assertRaises(FileExistsError):
                packer.pack(src, dst)

    def test_rejects_non_ext4_and_unrecovered_journal(self):
        with tempfile.TemporaryDirectory() as directory:
            src, dst = Path(directory)/"fs", Path(directory)/"namespace"
            src.write_bytes(bytes(8192))
            with self.assertRaises(ValueError):
                packer.pack(src, dst)
            data = bytearray(src.read_bytes())
            struct.pack_into("<H", data, 1080, 0xef53)
            struct.pack_into("<I", data, 1048, 2)
            struct.pack_into("<I", data, 1120, 4)
            src.write_bytes(data)
            with self.assertRaises(ValueError):
                packer.pack(src, dst)
            self.assertFalse(dst.exists())


if __name__ == "__main__":
    unittest.main()
