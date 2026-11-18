package index

import (
	"lamina/internal/datum"
	"sort"
)

// Entry is one distinct historical association a secondary index observed: a
// value and the primary key that had it.
//
// Entries carry no client null-placement policy. A tree has exactly one
// canonical order: value ascending, then typed primary key ascending. Null
// values are held in a separate canonical list because they have no comparison
// position. Reads interpret the canonical order without mutating it, so one
// physical structure serves snapshots taken before or after later writes and
// answers ascending, descending, nulls-first and nulls-last requests without
// being rebuilt.
type Entry struct {
	Value    datum.Value
	KeyValue datum.Value
	Key      string
}

type Tree struct {
	// entries holds non-null associations in canonical value/key order.
	entries []Entry
	// nulls holds null associations in canonical primary-key order. Null
	// values never satisfy normal comparisons, but ordered access still needs
	// to visit them so a null-placement request sees the complete row set.
	nulls    []Entry
	seen     map[string]bool
	nullSeen map[string]bool
	keys     map[string]struct{}
}

func New() *Tree {
	return &Tree{seen: map[string]bool{}, nullSeen: map[string]bool{}, keys: map[string]struct{}{}}
}

// Add records one association. Associations are deduplicated, so repeated
// historical values do not inflate the structure. Additions are inserted at a
// canonical position; no read path rewrites existing positions. An update
// therefore adds a new canonical association without disturbing the one that
// historical readers still need.
func (t *Tree) Add(value, keyValue datum.Value, key string) {
	t.keys[key] = struct{}{}
	if value.Null() {
		if t.nullSeen[key] {
			return
		}
		t.nullSeen[key] = true
		at := sort.Search(len(t.nulls), func(i int) bool {
			return compareKey(t.nulls[i], Entry{KeyValue: keyValue, Key: key}) >= 0
		})
		t.nulls = append(t.nulls, Entry{})
		copy(t.nulls[at+1:], t.nulls[at:])
		t.nulls[at] = Entry{Value: value, KeyValue: keyValue, Key: key}
		return
	}
	token := value.Key() + "\x00" + key
	if t.seen[token] {
		return
	}
	t.seen[token] = true
	at := sort.Search(len(t.entries), func(i int) bool {
		return compareEntry(t.entries[i], Entry{Value: value, KeyValue: keyValue, Key: key}) >= 0
	})
	t.entries = append(t.entries, Entry{})
	copy(t.entries[at+1:], t.entries[at:])
	t.entries[at] = Entry{Value: value, KeyValue: keyValue, Key: key}
}

func compareEntry(a, b Entry) int {
	if n := datum.Order(a.Value, b.Value); n != 0 {
		return n
	}
	return compareKey(a, b)
}

func compareKey(a, b Entry) int {
	if n := datum.Order(a.KeyValue, b.KeyValue); n != 0 {
		return n
	}
	if a.Key < b.Key {
		return -1
	}
	if a.Key > b.Key {
		return 1
	}
	return 0
}

func (t *Tree) bounds(op string, value datum.Value) (begin, end int, ok bool) {
	if value.Null() {
		return 0, 0, false
	}
	lo := sort.Search(len(t.entries), func(i int) bool { return datum.Order(t.entries[i].Value, value) >= 0 })
	hi := sort.Search(len(t.entries), func(i int) bool { return datum.Order(t.entries[i].Value, value) > 0 })
	switch op {
	case "=":
		return lo, hi, true
	case ">":
		return hi, len(t.entries), true
	case ">=":
		return lo, len(t.entries), true
	case "<":
		return 0, lo, true
	case "<=":
		return 0, hi, true
	}
	return 0, 0, false
}

// Candidates returns the distinct keys in a value range.
func (t *Tree) Candidates(op string, value datum.Value) []string {
	begin, end, ok := t.bounds(op, value)
	if !ok {
		return nil
	}
	out := make([]string, 0, end-begin)
	seen := map[string]bool{}
	for _, e := range t.entries[begin:end] {
		if !seen[e.Key] {
			seen[e.Key] = true
			out = append(out, e.Key)
		}
	}
	return out
}

// Ordered visits every association in the requested presentation order without
// rewriting the tree. Non-null values follow ascending or descending value
// order with primary keys ascending inside equal-value groups. Null
// associations form one block before or after the non-null values, also in
// primary-key order. This is exactly the total order query execution uses for a
// single ordering key, so called rows may be emitted directly.
func (t *Tree) Ordered(desc, nullsFirst bool, visit func(Entry) bool) {
	nulls := func() bool {
		for _, e := range t.nulls {
			if !visit(e) {
				return false
			}
		}
		return true
	}
	if nullsFirst {
		if nulls() {
			walkEntries(t.entries, desc, visit)
		}
		return
	}
	if walkEntries(t.entries, desc, visit) {
		nulls()
	}
}

// OrderedRange is Ordered restricted to a non-null comparison range. A normal
// predicate never matches null, so the null block is intentionally excluded.
func (t *Tree) OrderedRange(op string, value datum.Value, desc bool, visit func(Entry) bool) {
	begin, end, ok := t.bounds(op, value)
	if !ok {
		return
	}
	walkEntries(t.entries[begin:end], desc, visit)
}

// walkEntries visits a canonical-order slice. Descending reverses value groups
// while primary keys remain ascending inside each equal-value group, matching
// query execution's final primary-key tie-break. Reports whether the walk ran
// to completion.
func walkEntries(entries []Entry, desc bool, visit func(Entry) bool) bool {
	if !desc {
		for _, e := range entries {
			if !visit(e) {
				return false
			}
		}
		return true
	}
	for end := len(entries); end > 0; {
		start := end - 1
		for start > 0 && datum.Order(entries[start-1].Value, entries[end-1].Value) == 0 {
			start--
		}
		for _, e := range entries[start:end] {
			if !visit(e) {
				return false
			}
		}
		end = start
	}
	return true
}

// Size counts non-null value-to-key associations, the historical candidate
// size reported by /stats.
func (t *Tree) Size() int { return len(t.entries) }

// KeyCount counts distinct primary keys ever associated with this index,
// including keys that have only ever held null. It is the candidate key count
// for ordered access, which may need to visit every associated key.
func (t *Tree) KeyCount() int { return len(t.keys) }
