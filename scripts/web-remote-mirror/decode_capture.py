#!/usr/bin/env python3
"""Decode captured official desktop WS binary frames: head array + data value."""
import json, base64, sys

def rd_varint(b, i):
    v = 0; shift = 0
    while True:
        v |= (b[i] & 0x7F) << shift; i += 1
        if not (b[i-1] & 0x80): break
        shift += 7
    return v, i

def rd_value(b, i):
    tag = b[i]; i += 1
    if tag == 0: return None, i
    if tag == 6:
        v, i = rd_varint(b, i); return v, i
    if tag == 1:
        n, i = rd_varint(b, i); return b[i:i+n].decode("utf8", "replace"), i+n
    if tag in (2, 3):
        n, i = rd_varint(b, i); return b[i:i+n], i+n
    if tag == 4:
        n, i = rd_varint(b, i); out = []
        for _ in range(n):
            v, i = rd_value(b, i); out.append(v)
        return out, i
    if tag == 5:
        n, i = rd_varint(b, i)
        try:
            return json.loads(b[i:i+n]), i+n
        except Exception:
            return {"_raw": b[i:i+n].decode("utf8","replace")[:300]}, i+n
    raise ValueError("tag %d at %d" % (tag, i))

frames = json.loads(json.load(open(sys.argv[1])))
out = []
for f in frames:
    b = base64.b64decode(f["b64"])
    rec = {"seq": f["seq"]}
    try:
        head, i = rd_value(b, 0)
        rec["head"] = head
        if i < len(b):
            data, j = rd_value(b, i)
            rec["data"] = data
    except Exception as e:
        rec["err"] = str(e)
    out.append(rec)
json.dump(out, open("/tmp/official-decoded.json", "w"), ensure_ascii=False, indent=1)

from collections import Counter
c = Counter()
for o in out:
    h = o.get("head")
    if isinstance(h, list):
        c[tuple(h[:1])] += 1
print(c.most_common(10))
