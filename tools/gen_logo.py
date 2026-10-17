#!/usr/bin/env python3
"""Renders the Skien Mails app icons (favicon.svg + PNGs for the web app manifest)."""
import pathlib
from PIL import Image, ImageDraw

OUT = pathlib.Path(__file__).resolve().parent.parent / "web/static/icons"
OUT.mkdir(parents=True, exist_ok=True)
C1, C2 = (11, 87, 208), (76, 141, 246)

SVG = """<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">
<defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#0b57d0"/><stop offset="1" stop-color="#4c8df6"/></linearGradient></defs>
<rect width="64" height="64" rx="16" fill="url(#g)"/>
<rect x="13" y="19" width="38" height="27" rx="5" fill="#fff"/>
<path d="M15 22.5 32 34.5 49 22.5" fill="none" stroke="#0b57d0" stroke-width="3.6" stroke-linecap="round" stroke-linejoin="round"/>
<circle cx="49" cy="19" r="6.5" fill="#fbbc04" stroke="#fff" stroke-width="2.5"/>
</svg>"""
(OUT / "favicon.svg").write_text(SVG)

def render(size, maskable=False, radius=True):
    S = size * 4
    img = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    grad = Image.new("RGBA", (S, S))
    gd = ImageDraw.Draw(grad)
    for i in range(S * 2):
        t = i / (S * 2)
        col = tuple(int(C1[k] + (C2[k] - C1[k]) * t) for k in range(3)) + (255,)
        gd.line([(i, 0), (0, i)], fill=col, width=2)
    mask = Image.new("L", (S, S), 0)
    md = ImageDraw.Draw(mask)
    if maskable or not radius:
        md.rectangle([0, 0, S, S], fill=255)
    else:
        md.rounded_rectangle([0, 0, S - 1, S - 1], radius=int(S * 0.25), fill=255)
    img.paste(grad, (0, 0), mask)
    d = ImageDraw.Draw(img)
    scale = 0.72 if maskable else 1.0      # keep the glyph inside the maskable safe zone
    off = S * (1 - scale) / 2
    u = S / 64 * scale
    P = lambda x, y: (off + x * u, off + y * u)
    d.rounded_rectangle([P(13, 19), P(51, 46)], radius=5 * u, fill=(255, 255, 255, 255))
    d.line([P(15, 22.5), P(32, 34.5), P(49, 22.5)], fill=C1 + (255,), width=int(3.6 * u), joint="curve")
    for x, y in (P(15, 22.5), P(49, 22.5)):
        r = 1.8 * u
        d.ellipse([x - r, y - r, x + r, y + r], fill=C1 + (255,))
    cx, cy = P(49, 19)
    r1, r2 = 6.5 * u + 1.25 * u, 6.5 * u - 1.25 * u
    d.ellipse([cx - r1, cy - r1, cx + r1, cy + r1], fill=(255, 255, 255, 255))
    d.ellipse([cx - r2, cy - r2, cx + r2, cy + r2], fill=(251, 188, 4, 255))
    return img.resize((size, size), Image.LANCZOS)

render(192).save(OUT / "icon-192.png", optimize=True)
render(512).save(OUT / "icon-512.png", optimize=True)
render(512, maskable=True).save(OUT / "maskable-512.png", optimize=True)
render(180, radius=False).save(OUT / "apple-touch-icon.png", optimize=True)
render(32).save(OUT / "favicon-32.png", optimize=True)
print("icons written to", OUT)
