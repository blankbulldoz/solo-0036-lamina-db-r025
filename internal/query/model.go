package query

import "lamina/internal/datum"

type Predicate struct {
	Column string      `json:"column"`
	Op     string      `json:"op"`
	Value  datum.Value `json:"value"`
}
type Ordering struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc"`
	// NullsFirst places null values before non-null values; NullsLast places
	// them after. Their effect does not depend on Desc. Neither flag (the
	// default) means the historical compatibility convention: nulls first for
	// ascending, nulls last for descending. Bind rejects setting both.
	NullsFirst bool `json:"nulls_first,omitempty"`
	NullsLast  bool `json:"nulls_last,omitempty"`
}

// NullsFirstResolved reports the effective null placement for this ordering
// key. The historical default is preserved exactly for requests that do not
// mention nulls, so old clients observe unchanged results.
func (o Ordering) NullsFirstResolved() bool {
	if o.NullsFirst {
		return true
	}
	if o.NullsLast {
		return false
	}
	return !o.Desc
}

// Explicit reports whether the client stated a null-placement policy. Requests
// that do not are served by the historical access path so their plans and rows
// stay unchanged; explicit requests may use ordered index access.
func (o Ordering) Explicit() bool { return o.NullsFirst || o.NullsLast }

type Aggregate struct {
	Function string `json:"function"`
	Column   string `json:"column,omitempty"`
}
type Request struct {
	Relation    string      `json:"relation"`
	Columns     []string    `json:"columns,omitempty"`
	Where       []Predicate `json:"where,omitempty"`
	Order       []Ordering  `json:"order,omitempty"`
	Limit       int         `json:"limit,omitempty"`
	At          *uint64     `json:"at,omitempty"`
	Transaction string      `json:"transaction,omitempty"`
	Aggregate   *Aggregate  `json:"aggregate,omitempty"`
	ForceScan   bool        `json:"force_scan,omitempty"`
}
type Plan struct {
	Access     string `json:"access"`
	Index      string `json:"index,omitempty"`
	Candidates int    `json:"candidates"`
	Snapshot   uint64 `json:"snapshot"`
	Residuals  int    `json:"residuals"`
	// Ordered reports that rows came from an index walk already in the
	// requested presentation order instead of a full materialized sort. The
	// remaining ordered-walk parameters are execution details, not output.
	Ordered     bool        `json:"ordered,omitempty"`
	OrderColumn string      `json:"-"`
	OrderDesc   bool        `json:"-"`
	NullsFirst  bool        `json:"-"`
	RangeOp     string      `json:"-"`
	RangeValue  datum.Value `json:"-"`
}
type Result struct {
	Rows      []datum.Row `json:"rows"`
	Plan      Plan        `json:"plan"`
	Scanned   int         `json:"scanned"`
	Matched   int         `json:"matched"`
	Truncated bool        `json:"truncated"`
}
