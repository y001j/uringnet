#!/usr/bin/env python3
"""Generate the benchmark charts in docs/images from the README results.

Usage (from the repo root):  python3 bench/scripts/charts.py

No dependencies. Writes a light and a dark variant of each chart; the README
picks one with <picture> according to the GitHub theme.
"""
import os

OUT = os.path.join(os.path.dirname(__file__), "..", "..", "docs", "images")

FONT = "-apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif"

THEMES = {
    "light": {
        "text": "#1f2328", "muted": "#59636e", "grid": "#d8dee4", "ref": "#59636e",
        "series": ["#2a78d6", "#eb6834", "#1baf7a"], "neutral": "#8c959f", "gap": "#ffffff",
    },
    "dark": {
        "text": "#f0f6fc", "muted": "#9198a1", "grid": "#30363d", "ref": "#9198a1",
        "series": ["#3987e5", "#d95926", "#199e70"], "neutral": "#6e7681", "gap": "#0d1117",
    },
}

# Throughput relative to epoll, recommended configuration, Linux 6.17 (README tables).
THROUGHPUT = [
    # label, epoll req/s, gnet, uringnet no polling, uringnet adaptive polling
    ("1 conn, 128B", "10k/s", 1.05, 0.98, 1.60),
    ("1 conn, 16KB", "8k/s", 1.03, 0.99, 1.05),
    ("16 conn, 128B", "103k/s", 1.09, 0.90, 1.52),
    ("16 conn, 16KB", "81k/s", 0.94, 0.72, 1.06),
    ("16 conn ×8, 128B", "773k/s", 1.16, 0.92, 1.56),
    ("16 conn ×8, 16KB", "144k/s", 1.59, 1.22, 1.58),
    ("64 conn, 128B", "201k/s", 1.24, 1.00, 1.21),
    ("64 conn, 16KB", "152k/s", 1.06, 0.69, 0.90),
    ("64 conn ×8, 128B", "1.76M/s", 1.03, 0.95, 0.97),
    ("64 conn ×8, 16KB", "190k/s", 1.77, 1.61, 1.58),
    ("512 conn, 128B", "319k/s", 0.89, 1.13, 1.19),
    ("512 conn, 16KB", "199k/s", 0.97, 1.00, 0.93),
    ("512 conn ×8, 128B", "2.38M/s", 1.03, 1.16, 1.17),
    ("512 conn ×8, 16KB", "136k/s", 2.13, 1.84, 1.78),
]

# Median latency in µs at low and medium load: epoll, gnet, uringnet adaptive polling.
LATENCY = [
    ("1 conn, 128B", 78, 74, 30),
    ("1 conn, 16KB", 89, 85, 69),
    ("16 conn, 128B", 106, 84, 57),
    ("16 conn, 16KB", 150, 145, 113),
    ("64 conn, 128B", 212, 187, 170),
    ("64 conn, 16KB", 307, 285, 303),
]


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def text(x, y, s, fill, size=12, anchor="start", weight="normal"):
    return (f'<text x="{x:.1f}" y="{y:.1f}" fill="{fill}" font-size="{size}" '
            f'text-anchor="{anchor}" font-weight="{weight}">{esc(s)}</text>')


def hbar(x0, y, w, h, fill, gap):
    """Horizontal bar anchored at x0, square at the baseline, 4px-rounded data end, 1px surface outline as the gap."""
    r = min(4, h / 2, w)
    x1 = x0 + w
    d = (f"M{x0:.1f},{y:.1f} H{x1 - r:.1f} Q{x1:.1f},{y:.1f} {x1:.1f},{y + r:.1f} "
         f"V{y + h - r:.1f} Q{x1:.1f},{y + h:.1f} {x1 - r:.1f},{y + h:.1f} H{x0:.1f} Z")
    return f'<path d="{d}" fill="{fill}" stroke="{gap}" stroke-width="1"/>'


def legend(x, y, items, t):
    out = []
    for name, color in items:
        out.append(f'<rect x="{x}" y="{y - 9}" width="12" height="10" rx="2" fill="{color}"/>')
        out.append(text(x + 18, y, name, t["text"]))
        x += 18 + len(name) * 6.6 + 22
    return out


def svg(width, height, title, body):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" '
            f'viewBox="0 0 {width} {height}" font-family="{FONT}" role="img">'
            f"<title>{esc(title)}</title>" + "".join(body) + "</svg>\n")


