#!/usr/bin/env python3
"""Builds the Windows icon resource of Henosis.exe.

    python3 tools/winicon.py                # from app/

Draws the asterisk logo at every size Windows asks for, and writes:

  agent/favicon.ico                               the window's icon (served by the app)
  cmd/henosis-agent/rsrc_windows_amd64.syso       the .exe file's icon

A .syso file is a COFF object that `go build` links into Windows builds of
the package next to it. It holds one .rsrc section with an RT_ICON for each
size and an RT_GROUP_ICON (id 1) listing them, which is where Explorer, the
taskbar and Alt+Tab take a program's icon from. Needs Pillow; nothing else.
"""
import io
import os
import struct

from PIL import Image, ImageDraw

HERE = os.path.dirname(os.path.abspath(__file__))
APP = os.path.dirname(HERE)

# The logo: the same polygon as the header of agent/page.html (0..100 box).
POINTS = [(36.3, 5.1), (63.7, 5.1), (59.4, 37.1), (88.5, 23.0), (97.0, 49.2), (65.2, 54.9), (87.5, 78.3),
          (65.3, 94.4), (50.0, 66.0), (34.7, 94.4), (12.5, 78.3), (34.8, 54.9), (3.0, 49.2), (11.5, 23.0),
          (40.6, 37.1)]
BACKGROUND = (20, 21, 26, 255)   # the dark theme's background
ACCENT = (255, 138, 61, 255)     # the dark theme's accent
SIZES = [16, 20, 24, 32, 40, 48, 64, 128, 256]


def render(size):
    big = size * 8  # drawn large and scaled down, for smooth edges
    im = Image.new("RGBA", (big, big), (0, 0, 0, 0))
    draw = ImageDraw.Draw(im)
    draw.rounded_rectangle([0, 0, big - 1, big - 1], radius=int(big * 0.22), fill=BACKGROUND)
    pad = big * (0.16 if size >= 32 else 0.10)
    span = big - 2 * pad
    draw.polygon([(pad + x / 100 * span, pad + y / 100 * span) for x, y in POINTS], fill=ACCENT)
    return im.resize((size, size), Image.LANCZOS)


def dib(im):
    """An icon image as Windows stores it below 256 px: a 32-bit DIB of twice
    the height (colour, then an all-clear AND mask), rows bottom-up, BGRA."""
    w, h = im.size
    header = struct.pack("<IiiHHIIiiII", 40, w, h * 2, 1, 32, 0, 0, 0, 0, 0, 0)
    rows = []
    px = im.load()
    for y in range(h - 1, -1, -1):
        rows.append(b"".join(struct.pack("BBBB", *(lambda p: (p[2], p[1], p[0], p[3]))(px[x, y])) for x in range(w)))
    mask_row = ((w + 31) // 32) * 4
    return header + b"".join(rows) + b"\0" * (mask_row * h)


def png(im):
    out = io.BytesIO()
    im.save(out, "PNG", optimize=True)
    return out.getvalue()


def rsrc_section(icons):
    """The .rsrc section: directory tables, data entries, then the data. Each
    data entry's address is section-relative here; a relocation makes the
    linker turn it into an RVA. Returns (bytes, offsets that need relocating)."""
    RT_ICON, RT_GROUP_ICON, LANG = 3, 14, 0x0409
    group = struct.pack("<HHH", 0, 1, len(icons))
    for i, (size, data) in enumerate(icons, start=1):
        b = 0 if size >= 256 else size
        group += struct.pack("<BBBBHHIH", b, b, 0, 0, 1, 32, len(data), i)
    blobs = [data for _, data in icons] + [group]  # data entry i -> blobs[i]

    def directory(n):
        return struct.pack("<IIHHHH", 0, 0, 0, 0, 0, n)

    # Sizes, to lay everything out before writing it.
    n_icons = len(icons)
    root = 16 + 2 * 8
    type_icon = 16 + n_icons * 8
    type_group = 16 + 8
    lang = 16 + 8  # one per resource
    off_type_icon = root
    off_type_group = off_type_icon + type_icon
    off_langs = off_type_group + type_group
    n_res = n_icons + 1
    off_entries = off_langs + n_res * lang
    off_data = off_entries + n_res * 16
    data_offsets = []
    pos = off_data
    for blob in blobs:
        pos = (pos + 7) & ~7
        data_offsets.append(pos)
        pos += len(blob)

    out = bytearray()
    out += directory(2)
    out += struct.pack("<II", RT_ICON, 0x80000000 | off_type_icon)
    out += struct.pack("<II", RT_GROUP_ICON, 0x80000000 | off_type_group)
    out += directory(n_icons)
    for i in range(n_icons):
        out += struct.pack("<II", i + 1, 0x80000000 | (off_langs + i * lang))
    out += directory(1)
    out += struct.pack("<II", 1, 0x80000000 | (off_langs + n_icons * lang))
    for i in range(n_res):
        out += directory(1)
        out += struct.pack("<II", LANG, off_entries + i * 16)
    relocs = []
    for i, blob in enumerate(blobs):
        relocs.append(len(out))  # the OffsetToData field of this entry
        out += struct.pack("<IIII", data_offsets[i], len(blob), 0, 0)
    for i, blob in enumerate(blobs):
        out += b"\0" * (data_offsets[i] - len(out))
        out += blob
    out += b"\0" * ((-len(out)) % 8)
    return bytes(out), relocs


def coff(section, relocs):
    """An x86-64 COFF object with the .rsrc section and one symbol for it."""
    IMAGE_REL_AMD64_ADDR32NB = 3
    header_size, section_header_size = 20, 40
    raw_at = header_size + section_header_size
    relocs_at = raw_at + len(section)
    symbols_at = relocs_at + 10 * len(relocs)
    out = struct.pack("<HHIIIHH", 0x8664, 1, 0, symbols_at, 1, 0, 0)
    out += struct.pack("<8sIIIIIIHHI", b".rsrc", 0, 0, len(section), raw_at, relocs_at, 0, len(relocs), 0,
                       0x40000040)  # initialized data, readable
    out += section
    for at in relocs:
        out += struct.pack("<IIH", at, 0, IMAGE_REL_AMD64_ADDR32NB)
    out += struct.pack("<8sIhHBB", b".rsrc", 0, 1, 0, 3, 0)  # static symbol of section 1
    out += struct.pack("<I", 4)  # an empty string table
    return out


def main():
    images = {size: render(size) for size in SIZES}
    images[256].save(os.path.join(APP, "agent", "favicon.ico"), sizes=[(s, s) for s in (16, 24, 32, 48, 64, 128, 256)])
    icons = [(size, png(images[size]) if size >= 256 else dib(images[size])) for size in SIZES]
    section, relocs = rsrc_section(icons)
    target = os.path.join(APP, "cmd", "henosis-agent", "rsrc_windows_amd64.syso")
    with open(target, "wb") as f:
        f.write(coff(section, relocs))
    print("wrote", os.path.relpath(target, APP), "and agent/favicon.ico")


if __name__ == "__main__":
    main()
