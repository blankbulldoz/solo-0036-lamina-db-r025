package query

import (
	"lamina/internal/catalog"
	"lamina/internal/datum"
	"lamina/internal/index"
	"sort"
)

// Choose selects an access path. A predicate index narrows the candidate key
// set. Independently, a single ordering key backed by an index may use that
// index's canonical order, so the engine can walk visible rows directly in
// presentation order instead of materializing and sorting the whole result.
// When both are available the smaller candidate set wins; on a tie an ordered
// path wins because it also avoids the sort. Direction and null placement are
// recorded as traversal parameters; planning never rewrites the index.
func Choose(c *catalog.Catalog, registry *index.Registry, q Request, at uint64, total int) (Plan, []string) {
	plan := Plan{Access: "scan", Candidates: total, Snapshot: at, Residuals: len(q.Where)}
	if q.ForceScan {
		return plan, nil
	}
	names := make([]string, 0, len(c.Indexes))
	for name := range c.Indexes {
		names = append(names, name)
	}
	sort.Strings(names)
	var chosen []string
	for _, name := range names {
		definition := c.Indexes[name]
		if definition.Relation != q.Relation || definition.Created > at {
			continue
		}
		for _, p := range q.Where {
			if p.Column != definition.Column || p.Value.Null() {
				continue
			}
			switch p.Op {
			case "=", "<", ">", "<=", ">=":
			default:
				continue
			}
			candidates := registry.Trees[name].Candidates(p.Op, p.Value)
			if len(candidates) < plan.Candidates || plan.Access == "scan" && len(candidates) == 0 {
				plan.Access = "index"
				plan.Index = name
				plan.Candidates = len(candidates)
				plan.Ordered = false
				chosen = candidates
			}
		}
	}
	if len(q.Order) != 1 || q.Aggregate != nil {
		return plan, chosen
	}
	ordering := q.Order[0]
	// Ordered index access is offered only when the client explicitly chooses
	// a null placement. Requests that rely on the historical default keep the
	// original plan (predicate index or scan plus a presentation sort), so
	// their rows, candidate counts and access facts are unchanged.
	if !ordering.Explicit() {
		return plan, chosen
	}
	for _, name := range names {
		definition := c.Indexes[name]
		if definition.Relation != q.Relation || definition.Column != ordering.Column || definition.Created > at {
			continue
		}
		op, value, ranged := orderedRange(q, ordering.Column)
		candidates := registry.Trees[name].KeyCount()
		if ranged {
			candidates = len(registry.Trees[name].Candidates(op, value))
		}
		if plan.Access == "index" && candidates > plan.Candidates {
			continue
		}
		plan.Access = "index"
		plan.Index = name
		plan.Candidates = candidates
		plan.Ordered = true
		plan.OrderColumn = ordering.Column
		plan.OrderDesc = ordering.Desc
		plan.NullsFirst = ordering.NullsFirstResolved()
		plan.RangeOp = op
		plan.RangeValue = value
		return plan, nil
	}
	return plan, chosen
}

// orderedRange returns the first normal comparison on the ordering column that
// an index can evaluate as a value range. Other predicates stay residual checks
// and do not affect the ordering.
func orderedRange(q Request, column string) (string, datum.Value, bool) {
	for _, p := range q.Where {
		if p.Column != column || p.Value.Null() {
			continue
		}
		switch p.Op {
		case "=", "<", ">", "<=", ">=":
			return p.Op, p.Value, true
		}
	}
	return "", datum.Value{}, false
}
