"""Render the README screenshots.

Runs the frame generator (internal/ui TestWriteScreenshots, synthetic demo data)
and draws each frame, colours included, into docs/screenshots/*.png.

    python tools/screenshots.py

Needs Pillow and a monospace font with box-drawing characters (Cascadia Mono,
DejaVu Sans Mono or Consolas are tried in that order; --font PATH to choose).
"""
import argparse
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parent.parent
BG, FG = (13, 17, 23), (201, 209, 217)
FONTS = [
    "C:/Windows/Fonts/CascadiaMono.ttf",
    "/usr/share/fonts/truetype/cascadia-code/CascadiaMono.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
    "C:/Windows/Fonts/consola.ttf",
]
BASE16 = [(0, 0, 0), (205, 49, 49), (13, 188, 121), (229, 229, 16), (36, 114, 200), (188, 63, 188),
          (17, 168, 205), (229, 229, 229), (102, 102, 102), (241, 76, 76), (35, 209, 139), (245, 245, 67),
          (59, 142, 234), (214, 112, 214), (41, 184, 219), (255, 255, 255)]
# characters drawn as rectangles so bars and frames join without seams:
# (x0, y0, x1, y1) as fractions of the cell
EIGHTHS = {c: (0, 1 - (i + 1) / 8, 1, 1) for i, c in enumerate("▁▂▃▄▅▆▇█")}
SGR = re.compile(r"\x1b\[([0-9;]*)m")


def xterm(n):
    if n < 16:
        return BASE16[n]
    if n < 232:
        n -= 16
        steps = [0, 95, 135, 175, 215, 255]
        return (steps[n // 36], steps[n // 6 % 6], steps[n % 6])
    v = 8 + (n - 232) * 10
    return (v, v, v)


def cells(line):
    """Yield (char, colour, bold) for one line of text with SGR escapes."""
    colour, bold, pos = FG, False, 0
    for m in SGR.finditer(line):
        for ch in line[pos:m.start()]:
            yield ch, colour, bold
        pos = m.end()
        codes = [int(c) for c in m.group(1).split(";") if c] or [0]
        i = 0
        while i < len(codes):
            c = codes[i]
            if c == 0:
                colour, bold = FG, False
            elif c == 1:
                bold = True
            elif c == 22:
                bold = False
            elif c == 39:
                colour = FG
            elif 30 <= c <= 37:
                colour = BASE16[c - 30]
            elif 90 <= c <= 97:
                colour = BASE16[c - 90 + 8]
            elif c == 38 and i + 2 < len(codes) and codes[i + 1] == 5:
                colour = xterm(codes[i + 2])
                i += 2
            elif c == 38 and i + 4 < len(codes) and codes[i + 1] == 2:
                colour = tuple(codes[i + 2:i + 5])
                i += 4
            i += 1
    for ch in line[pos:]:
        yield ch, colour, bold


def draw(text, font, bold_font, size):
    lines = text.split("\n")
    cols = max(len(SGR.sub("", l)) for l in lines)
    cw = round(font.getlength("M"))
    ascent, descent = font.getmetrics()
    chh = ascent + descent
    pad = size
    img = Image.new("RGB", (cols * cw + 2 * pad, len(lines) * chh + 2 * pad), BG)
    d = ImageDraw.Draw(img)
    for row, line in enumerate(lines):
        y = pad + row * chh
        for col, (ch, colour, bold) in enumerate(cells(line)):
            x = pad + col * cw
            if ch == " ":
                continue
            if ch in EIGHTHS:
                x0, y0, x1, y1 = EIGHTHS[ch]
                d.rectangle([x + x0 * cw, y + round(y0 * chh), x + x1 * cw - 1, y + chh - 1], fill=colour)
            elif ch == "░":
                dim = tuple((c + 3 * b) // 4 for c, b in zip(colour, BG))
                d.rectangle([x, y, x + cw - 1, y + chh - 1], fill=dim)
            else:
                d.text((x, y), ch, font=bold_font if bold else font, fill=colour)
    return img


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--font")
    ap.add_argument("--size", type=int, default=16)
    ap.add_argument("--out", default=str(ROOT / "docs" / "screenshots"))
    a = ap.parse_args()
    path = a.font or next((f for f in FONTS if os.path.exists(f)), None)
    if not path:
        sys.exit("no monospace font found; pass --font PATH")
    font = ImageFont.truetype(path, a.size)
    bold = font
    try:
        bold = ImageFont.truetype(path, a.size)
        bold.set_variation_by_name("Bold")
    except Exception:
        bold = font
    out = Path(a.out)
    out.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as tmp:
        env = dict(os.environ, LLAMADASH_SHOTS=tmp)
        subprocess.run(["go", "test", "-count=1", "-run", "TestWriteScreenshots", "./internal/ui/"],
                       cwd=ROOT, env=env, check=True)
        for f in sorted(Path(tmp).glob("*.ans")):
            img = draw(f.read_text(encoding="utf-8"), font, bold, a.size)
            img.save(out / (f.stem + ".png"), optimize=True)
            print(out / (f.stem + ".png"), img.size)


if __name__ == "__main__":
    main()
