"""Writes placeholder Teams icons (192x192 color, 32x32 transparent outline) without dependencies.
Replace them with real artwork before publishing to the org catalog."""
import struct
import sys
import zlib


def png(path, size, pixel):
    rows = b"".join(b"\x00" + b"".join(pixel(x, y) for x in range(size)) for y in range(size))
    def chunk(tag, data):
        return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
    with open(path, "wb") as f:
        f.write(b"\x89PNG\r\n\x1a\n")
        f.write(chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0)))
        f.write(chunk(b"IDAT", zlib.compress(rows, 9)))
        f.write(chunk(b"IEND", b""))


out = sys.argv[1] if len(sys.argv) > 1 else "."
blue, white, clear = b"\x2f\x5b\xea\xff", b"\xff\xff\xff\xff", b"\x00\x00\x00\x00"
png(f"{out}/color.png", 192, lambda x, y: white if 56 <= x < 136 and 56 <= y < 136 and not (72 <= x < 120 and 72 <= y < 120) else blue)
png(f"{out}/outline.png", 32, lambda x, y: white if 6 <= x < 26 and 6 <= y < 26 and not (10 <= x < 22 and 10 <= y < 22) else clear)
