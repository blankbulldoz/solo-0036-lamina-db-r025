#!/usr/bin/env python3
"""Randomized differential check for null ordering and index/scan agreement.

Bounded observation only. It generates a small relation with nullable values,
performs random inserts, updates, deletes and null toggles, and at every step
compares:

  * ordered index access against a forced full scan (both explicit and legacy
    placement requests);
  * each result against an independent Python ordering model;
  * historical snapshots in the retained interval against the same model;
  * the complete visible key set, so no path drops or invents a row.

The seed is fixed by default so failures reproduce.
"""
import argparse
import json
import pathlib
import random
import sys
import tempfile

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import walkthrough as w

SCHEMA = {
    "name": "spectra",
    "primary": "sample",
    "columns": [
        {"name": "sample", "type": "int"},
        {"name": "band", "type": "text"},
        {"name": "amplitude", "type": "int", "nullable": True},
        {"name": "valid", "type": "bool"},
    ],
}


def row(n, value, valid=True):
    return {"sample": n, "band": f"b{n}", "amplitude": value, "valid": valid}


def model_order(rows, desc, nulls_first):
    non_null = [r for r in rows if r["amplitude"] is not None]
    nulls = [r for r in rows if r["amplitude"] is None]
    sign = -1 if desc else 1
    non_null.sort(key=lambda r: (sign * r["amplitude"], r["sample"]))
    nulls.sort(key=lambda r: r["sample"])
    return nulls + non_null if nulls_first else non_null + nulls


def model_rows(snapshot, key, rows, at):
    """Independent version model: list of (sequence, row-or-None)."""
    visible = None
    for sequence, value in snapshot[key]:
        if sequence <= at:
            visible = value
        else:
            break
    return visible


def placements(rng):
    base = [{"column": "amplitude"}]
    for desc in (False, True):
        base.append({"column": "amplitude", "desc": desc})
        base.append({"column": "amplitude", "desc": desc, "nulls_first": True})
        base.append({"column": "amplitude", "desc": desc, "nulls_last": True})
    rng.shuffle(base)
    return base


def run(binary, seed, steps):
    rng = random.Random(seed)
    with tempfile.TemporaryDirectory(prefix="lamina-null-fuzz-") as temp:
        temp = pathlib.Path(temp)
        s = w.Session(binary, temp / "db").start()
        try:
            s.call("POST", "/relations", SCHEMA, 201)
            history = {}          # key -> list[(sequence, row|None)]
            snaps = []            # retained-capable snapshots
            next_key = 1
            created_index = False
            for step in range(steps):
                action = rng.choice(["insert", "update", "delete", "toggle", "index", "vacuum", "restart", "check"])
                if action == "insert" or not history:
                    key = next_key
                    next_key += 1
                    value = rng.choice([None, *range(0, 40, 10)])
                    tx = s.begin()
                    s.put(tx, row(key, value), insert=True)
                    seq = s.commit(tx)["sequence"]
                    history[key] = [(seq, row(key, value))]
                    snaps.append(seq)
                elif action == "update":
                    key = rng.choice(sorted(history))
                    value = rng.choice([None, *range(0, 40, 10)])
                    tx = s.begin()
                    s.put(tx, row(key, value))
                    seq = s.commit(tx)["sequence"]
                    history[key].append((seq, row(key, value)))
                    snaps.append(seq)
                elif action == "delete":
                    key = rng.choice(sorted(history))
                    tx = s.begin()
                    s.call("POST", f"/transactions/{tx}/delete", {"relation": "spectra", "key": key})
                    seq = s.commit(tx)["sequence"]
                    history[key].append((seq, None))
                    snaps.append(seq)
                elif action == "toggle" and history:
                    key = rng.choice(sorted(history))
                    current = model_rows(history, key, None, 10**9)
                    value = None if current is not None else rng.choice([0, 10, 20, 30])
                    tx = s.begin()
                    s.put(tx, row(key, value))
                    seq = s.commit(tx)["sequence"]
                    history[key].append((seq, row(key, value)))
                    snaps.append(seq)
                elif action == "index" and not created_index:
                    s.call("POST", "/indexes",
                           {"name": "by_amplitude", "relation": "spectra", "column": "amplitude"}, 201)
                    created_index = True
                elif action == "vacuum":
                    # Reclaim only what no reader pins; keep every snapshot we
                    # still want to query. The new horizon must never exceed the
                    # current head.
                    head = s.stats()["sequence"]
                    result = s.call("POST", "/vacuum", {"horizon": head})
                    horizon = result["horizon"]
                    snaps = [seq for seq in snaps if seq >= horizon]
                    if not snaps:
                        snaps.append(head)
                elif action == "restart":
                    # A restart rebuilds every index from retained versions.
                    s.stop()
                    s.start()

                head = s.stats()["sequence"]
                # Model the visible rows at head from the independent history.
                expected = {}
                for key, chain in history.items():
                    value = model_rows(history, key, None, head)
                    if value is not None:
                        expected[key] = value
                actual = {r["sample"]: r for r in s.query()["rows"]}
                w.require(set(actual) == set(expected),
                          f"step {step}: visible key set diverged: {set(actual)} != {set(expected)}")

                at = rng.choice(snaps + [head])
                for spec in placements(rng)[:3]:
                    legacy = not (spec.get("nulls_first") or spec.get("nulls_last"))
                    fast = s.query(order=[spec], at=at)
                    slow = s.query(order=[spec], at=at, force_scan=True)
                    w.require(fast["rows"] == slow["rows"],
                              f"step {step} {spec} at={at}: index/scan diverged")
                    rows_at = {k: model_rows(history, k, None, at) for k in history}
                    visible_at = [r for r in rows_at.values() if r is not None]
                    desc = spec.get("desc", False)
                    if spec.get("nulls_first"):
                        nulls_first = True
                    elif spec.get("nulls_last"):
                        nulls_first = False
                    else:
                        nulls_first = not desc
                    want = [r["sample"] for r in model_order(visible_at, desc, nulls_first)]
                    got = [r["sample"] for r in fast["rows"]]
                    w.require(got == want, f"step {step} {spec} at={at}: {got} != {want}")

                # Deleting a key must never resurrect it at a later snapshot
                # that is still inside the retained interval.
                horizon = s.stats()["horizon"]
                for key, chain in history.items():
                    deleted_at = None
                    for sequence, value in chain:
                        if value is None and sequence <= head:
                            deleted_at = sequence
                    if deleted_at is not None and key not in expected and deleted_at >= horizon:
                        rows = {r["sample"] for r in s.query(at=deleted_at)["rows"]}
                        w.require(key not in rows, f"step {step}: deleted key {key} visible at {deleted_at}")
            return {"workflow": "null-ordering-fuzz", "status": "observed",
                    "seed": seed, "steps": steps,
                    "observations": {"index_created": created_index,
                                     "keys": len(history),
                                     "head": s.stats()["sequence"]}}
        finally:
            s.stop()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--seed", type=int, default=20261117)
    parser.add_argument("--steps", type=int, default=40)
    parser.add_argument("--binary", default=None)
    args = parser.parse_args()
    binary = pathlib.Path(args.binary) if args.binary else pathlib.Path(__file__).resolve().parents[1] / "bin/laminad"
    if not binary.exists():
        raise SystemExit("build first: go build -o bin/laminad ./cmd/laminad")
    print(json.dumps(run(str(binary), args.seed, args.steps), ensure_ascii=False))


if __name__ == "__main__":
    main()
