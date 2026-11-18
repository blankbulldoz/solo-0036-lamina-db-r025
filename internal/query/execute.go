package query

import (
	"fmt"
	"lamina/internal/catalog"
	"lamina/internal/datum"
	"math"
	"sort"
)

type orderKey struct {
	column     string
	desc       bool
	nullsFirst bool
}

// Execute evaluates a query against the rows selected by the engine. Ordering
// is a presentation concern: the matched row set does not change with access
// path, sort direction or null placement. The primary key is always the final
// tie-break in canonical ascending order, so equal ordering keys produce one
// deterministic result. When the plan is an ordered index walk, keys already
// arrive in presentation order; the same comparator is still applied so a
// historical or duplicate candidate cannot escape the ordering.
func Execute(r catalog.Relation, q Request, rows map[string]datum.Row, plan Plan, keys []string) (Result, error) {
	result := Result{Rows: []datum.Row{}, Plan: plan}
	if plan.Access == "scan" {
		keys = datum.Keys(rows)
	}
	selected := make([]datum.Row, 0, len(keys))
	seen := map[string]bool{}
	for _, key := range keys {
		row, ok := rows[key]
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		result.Scanned++
		if Matches(row, q.Where) {
			selected = append(selected, row)
		}
	}
	result.Matched = len(selected)
	if q.Aggregate != nil {
		row, err := reduce(selected, *q.Aggregate)
		if err != nil {
			return result, err
		}
		result.Rows = append(result.Rows, row)
		return result, nil
	}
	order := make([]orderKey, 0, len(q.Order))
	for _, o := range q.Order {
		order = append(order, orderKey{column: o.Column, desc: o.Desc, nullsFirst: o.NullsFirstResolved()})
	}
	less := func(a, b datum.Row) bool {
		for _, o := range order {
			n := datum.OrderFor(a[o.column], b[o.column], o.desc, o.nullsFirst)
			if n != 0 {
				return n < 0
			}
		}
		return datum.Order(a[r.Primary], b[r.Primary]) < 0
	}
	if !plan.Ordered || !sortedBy(selected, less) {
		sort.SliceStable(selected, func(i, j int) bool { return less(selected[i], selected[j]) })
	}
	limit := q.Limit
	if limit == 0 {
		limit = 1000
	}
	if len(selected) > limit {
		selected = selected[:limit]
		result.Truncated = true
	}
	for _, row := range selected {
		if len(q.Columns) == 0 {
			result.Rows = append(result.Rows, datum.Clone(row))
		} else {
			projection := datum.Row{}
			for _, name := range q.Columns {
				projection[name] = row[name]
			}
			result.Rows = append(result.Rows, projection)
		}
	}
	return result, nil
}

// sortedBy reports whether rows are already in presentation order. An ordered
// index walk normally satisfies this, so the sort is skipped; the check keeps
// the result correct if candidate order ever differs from the requested
// presentation order.
func sortedBy(rows []datum.Row, less func(a, b datum.Row) bool) bool {
	for i := 1; i < len(rows); i++ {
		if less(rows[i], rows[i-1]) {
			return false
		}
	}
	return true
}

func reduce(rows []datum.Row, a Aggregate) (datum.Row, error) {
	var total int64
	var count int64
	for _, row := range rows {
		v := row[a.Column]
		if a.Column != "" && a.Column != "*" && v.Null() {
			continue
		}
		count++
		if a.Function == "sum" {
			if v.Int > 0 && total > math.MaxInt64-v.Int || v.Int < 0 && total < math.MinInt64-v.Int {
				return nil, fmt.Errorf("sum overflow")
			}
			total += v.Int
		}
	}
	if a.Function == "count" {
		return datum.Row{"count": datum.Integer(count)}, nil
	}
	if count == 0 {
		return datum.Row{"sum": datum.Value{Kind: "null"}}, nil
	}
	return datum.Row{"sum": datum.Integer(total)}, nil
}
