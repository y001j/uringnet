#!/usr/bin/env python3
"""Generate the UringNet logo: a "U" made of ring-buffer slots, plus the wordmark.

Usage (from the repo root):  python3 docs/images/make_logo.py docs/images
"""
import math, sys
out = sys.argv[1]
FONT = "-apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif"
# Warm to cool: the right arm (t=0) is orange, the left arm (t=1) blue. Going through magenta and
# violet instead of mixing orange and blue directly keeps the middle from turning grey.
STOPS = ["#f08c24", "#e8553a", "#d6487e", "#8e4fd6", "#2a78d6"]
def _rgb(h): return tuple(int(h[i:i + 2], 16) for i in (1, 3, 5))
def mix(t):
    x = t * (len(STOPS) - 1); i = min(int(x), len(STOPS) - 2); f = x - i
    a, b = _rgb(STOPS[i]), _rgb(STOPS[i + 1])
    return "#%02x%02x%02x" % tuple(round(u + (v - u) * f) for u, v in zip(a, b))
def pt(cx, cy, r, deg):
    a = math.radians(deg - 90); return cx + r * math.cos(a), cy + r * math.sin(a)
def seg(cx, cy, ro, ri, a0, a1):
    p = [pt(cx, cy, ro, a0), pt(cx, cy, ro, a1), pt(cx, cy, ri, a1), pt(cx, cy, ri, a0)]
    return (f"M{p[0][0]:.2f} {p[0][1]:.2f}A{ro} {ro} 0 0 1 {p[1][0]:.2f} {p[1][1]:.2f}"
            f"L{p[2][0]:.2f} {p[2][1]:.2f}A{ri} {ri} 0 0 0 {p[3][0]:.2f} {p[3][1]:.2f}Z")
def mark(cx, cy, s=1.0, empty="#d0d7de"):
    ro, ri = 46 * s, 29 * s
    n, start, end, gap = 9, 38, 322, 6   # opening at the top makes the ring read as a "U"
    step = (end - start) / n
    parts = []
    for i in range(n):
        a0 = start + i * step + gap / 2; a1 = start + (i + 1) * step - gap / 2
        fill = mix(i / (n - 1))
        parts.append(f'<path d="{seg(cx, cy, ro, ri, a0, a1)}" fill="{fill}"/>')
    # head marker: a dot just past the last filled slot, where the next entry goes
    hx, hy = pt(cx, cy, (ro + ri) / 2, start + 7 * step + step / 2)
    return parts
def logo(theme):
    text = "#1f2328" if theme == "light" else "#f0f6fc"
    empty = "#d0d7de" if theme == "light" else "#3d444d"
    muted = "#59636e" if theme == "light" else "#9198a1"
    W, H = 460, 120
    p = mark(60, 60, 1.0, empty)
    p.append(f'<text x="124" y="74" font-family="{FONT}" font-size="50" font-weight="700" letter-spacing="-1" fill="{text}">Uring<tspan fill="{mix(0.25)}">Net</tspan></text>')
    p.append(f'<text x="126" y="99" font-family="{FONT}" font-size="14.5" fill="{muted}" letter-spacing="0.3">io_uring networking for Go</text>')
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" role="img" aria-label="UringNet">'
            f'<title>UringNet</title>' + "".join(p) + "</svg>\n")
def icon():
    p = mark(60, 60, 1.0)
    return ('<svg xmlns="http://www.w3.org/2000/svg" width="120" height="120" viewBox="0 0 120 120" role="img" aria-label="UringNet">'
            '<title>UringNet</title>' + "".join(p) + "</svg>\n")
for t in ("light", "dark"):
    open(f"{out}/logo_{t}.svg", "w").write(logo(t))
open(f"{out}/logo_icon.svg", "w").write(icon())
