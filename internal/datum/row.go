package datum

import "sort"

type Row map[string]Value

func Clone(row Row) Row {
	if row == nil {
		return nil
	}
	c := make(Row, len(row))
	for k, v := range row {
		c[k] = v
	}
	return c
}
func Keys(rows map[string]Row) []string {
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Order is the canonical ascending total order for stored values. It is the
// order used by physical access structures such as secondary index entries and
// by the primary-key tie-break. Null sorts before every non-null value here;
// this is a storage convention, not a client-visible null-placement policy.
func Order(a, b Value) int {
	if a.Null() && b.Null() {
		return 0
	}
	if a.Null() {
		return -1
	}
	if b.Null() {
		return 1
	}
	n, _ := a.Compare(b)
	return n
}

// OrderFor is the presentation order for one explicit ordering key. desc
// reverses only the comparison of non-null values. nullsFirst places nulls
// before non-null values and nullsLast places them after, independently of
// desc, so a client can request any of the four combinations.
func OrderFor(a, b Value, desc, nullsFirst bool) int {
	aNull, bNull := a.Null(), b.Null()
	if aNull || bNull {
		if aNull && bNull {
			return 0
		}
		if aNull == nullsFirst {
			return -1
		}
		return 1
	}
	n, _ := a.Compare(b)
	if desc {
		return -n
	}
	return n
}
