#!/usr/bin/env python3
"""Reference oracle for the sync conformance vectors. NOT part of the contract —
a dev aid that proves each vector is internally self-consistent under the merge rule
defined in ../forestnote-sync-protocol.md §5. Run:

    python3 docs/sync/vectors/_oracle.py docs/sync/vectors/*.vector.json
"""
import json
import sys

# Known column sets per table (spec §3.1). cols keys outside these are dropped (§3.2).
KNOWN = {
    "notebook": {"name", "sort_order", "created_at", "deleted_at"},
    "page": {"notebook_id", "sort_order", "created_at", "deleted_at"},
    "stroke": {"page_id", "color", "pen_width_min", "pen_width_max",
               "points", "z", "created_at", "deleted_at"},
}
ULID_ALPHABET = set("0123456789ABCDEFGHJKMNPQRSTVWXYZ")


def is_ulid(s):
    return isinstance(s, str) and len(s) == 26 and all(c in ULID_ALPHABET for c in s)


def key(op):
    # (wall_ts, op_seq, site_id) lexicographic; site_id compared as ASCII string (spec §2.1)
    return (op["wall_ts"], op["op_seq"], op["site_id"])


def normalize(op):
    known = KNOWN[op["table"]]
    cols = {k: v for k, v in op["cols"].items() if k in known}
    return {**op, "cols": cols}


def merge(ops):
    winners = {}
    for op in ops:
        assert op["table"] in KNOWN, f"unknown table {op['table']!r}"
        assert is_ulid(op["pk"]), f"bad pk {op['pk']!r}"
        assert is_ulid(op["site_id"]), f"bad site_id {op['site_id']!r}"
        assert isinstance(op["op_seq"], int) and op["op_seq"] > 0
        op = normalize(op)
        assert set(op["cols"]) == KNOWN[op["table"]], \
            f"op for {op['pk']} missing/extra known cols: {sorted(op['cols'])}"
        k = (op["table"], op["pk"])
        cur = winners.get(k)
        if cur is None or key(op) > key(cur):
            winners[k] = op
    state = {"notebook": [], "page": [], "stroke": []}
    for (table, _pk), op in winners.items():
        row = {kk: op[kk] for kk in ("pk", "site_id", "op_seq", "wall_ts", "cols")}
        state[table].append(row)
    return state


def canonical(state):
    # order-independent: sort each table's rows by pk
    return {t: sorted((state.get(t) or []), key=lambda r: r["pk"]) for t in KNOWN}


def main(paths):
    failures = 0
    for path in paths:
        with open(path) as f:
            v = json.load(f)
        got = canonical(merge(v["ops"]))
        want = canonical(v["expected_state"])
        if got == want:
            print(f"PASS  {v['name']}")
        else:
            failures += 1
            print(f"FAIL  {v['name']}")
            print(f"  want: {json.dumps(want, sort_keys=True)}")
            print(f"  got:  {json.dumps(got, sort_keys=True)}")
    print(f"\n{len(paths) - failures}/{len(paths)} vectors pass")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
