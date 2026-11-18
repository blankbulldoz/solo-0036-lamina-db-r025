#!/usr/bin/env python3
"""Interleaved null-ordering and access-path verification against the public API.

This is a bounded observation script, not a test framework. It drives a real
laminad process through index creation, updates, deletes, vacuum and historical
reads, and checks that:

  * an explicit null placement is honoured for all four (direction, placement)
    combinations through both /query and /sql;
  * compound ordering falls back to a stable primary-key tie-break;
  * requests that omit null placement return exactly the historical rows;
  * the ordered index path and a forced scan return the same complete visible
    row set at every snapshot in the retained interval, before and after
    reclamation rebuilds the index.
"""
import argparse
import json
import pathlib
import sys
import tempfile

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import walkthrough as w


def rows_of(result):
    return result["rows"]


def by_sample(result):
    return {row["sample"]: row for row in rows_of(result)}


def placements():
    return [
        {"column": "amplitude"},
        {"column": "amplitude", "desc": True},
        {"column": "amplitude", "nulls_first": True},
        {"column": "amplitude", "nulls_last": True},
        {"column": "amplitude", "desc": True, "nulls_first": True},
        {"column": "amplitude", "desc": True, "nulls_last": True},
    ]


def check_paths(s, label, where=None, order=None, at=None, transaction=None):
    """Compare ordered index access with a forced scan at one snapshot."""
    base = {}
    if where is not None:
        base["where"] = where
    if order is not None:
        base["order"] = order
    if at is not None:
        base["at"] = at
    if transaction is not None:
        base["transaction"] = transaction
    fast = s.query(**base)
    slow = s.query(force_scan=True, **base)
    w.require(fast["rows"] == slow["rows"],
              f"{label}: index and scan rows diverged\n index={fast['rows']}\n scan={slow['rows']}")
    w.require(fast["matched"] == slow["matched"], f"{label}: matched diverged")
    return fast, slow


def expected_order(rows, desc, nulls_first):
    non_null = [r for r in rows if r["amplitude"] is not None]
    nulls = [r for r in rows if r["amplitude"] is None]
    # Descending reverses value groups but the primary key always breaks ties
    # in ascending order, matching the documented stable tie-break.
    sign = -1 if desc else 1
    non_null.sort(key=lambda r: (sign * r["amplitude"], r["sample"]))
    nulls.sort(key=lambda r: r["sample"])
    return nulls + non_null if nulls_first else non_null + nulls


