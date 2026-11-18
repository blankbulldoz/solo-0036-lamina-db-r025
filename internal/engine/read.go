package engine

import (
	"lamina/internal/catalog"
	"lamina/internal/datum"
	"lamina/internal/index"
	"lamina/internal/query"
)

func (d *DB) Query(q query.Request) (query.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.usable(); err != nil {
		return query.Result{}, err
	}
	r, err := d.catalog.Relation(q.Relation)
	if err != nil {
		return query.Result{}, invalid(err)
	}
	at := d.sequence
	var tx *Transaction
	if q.Transaction != "" {
		if q.At != nil {
			return query.Result{}, fault("invalid", "at and transaction are mutually exclusive")
		}
		tx, err = d.tx(q.Transaction)
		if err != nil {
			return query.Result{}, err
		}
		at = tx.Snapshot
	} else if q.At != nil {
		at = *q.At
	}
	if at < d.horizon || at > d.sequence {
		return query.Result{}, fault("snapshot", "snapshot unavailable; retained interval %d..%d", d.horizon, d.sequence)
	}
	if at < r.Created {
		return query.Result{}, fault("snapshot", "relation did not exist at snapshot")
	}
	if err = query.Bind(r, q); err != nil {
		return query.Result{}, invalid(err)
	}
	if tx != nil && len(tx.writes[q.Relation]) > 0 {
		q.ForceScan = true
	}
	plan, keys := query.Choose(d.catalog, d.indexes, q, at, len(d.store.Tables[q.Relation]))
	if plan.Ordered {
		keys = d.orderedCandidates(r, d.indexes.Trees[plan.Index], plan, at)
	}
	rows := map[string]datum.Row{}
	if plan.Access == "index" {
		for _, key := range keys {
			if row, exists := d.store.Read(q.Relation, key, at); exists {
				rows[key] = row
			}
		}
	} else {
		rows = d.store.Scan(q.Relation, at)
	}
	if tx != nil {
		overlay(rows, tx.writes, q.Relation)
	}
	result, err := query.Execute(r, q, rows, plan, keys)
	d.queries++
	if err != nil {
		return result, invalid(err)
	}
	return result, nil
}

// orderedCandidates produces primary keys in presentation order for a single
// ordering key backed by an index. The tree is walked in its canonical order;
// each association is re-interpreted against the requested snapshot, so only a
// key whose visible row actually carries that value is emitted, exactly once.
// Historical associations that no longer describe the visible row are skipped,
// which is why an append-only index can serve both old and new snapshots with
// the same physical order. Null placement is applied by walking the null block
// before or after the non-null values; the tree itself is never reordered.
func (d *DB) orderedCandidates(r catalog.Relation, tree *index.Tree, plan query.Plan, at uint64) []string {
	column := plan.OrderColumn
	out := make([]string, 0, plan.Candidates)
	seen := map[string]bool{}
	visit := func(e index.Entry) bool {
		if seen[e.Key] {
			return true
		}
		row, visible := d.store.Read(r.Name, e.Key, at)
		if !visible || row[column].Key() != e.Value.Key() {
			return true
		}
		seen[e.Key] = true
		out = append(out, e.Key)
		return true
	}
	if plan.RangeOp == "" {
		tree.Ordered(plan.OrderDesc, plan.NullsFirst, visit)
	} else {
		tree.OrderedRange(plan.RangeOp, plan.RangeValue, plan.OrderDesc, visit)
	}
	return out
}
func (d *DB) Get(id, table string, key datum.Value) (datum.Row, error) {
	q := query.Request{Relation: table, Transaction: id, Limit: 1}
	d.mu.RLock()
	r, err := d.catalog.Relation(table)
	d.mu.RUnlock()
	if err != nil {
		return nil, invalid(err)
	}
	col, _ := r.Column(r.Primary)
	if key.Null() || key.Kind != col.Type {
		return nil, fault("invalid", "invalid primary value")
	}
	q.Where = []query.Predicate{{Column: r.Primary, Op: "=", Value: key}}
	result, err := d.Query(q)
	if err != nil {
		return nil, err
	}
	if len(result.Rows) == 0 {
		return nil, fault("not_found", "tuple not found")
	}
	return result.Rows[0], nil
}
