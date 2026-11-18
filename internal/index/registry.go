package index

import (
	"lamina/internal/catalog"
	"lamina/internal/versions"
)

type Registry struct{ Trees map[string]*Tree }

// Build derives index trees from the retained version chains. Every retained
// live version contributes an association, so a tree can satisfy reads at any
// snapshot in the retained interval, not only the current one. Rebuild is how
// reclamation updates the trees; the retained history is the single source of
// truth.
func Build(c *catalog.Catalog, s *versions.Store) *Registry {
	r := &Registry{Trees: map[string]*Tree{}}
	for name, definition := range c.Indexes {
		tree := New()
		relation, ok := c.Relations[definition.Relation]
		if !ok {
			continue
		}
		for key, chain := range s.Tables[definition.Relation] {
			for _, v := range chain {
				if v.Deleted {
					continue
				}
				tree.Add(v.Row[definition.Column], v.Row[relation.Primary], key)
			}
		}
		r.Trees[name] = tree
	}
	return r
}

// Apply folds one committed record's writes into the existing trees. It is
// append-only: an update adds the new association and a delete adds nothing,
// because older associations may still be needed by retained snapshots. Only
// reclamation, followed by Build, removes history.
func (r *Registry) Apply(c *catalog.Catalog, writes []versions.Mutation) {
	for _, m := range writes {
		if m.Deleted {
			continue
		}
		for name, i := range c.Indexes {
			if i.Relation != m.Relation {
				continue
			}
			relation, ok := c.Relations[m.Relation]
			if !ok {
				continue
			}
			r.Trees[name].Add(m.Row[i.Column], m.Row[relation.Primary], m.Key)
		}
	}
}
