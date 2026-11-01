package main

import (
	"database/sql"
	"math"
	"net/http"
)

// Aggregates over completed sales only. Sums are computed in integer cents.
const (
	unitsExpr = `COALESCE(SUM(si.quantity), 0)`
	revExpr   = `COALESCE(SUM(si.quantity * si.unit_price_cents), 0)`
	cogsExpr  = `COALESCE(SUM(si.quantity * si.unit_cost_cents), 0)`
)

type SummaryRow struct {
	Period    string  `json:"period"`
	Orders    int64   `json:"orders"`
	UnitsSold int64   `json:"units_sold"`
	Revenue   float64 `json:"revenue"`
	COGS      float64 `json:"cogs"`
	Profit    float64 `json:"profit"`
}

type SummaryTotals struct {
	Orders        int64   `json:"orders"`
	Revenue       float64 `json:"revenue"`
	Profit        float64 `json:"profit"`
	AvgOrderValue float64 `json:"avg_order_value"`
}

type SalesSummary struct {
	Totals SummaryTotals `json:"totals"`
	Series []SummaryRow  `json:"series"`
}

// salesSummary: revenue, COGS, profit and order count per day or month.
func (s *Server) salesSummary(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r)
	branchID := p.id("branch_id")
	lo, hi := p.dateRange()
	groupBy := p.enum("group_by", "day", "day", "month")
	if p.err != nil {
		return p.err
	}
	format := "%Y-%m-%d"
	if groupBy == "month" {
		format = "%Y-%m"
	}
	var wh where
	wh.add("s.status = 'completed'")
	wh.add("s.created_at >= ? AND s.created_at < ?", lo, hi)
	wh.optID("s.branch_id", branchID)

	var revTotal, cogsTotal int64
	out := SalesSummary{}
	rows, err := queryAll(s.db, func(r *sql.Rows) (SummaryRow, error) {
		var x SummaryRow
		var rev, cogs int64
		err := r.Scan(&x.Period, &x.Orders, &x.UnitsSold, &rev, &cogs)
		x.Revenue, x.COGS, x.Profit = money(rev), money(cogs), money(rev-cogs)
		revTotal += rev
		cogsTotal += cogs
		return x, err
	}, `SELECT strftime(?, s.created_at) AS period, COUNT(DISTINCT s.id), `+unitsExpr+`, `+revExpr+`, `+cogsExpr+`
		FROM sales s JOIN sale_items si ON si.sale_id = s.id`+wh.sql()+`
		GROUP BY period ORDER BY period`, append([]any{format}, wh.args...)...)
	if err != nil {
		return err
	}
	for _, x := range rows {
		out.Totals.Orders += x.Orders
	}
	out.Series = rows
	out.Totals.Revenue = money(revTotal)
	out.Totals.Profit = money(revTotal - cogsTotal)
	if out.Totals.Orders > 0 {
		out.Totals.AvgOrderValue = math.Round(float64(revTotal)/float64(out.Totals.Orders)) / 100
	}
	return writeJSON(w, http.StatusOK, out)
}

type TopProduct struct {
	ProductID int64   `json:"product_id"`
	SKU       string  `json:"sku"`
	Name      string  `json:"name"`
	UnitsSold int64   `json:"units_sold"`
	Revenue   float64 `json:"revenue"`
	Profit    float64 `json:"profit"`
}

func (s *Server) topProducts(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r)
	branchID := p.id("branch_id")
	lo, hi := p.dateRange()
	by := p.enum("by", "revenue", "revenue", "units", "profit")
	limit := 10
	if p.str("limit") != "" {
		limit, _ = p.page(10, 100)
	}
	if p.err != nil {
		return p.err
	}
	orderBy := map[string]string{
		"revenue": revExpr,
		"units":   unitsExpr,
		"profit":  "(" + revExpr + " - " + cogsExpr + ")",
	}[by] // whitelisted expression, never user input

	var wh where
	wh.add("s.status = 'completed'")
	wh.add("s.created_at >= ? AND s.created_at < ?", lo, hi)
	wh.optID("s.branch_id", branchID)

	out, err := queryAll(s.db, func(r *sql.Rows) (TopProduct, error) {
		var x TopProduct
		var rev, cogs int64
		err := r.Scan(&x.ProductID, &x.SKU, &x.Name, &x.UnitsSold, &rev, &cogs)
		x.Revenue, x.Profit = money(rev), money(rev-cogs)
		return x, err
	}, `SELECT p.id, p.sku, p.name, `+unitsExpr+`, `+revExpr+`, `+cogsExpr+`
		FROM products p
		JOIN sale_items si ON si.product_id = p.id
		JOIN sales s ON s.id = si.sale_id`+wh.sql()+`
		GROUP BY p.id ORDER BY `+orderBy+` DESC, p.id LIMIT ?`, append(wh.args, limit)...)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

