package designer

// Fan-out: what a join does to the number of rows.
//
// A join between two tables on some columns repeats the rows of one side
// once per match on the other. Whether it can match MORE THAN ONE row is
// decided by the keys: matching orders.customer_id = customers.id, each
// order finds at most one customer (id is customers' primary key), but each
// customer may find many orders (customer_id is no key of orders). So,
// reading the join from customers, it FANS OUT: from here on a customer is
// as many rows as it has orders.
//
// That is harmless until something counts. A sum of customers.credit after
// that join adds each customer's credit once per order; a count(*) counts
// orders, not customers; two fan-outs from the same table (orders AND
// payments of a customer) pair every order with every payment — the "chasm
// trap". None of this is an error the database can report: the statement
// runs, and returns a confident, wrong number. Hence the rules here, which
// are the designer's reason to exist.
//
// For a table T, a tree edge multiplies T's rows when the side of the edge
// AWAY from T is not unique on the join's columns:
//
//	customers c ── orders o ── order_lines ol
//	     (c.id unique)  (o.id unique)
//
//	T = c:  c→o   far side o.customer_id not unique  ⇒ multiplies c
//	        o→ol  far side ol.order_id not unique    ⇒ multiplies c (via o)
//	T = ol: o→ol  far side o.id unique               ⇒ no
//	        c→o   far side c.id unique               ⇒ no — ol is the grain
//
// Subquery joins (exists, notexists, summary) never multiply: that is what
// they are for. A cycle join (node.extra) only removes rows.

// mult is one edge that multiplies a table's rows: the child end of the tree
// edge, and whether the keys say so for certain (known) or the far side has
// no keys to tell by (a view), when it only may.
type mult struct {
	edge  *node
	known bool
}

// edgeCols are a tree edge's columns on its parent's side and its child's.
func edgeCols(c *node) (parentCols, childCols []string) {
	return sideCols(c.via, c.fromLeft), sideCols(c.via, !c.fromLeft)
}

// multipliers lists the edges multiplying t's rows, in FROM order.
func (tr *tree) multipliers(t *node) []mult {
	var out []mult
	for _, c := range tr.order {
		if c.via == nil || c.via.Kind.subquery() || c.comp != t.comp || len(c.via.On) == 0 {
			continue
		}
		pCols, cCols := edgeCols(c)
		far, cols := c, cCols
		if t.under(c) {
			far, cols = c.parent, pCols
		}
		if u, known := tr.cat.unique(far.et, cols); !u {
			out = append(out, mult{edge: c, known: known})
		}
	}
	return out
}

// fansOut reports whether reading edge c from its parent repeats the
// parent's rows: the child's join columns are not unique.
func (tr *tree) fansOut(c *node) (yes, known bool) {
	_, cCols := edgeCols(c)
	u, known := tr.cat.unique(c.et, cCols)
	return !u, known
}

// cardinality is a join's shape read from Left to Right, as the canvas
// labels it: "N:1" is many Left rows per Right row (orders → customers),
// "1:N" the reverse, "N:N" a join on no key of either side (rows multiply
// both ways), "?" when a side has no keys to tell by.
func (tr *tree) cardinality(j *Join) string {
	l, r := tr.nodes[j.Left], tr.nodes[j.Right]
	if l == nil || r == nil || len(j.On) == 0 {
		return "?"
	}
	lu, lk := tr.cat.unique(l.et, sideCols(j, true))
	ru, rk := tr.cat.unique(r.et, sideCols(j, false))
	side := func(u, k bool) string {
		switch {
		case u:
			return "1"
		case k:
			return "N"
		}
		return "?"
	}
	return side(lu, lk) + ":" + side(ru, rk)
}
