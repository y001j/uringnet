#!/usr/bin/env python3
"""汇总 run.sh 的输出：每个场景取各轮中位数，并给出相对基准服务端的比值。
用法: python3 summarize.py results.txt [基准服务端，默认 stdcopy]"""
import collections, re, statistics as st, sys

rows = collections.defaultdict(list)
names = []
pat = re.compile(r"([\w+-]+)\s+c=(\d+) p=(\d+) s=(\d+) qps=(\d+) clientcpu=(\d+)% p50=(\d+)us p99=(\d+)us servercpu=(\d+)%")
for line in open(sys.argv[1]):
    if line.startswith("#"):
        print(line.strip())
    m = pat.match(line)
    if not m:
        continue
    name = m[1]
    rows[(int(m[2]), int(m[3]), int(m[4]), name)].append([int(x) for x in m.groups()[4:]])
    if name not in names:
        names.append(name)
if not names:
    sys.exit("no results")
base = sys.argv[2] if len(sys.argv) > 2 else ("stdcopy" if "stdcopy" in names else names[0])

def med(v, i):
    return st.median(x[i] for x in v)

print(f"\nQPS（中位数，括号内为相对 {base}）")
print(f"{'场景':<16}" + "".join(f"{n:>18}" for n in names))
for key in sorted({k[:3] for k in rows}):
    c, p, s = key
    b = med(rows[key + (base,)], 0) if rows.get(key + (base,)) else None
    line = f"c={c:<4} p={p:<2} {s:>5}B "
    for n in names:
        v = rows.get(key + (n,))
        if not v:
            line += f"{'-':>18}"
            continue
        q = med(v, 0)
        line += f"{q/1000:>9.0f}k ({q/b:.2f})" if b else f"{q/1000:>16.0f}k"
    print(line)

print("\n服务端 CPU% / p50 延迟(us)")
print(f"{'场景':<16}" + "".join(f"{n:>18}" for n in names))
for key in sorted({k[:3] for k in rows}):
    c, p, s = key
    line = f"c={c:<4} p={p:<2} {s:>5}B "
    for n in names:
        v = rows.get(key + (n,))
        line += f"{med(v,4):>10.0f}% {med(v,2):>5.0f}" if v else f"{'-':>18}"
    print(line)