type BranchPerformance struct {
	BranchID int64   `json:"branch_id"`
	Code     string  `json:"code"`
	Name     string  `json:"name"`
	Orders   int64   `json:"orders"`
	Revenue  float64 `json:"revenue"`
	Profit   float64 `json:"profit"`
}

// branchPerformance ranks every branch (including those with no sales) by revenue.
func (s *Server) branchPerformance(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r)
	lo, hi := p.dateRange()
	if p.err != nil {
		return p.err
	}
	out, err := queryAll(s.db, func(r *sql.Rows) (BranchPerformance, error) {
		var x BranchPerformance
		var rev, cogs int64
		err := r.Scan(&x.BranchID, &x.Code, &x.Name, &x.Orders, &rev, &cogs)
		x.Revenue, x.Profit = money(rev), money(rev-cogs)
		return x, err
	}, `SELECT b.id, b.code, b.name, COUNT(DISTINCT s.id), `+revExpr+`, `+cogsExpr+`
		FROM branches b
		LEFT JOIN sales s ON s.branch_id = b.id AND s.status = 'completed'
			AND s.created_at >= ? AND s.created_at < ?
		LEFT JOIN sale_items si ON si.sale_id = s.id
		GROUP BY b.id ORDER BY `+revExpr+` DESC, b.name`, lo, hi)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

type LowStockRow struct {
	BranchID         int64  `json:"branch_id"`
	Branch           string `json:"branch"`
	ProductID        int64  `json:"product_id"`
	SKU              string `json:"sku"`
	Product          string `json:"product"`
	Quantity         int64  `json:"quantity"`
	ReorderLevel     int64  `json:"reorder_level"`
	SuggestedReorder int64  `json:"suggested_reorder"` // tops stock up to 2x the reorder level
}

func (s *Server) lowStock(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r)
	branchID := p.id("branch_id")
	if p.err != nil {
		return p.err
	}
	var wh where
	wh.add("s.quantity <= p.reorder_level")
	wh.add("p.is_active = 1")
	wh.optID("b.id", branchID)
	out, err := queryAll(s.db, func(r *sql.Rows) (LowStockRow, error) {
		var x LowStockRow
		err := r.Scan(&x.BranchID, &x.Branch, &x.ProductID, &x.SKU, &x.Product, &x.Quantity, &x.ReorderLevel)
		x.SuggestedReorder = max(x.ReorderLevel*2-x.Quantity, 0)
		return x, err
	}, `SELECT b.id, b.name, p.id, p.sku, p.name, s.quantity, p.reorder_level
		FROM stock s
		JOIN branches b ON b.id = s.branch_id
		JOIN products p ON p.id = s.product_id`+wh.sql()+`
		ORDER BY s.quantity, p.name`, wh.args...)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

type ValuationRow struct {
	BranchID    int64   `json:"branch_id"`
	Branch      string  `json:"branch"`
	Units       int64   `json:"units"`
	CostValue   float64 `json:"cost_value"`
	RetailValue float64 `json:"retail_value"`
}

type Valuation struct {
	Branches []ValuationRow `json:"branches"`
	Totals   struct {
		Units       int64   `json:"units"`
		CostValue   float64 `json:"cost_value"`
		RetailValue float64 `json:"retail_value"`
	} `json:"totals"`
}

func (s *Server) inventoryValuation(w http.ResponseWriter, r *http.Request) error {
	var cost, retail int64
	var v Valuation
	rows, err := queryAll(s.db, func(r *sql.Rows) (ValuationRow, error) {
		var x ValuationRow
		var c, rt int64
		err := r.Scan(&x.BranchID, &x.Branch, &x.Units, &c, &rt)
		x.CostValue, x.RetailValue = money(c), money(rt)
		cost += c
		retail += rt
		return x, err
	}, `SELECT b.id, b.name,
			COALESCE(SUM(s.quantity), 0),
			COALESCE(SUM(s.quantity * p.cost_price_cents), 0),
			COALESCE(SUM(s.quantity * p.unit_price_cents), 0)
		FROM branches b
		LEFT JOIN stock s ON s.branch_id = b.id
		LEFT JOIN products p ON p.id = s.product_id
		GROUP BY b.id ORDER BY b.name`)
	if err != nil {
		return err
	}
	v.Branches = rows
	for _, x := range rows {
		v.Totals.Units += x.Units
	}
	v.Totals.CostValue, v.Totals.RetailValue = money(cost), money(retail)
	return writeJSON(w, http.StatusOK, v)
}