def throughput_chart(t):
    W, left, right, top = 800, 200, 50, 78
    bar, inner, pitch = 8, 2, 40
    n = len(THROUGHPUT)
    H = top + n * pitch + 40
    xmax = 2.3
    sx = lambda v: left + v / xmax * (W - left - right)
    s = t["series"]
    names = [("uringnet, adaptive polling", s[0]), ("gnet", s[1]), ("uringnet, no polling", s[2])]
    b = [text(0, 20, "Throughput relative to epoll (higher is better)", t["text"], 16, weight="600"),
         text(0, 40, "Echo server on 1 core, Linux 6.17, incremental buffers. Right column: epoll requests/s.",
              t["muted"], 12)]
    b += legend(0, 62, names, t)
    for v in (0.5, 1.0, 1.5, 2.0):
        x = sx(v)
        ref = v == 1.0
        b.append(f'<line x1="{x:.1f}" y1="{top - 6}" x2="{x:.1f}" y2="{top + n * pitch}" '
                 f'stroke="{t["ref"] if ref else t["grid"]}" stroke-width="1"'
                 f'{" stroke-dasharray=\"4 3\"" if ref else ""}/>')
        b.append(text(x, top + n * pitch + 16, f"{v:.1f}×", t["muted"], 11, "middle"))
    b.append(text(sx(1.0), top + n * pitch + 32, "epoll", t["muted"], 11, "middle"))
    for i, (label, base, gnet, nopoll, adaptive) in enumerate(THROUGHPUT):
        y = top + i * pitch + 4
        b.append(text(left - 12, y + 16, label, t["text"], 12, "end"))
        b.append(text(left - 12, y + 30, base, t["muted"], 10, "end"))
        for j, (v, c) in enumerate(((adaptive, s[0]), (gnet, s[1]), (nopoll, s[2]))):
            b.append(hbar(left, y + j * (bar + inner), sx(v) - left, bar, c, t["gap"]))
        best = max(adaptive, gnet, nopoll)
        b.append(text(sx(best) + 6, y + 10, f"{adaptive:.2f}×", t["text"], 10))
        if i < n - 1:
            yl = top + (i + 1) * pitch - 4
            b.append(f'<line x1="0" y1="{yl}" x2="{W}" y2="{yl}" stroke="{t["grid"]}" stroke-width="0.5" opacity="0.6"/>')
    return svg(W, H, "uringnet throughput relative to epoll", b)


def latency_chart(t):
    W, left, right, top = 800, 200, 60, 78
    bar, inner, pitch = 10, 2, 48
    n = len(LATENCY)
    H = top + n * pitch + 28
    xmax = 320
    sx = lambda v: left + v / xmax * (W - left - right)
    s = t["series"]
    series = [("epoll", t["neutral"]), ("gnet", s[1]), ("uringnet, adaptive polling", s[0])]
    b = [text(0, 20, "Median latency at low and medium load (lower is better)", t["text"], 16, weight="600"),
         text(0, 40, "Echo server on 1 core, Linux 6.17, one request in flight per connection.", t["muted"], 12)]
    b += legend(0, 62, series, t)
    for v in range(0, 301, 50):
        x = sx(v)
        b.append(f'<line x1="{x:.1f}" y1="{top - 6}" x2="{x:.1f}" y2="{top + n * pitch - 8}" '
                 f'stroke="{t["grid"]}" stroke-width="1"/>')
        b.append(text(x, top + n * pitch + 8, f"{v}µs", t["muted"], 11, "middle"))
    for i, (label, *vals) in enumerate(LATENCY):
        y = top + i * pitch
        b.append(text(left - 12, y + 21, label, t["text"], 12, "end"))
        for j, (v, (_, c)) in enumerate(zip(vals, series)):
            yb = y + j * (bar + inner)
            b.append(hbar(left, yb, sx(v) - left, bar, c, t["gap"]))
            b.append(text(sx(v) + 6, yb + 9, f"{v}", t["text"] if j == 2 else t["muted"], 10,
                          weight="600" if j == 2 else "normal"))
    return svg(W, H, "Median latency: epoll, gnet, uringnet", b)


def main():
    os.makedirs(OUT, exist_ok=True)
    for theme, t in THEMES.items():
        for name, fn in (("bench_throughput", throughput_chart), ("bench_latency", latency_chart)):
            path = os.path.join(OUT, f"{name}_{theme}.svg")
            with open(path, "w") as f:
                f.write(fn(t))
            print(path)


if __name__ == "__main__":
    main()
