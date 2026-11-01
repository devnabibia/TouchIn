package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type SaleItem struct {
	ProductID int64   `json:"product_id"`
	Quantity  int64   `json:"quantity"`
	UnitPrice float64 `json:"unit_price"` // price snapshot at time of sale
	UnitCost  float64 `json:"unit_cost"`  // cost snapshot, used for profit reporting
}

type Sale struct {
	ID        int64      `json:"id"`
	BranchID  int64      `json:"branch_id"`
	Customer  *string    `json:"customer"`
	Status    string     `json:"status"` // completed | voided
	Total     float64    `json:"total"`
	CreatedAt string     `json:"created_at"`
	Items     []SaleItem `json:"items"`
}

const saleCols = `id, branch_id, customer, status, total_cents, created_at`

func scanSale(sc scanner) (*Sale, error) {
	var s Sale
	var total int64
	err := sc.Scan(&s.ID, &s.BranchID, &s.Customer, &s.Status, &total, &s.CreatedAt)
	s.Total = money(total)
	s.Items = []SaleItem{}
	return &s, err
}

// loadItems attaches line items to the given sales with a single query.
func loadItems(q dbtx, sales []*Sale) error {
	if len(sales) == 0 {
		return nil
	}
	byID := make(map[int64]*Sale, len(sales))
	args := make([]any, 0, len(sales))
	for _, s := range sales {
		byID[s.ID] = s
		args = append(args, s.ID)
	}
	type row struct {
		saleID int64
		item   SaleItem
	}
	rows, err := queryAll(q, func(r *sql.Rows) (row, error) {
		var x row
		var price, cost int64
		err := r.Scan(&x.saleID, &x.item.ProductID, &x.item.Quantity, &price, &cost)
		x.item.UnitPrice, x.item.UnitCost = money(price), money(cost)
		return x, err
	}, `SELECT sale_id, product_id, quantity, unit_price_cents, unit_cost_cents
		FROM sale_items WHERE sale_id IN (`+placeholders(len(args))+`) ORDER BY id`, args...)
	if err != nil {
		return err
	}
	for _, x := range rows {
		byID[x.saleID].Items = append(byID[x.saleID].Items, x.item)
	}
	return nil
}

func getSale(q dbtx, id int64) (*Sale, error) {
	s, err := scanSale(q.QueryRow(`SELECT `+saleCols+` FROM sales WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errf(http.StatusNotFound, "sale %d not found", id)
	}
	if err != nil {
		return nil, err
	}
	return s, loadItems(q, []*Sale{s})
}

func (s *Server) createSale(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		BranchID int64   `json:"branch_id"`
		Customer *string `json:"customer"`
		Items    []struct {
			ProductID int64 `json:"product_id"`
			Quantity  int64 `json:"quantity"`
		} `json:"items"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if len(in.Items) == 0 {
		return errf(http.StatusUnprocessableEntity, "items must contain at least one line")
	}
	// Merge duplicate product lines, preserving first-seen order.
	qty := map[int64]int64{}
	var order []int64
	for _, it := range in.Items {
		if it.Quantity <= 0 {
			return errf(http.StatusUnprocessableEntity, "item quantity must be > 0")
		}
		if _, seen := qty[it.ProductID]; !seen {
			order = append(order, it.ProductID)
		}
		qty[it.ProductID] += it.Quantity
	}

	var saleID int64
	// Everything happens in one transaction: if any line lacks stock, nothing is saved.
	err := withTx(s.db, func(tx *sql.Tx) error {
		if _, err := activeBranch(tx, in.BranchID); err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO sales(branch_id, customer, created_at) VALUES (?, ?, ?)`,
			in.BranchID, in.Customer, ts(time.Now()))
		if err != nil {
			return err
		}
		saleID, _ = res.LastInsertId()

		var total int64
		for _, pid := range order {
			n := qty[pid]
			p, err := activeProduct(tx, pid)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO sale_items(sale_id, product_id, quantity, unit_price_cents, unit_cost_cents)
				VALUES (?, ?, ?, ?, ?)`, saleID, pid, n, p.unitCents, p.costCents); err != nil {
				return err
			}
			if _, err := changeStock(tx, in.BranchID, pid, -n, "sale", fmt.Sprintf("SALE-%d", saleID), nil); err != nil {
				return err
			}
			total += n * p.unitCents
		}
		_, err = tx.Exec(`UPDATE sales SET total_cents = ? WHERE id = ?`, total, saleID)
		return err
	})
	if err != nil {
		return err
	}
	sale, err := getSale(s.db, saleID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, sale)
}

func (s *Server) listSales(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r)
	branchID := p.id("branch_id")
	status := p.enum("status", "", "completed", "voided")
	lo, hi := p.dateRange()
	limit, offset := p.page(50, 500)
	if p.err != nil {
		return p.err
	}
	var wh where
	wh.optID("branch_id", branchID)
	if status != "" {
		wh.add("status = ?", status)
	}
	wh.add("created_at >= ? AND created_at < ?", lo, hi)
	sales, err := queryAll(s.db, func(r *sql.Rows) (*Sale, error) { return scanSale(r) },
		`SELECT `+saleCols+` FROM sales`+wh.sql()+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(wh.args, limit, offset)...)
	if err != nil {
		return err
	}
	if err := loadItems(s.db, sales); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, sales)
}

func (s *Server) getSaleH(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	sale, err := getSale(s.db, id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, sale)
}

// voidSale cancels a sale and returns its items to the selling branch's stock.
func (s *Server) voidSale(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	err = withTx(s.db, func(tx *sql.Tx) error {
		sale, err := getSale(tx, id)
		if err != nil {
			return err
		}
		// The status guard makes a double-void impossible even under concurrency.
		res, err := tx.Exec(`UPDATE sales SET status = 'voided' WHERE id = ? AND status = 'completed'`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errf(http.StatusConflict, "sale %d is already voided", id)
		}
		for _, it := range sale.Items {
			if _, err := changeStock(tx, sale.BranchID, it.ProductID, it.Quantity, "void", fmt.Sprintf("SALE-%d", id), nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	sale, err := getSale(s.db, id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, sale)
}