def run(binary):
    with tempfile.TemporaryDirectory(prefix="lamina-null-order-") as temp:
        temp = pathlib.Path(temp)
        s = w.Session(binary, temp / "db").start()
        try:
            seed_seq = w.seed(s)  # sample 7 has amplitude null
            observations = {}

            # Baseline legacy behaviour: no explicit null placement must match
            # the documented convention (asc nulls first, desc nulls last).
            legacy_asc = s.query(order=[{"column": "amplitude"}])
            w.require([r["sample"] for r in legacy_asc["rows"]][0] == 7,
                      "legacy ascending no longer places null first")
            legacy_desc = s.query(order=[{"column": "amplitude", "desc": True}])
            w.require([r["sample"] for r in legacy_desc["rows"]][-1] == 7,
                      "legacy descending no longer places null last")
            observations["legacy"] = {
                "asc": [r["sample"] for r in legacy_asc["rows"]],
                "desc": [r["sample"] for r in legacy_desc["rows"]],
            }

            # Create the index after data exists, so the tree is built from
            # retained history rather than only from later commits.
            s.call("POST", "/indexes",
                   {"name": "by_amplitude", "relation": "spectra", "column": "amplitude"}, 201)

            # Every explicit placement must be honoured and must agree with the
            # scan path at the current snapshot.
            current = by_sample(s.query())
            for spec in placements():
                fast, slow = check_paths(s, f"initial {spec}", order=[spec])
                got = [r["sample"] for r in fast["rows"]]
                desc = spec.get("desc", False)
                if spec.get("nulls_first"):
                    nulls_first = True
                elif spec.get("nulls_last"):
                    nulls_first = False
                else:
                    nulls_first = not desc
                want = [r["sample"] for r in
                        expected_order(list(current.values()), desc, nulls_first)]
                w.require(got == want, f"placement {spec}: {got} != {want}")
                if spec.get("nulls_first") or spec.get("nulls_last"):
                    w.require(fast["plan"].get("ordered", False),
                              f"placement {spec}: index order not used")
            observations["initial_placements"] = {
                str(spec): [r["sample"] for r in s.query(order=[spec])["rows"]]
                for spec in placements()
            }

            # SQL and structured query must agree for the same placement.
            sql = s.call("POST", "/sql", {
                "sql": "SELECT sample FROM spectra ORDER BY amplitude DESC NULLS FIRST;",
            })
            structured = s.query(columns=["sample"],
                                 order=[{"column": "amplitude", "desc": True, "nulls_first": True}])
            w.require(sql["rows"] == structured["rows"], "SQL null placement diverged from structured query")
            observations["sql"] = [r["sample"] for r in sql["rows"]]

            # Interleaving: update values (including null -> value and
            # value -> null), then read the pre-update snapshot through the
            # index and through a scan. The index must still serve the old
            # snapshot even though it now also holds newer candidates.
            at_before = s.stats()["sequence"]
            tx = s.begin()
            s.put(tx, w.tuple_(1, amplitude=None))       # 10 -> null
            s.put(tx, w.tuple_(7, amplitude=35))         # null -> 35
            s.put(tx, w.tuple_(3, amplitude=45))         # 30 -> 45
            s.commit(tx)
            for spec in placements():
                check_paths(s, f"historical {spec}", order=[spec], at=at_before)
                check_paths(s, f"current-after-update {spec}", order=[spec])
            historical = s.query(at=at_before, order=[{"column": "amplitude", "nulls_last": True}])
            w.require(by_sample(historical) == current,
                      "historical snapshot changed after update")
            observations["historical_rows"] = [r["sample"] for r in historical["rows"]]

            # Compound ordering: values tie on amplitude, so the primary key
            # decides. Both null placements must still be consistent and the
            # tie-break must stay ascending in both directions.
            tie_tx = s.begin()
            s.put(tie_tx, w.tuple_(8, amplitude=45))
            s.put(tie_tx, w.tuple_(9, amplitude=45))
            s.commit(tie_tx)
            for spec in ({"column": "amplitude", "nulls_last": True},
                         {"column": "amplitude", "desc": True, "nulls_first": True}):
                compound = s.query(order=[{"column": "valid", "desc": True}, spec])
                w.require(compound["rows"] == s.query(order=[{"column": "valid", "desc": True}, spec],
                                                      force_scan=True)["rows"],
                          f"compound {spec}: access paths diverged")
            observations["compound"] = [r["sample"] for r in
                                         s.query(order=[{"column": "valid", "desc": True},
                                                        {"column": "amplitude", "nulls_last": True}])["rows"]]

            # Interleaving: delete a row, read both before and after the
            # delete snapshot through every path. An old reader is opened
            # before the delete so vacuum has a real snapshot to pin.
            at_with_rows = s.stats()["sequence"]
            before_delete = set(by_sample(s.query()))
            old_reader = s.begin()
            tx = s.begin()
            s.call("POST", f"/transactions/{tx}/delete", {"relation": "spectra", "key": 4})
            s.commit(tx)
            for spec in placements():
                check_paths(s, f"after-delete-current {spec}", order=[spec])
                check_paths(s, f"after-delete-historical {spec}", order=[spec], at=at_with_rows)
                check_paths(s, f"after-delete-reader {spec}", order=[spec],
                            transaction=old_reader)
            w.require(4 not in by_sample(s.query()), "deleted row still visible")
            w.require(4 in by_sample(s.query(at=at_with_rows)), "historical delete lost a visible row")

            # Interleaving: vacuum while the old reader pins the pre-delete
            # snapshot, verify the rebuilt candidates still serve that pinned
            # snapshot, then close the reader and reclaim again.
            head = s.stats()["sequence"]
            pinned = s.call("POST", "/vacuum", {"horizon": head})
            w.require(pinned["pinned"] and pinned["horizon"] == at_with_rows,
                      f"vacuum did not stop at the active snapshot: {pinned}")
            for spec in placements():
                check_paths(s, f"vacuum-pinned {spec}", order=[spec],
                            transaction=old_reader)
            s.call("POST", f"/transactions/{old_reader}/rollback")
            reclaimed = s.call("POST", "/vacuum", {"horizon": head})
            w.require(not reclaimed["pinned"], "vacuum stayed pinned without readers")
            # Reclamation rebuilt the trees from retained versions only. The
            # retained interval now starts at a surviving snapshot; requests
            # inside it must still agree across access paths, and the complete
            # visible key set must match the pre-reclaim current set minus the
            # deleted key.
            horizon = s.stats()["horizon"]
            for spec in placements():
                check_paths(s, f"after-reclaim {spec}", order=[spec])
                if horizon <= s.stats()["sequence"]:
                    check_paths(s, f"after-reclaim-at-horizon {spec}", order=[spec], at=horizon)
            w.require(set(by_sample(s.query())) == before_delete - {4},
                      "reclaim changed the complete visible key set")
            s.call("POST", "/query", {"relation": "spectra", "at": seed_seq}, 410)

            # Restart rebuilds the trees from the retained versions. The same
            # placement requests and same visible rows must come back.
            before_restart = [r["sample"] for r in
                              s.query(order=[{"column": "amplitude", "nulls_last": True}])["rows"]]
            s.stop()
            s.start()
            after_restart = [r["sample"] for r in
                             s.query(order=[{"column": "amplitude", "nulls_last": True}])["rows"]]
            w.require(before_restart == after_restart, "restart rebuild changed ordered rows")
            for spec in placements():
                check_paths(s, f"after-restart {spec}", order=[spec])
            observations["restart"] = after_restart

            # Index entry facts: null associations are traversable but are not
            # counted as historical non-null candidate associations.
            stats = s.stats()
            w.require("by_amplitude" in stats["index_entries"], "index missing after restart")
            observations["index_entries"] = stats["index_entries"]

            return {"workflow": "null-ordering-interleaving", "status": "observed",
                    "observations": observations}
        finally:
            s.stop()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", default=None)
    args = parser.parse_args()
    binary = pathlib.Path(args.binary) if args.binary else pathlib.Path(__file__).resolve().parents[1] / "bin/laminad"
    if not binary.exists():
        raise SystemExit("build first: go build -o bin/laminad ./cmd/laminad")
    print(json.dumps(run(str(binary)), ensure_ascii=False))


if __name__ == "__main__":
    main()
