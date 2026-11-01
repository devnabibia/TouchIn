package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type StockOut struct {
	BranchID     int64  `json:"branch_id"`
	ProductID    int64  `json:"product_id"`
	SKU          string `json:"sku"`
	ProductName  string `json:"product_name"`
	Quantity     int64  `json:"quantity"`
	ReorderLevel int64  `json:"reorder_level"`
	LowStock     bool   `json:"low_stock"`
}

type Movement struct {
	ID             int64   `json:"id"`
	BranchID       int64   `json:"branch_id"`
	ProductID      int64   `json:"product_id"`
	Type           string  `json:"type"` // receive | adjust | sale | void | transfer_in | transfer_out
	QuantityChange int64   `json:"quantity_change"`
	Reference      *string `json:"reference"`
	Note           *string `json:"note"`
	CreatedAt      string  `json:"created_at"`
}

const movementCols = `id, branch_id, product_id, type, quantity_change, reference, note, created_at`

func scanMovement(r *sql.Rows) (Movement, error) {
	var m Movement
	err := r.Scan(&m.ID, &m.BranchID, &m.ProductID, &m.Type, &m.QuantityChange, &m.Reference, &m.Note, &m.CreatedAt)
	return m, err
}

// changeStock applies a stock delta and appends a ledger row, returning the movement id.
// Decrements are a single conditional UPDATE, so quantity can never go negative
// (a CHECK constraint backs this up). Call inside a transaction.
func changeStock(q dbtx, branchID, productID, delta int64, mtype, ref string, note *string) (int64, error) {
	if delta > 0 {
		_, err := q.Exec(`INSERT INTO stock(branch_id, product_id, quantity) VALUES (?, ?, ?)
			ON CONFLICT(branch_id, product_id) DO UPDATE SET quantity = quantity + excluded.quantity`,
			branchID, productID, delta)
		if err != nil {
			return 0, err
		}
	} else {
		res, err := q.Exec(`UPDATE stock SET quantity = quantity + ?
			WHERE branch_id = ? AND product_id = ? AND quantity >= ?`,
			delta, branchID, productID, -delta)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return 0, errf(http.StatusConflict, "insufficient stock for product %d at branch %d", productID, branchID)
		}
	}
	res, err := q.Exec(`INSERT INTO stock_movements(branch_id, product_id, type, quantity_change, reference, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		branchID, productID, mtype, delta, nz(ref), note, ts(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func movementsByID(q dbtx, ids ...int64) ([]Movement, error) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return queryAll(q, scanMovement,
		`SELECT `+movementCols+` FROM stock_movements WHERE id IN (`+placeholders(len(ids))+`) ORDER BY id`, args...)
}

func (s *Server) listStock(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r)
	branchID, productID := p.id("branch_id"), p.id("product_id")
	lowOnly := p.boolean("low_only", false)
	limit, offset := p.page(500, 5000)
	if p.err != nil {
		return p.err
	}
	var wh where
	wh.optID("s.branch_id", branchID)
	wh.optID("s.product_id", productID)
	if lowOnly {
		wh.add("s.quantity <= p.reorder_level")
	}
	out, err := queryAll(s.db, func(r *sql.Rows) (StockOut, error) {
		var o StockOut
		err := r.Scan(&o.BranchID, &o.ProductID, &o.SKU, &o.ProductName, &o.Quantity, &o.ReorderLevel)
		o.LowStock = o.Quantity <= o.ReorderLevel
		return o, err
	}, `SELECT s.branch_id, p.id, p.sku, p.name, s.quantity, p.reorder_level
		FROM stock s JOIN products p ON p.id = s.product_id`+wh.sql()+
		` ORDER BY p.name, s.branch_id LIMIT ? OFFSET ?`, append(wh.args, limit, offset)...)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) receiveStock(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		BranchID  int64   `json:"branch_id"`
		ProductID int64   `json:"product_id"`
		Quantity  int64   `json:"quantity"`
		Reference string  `json:"reference"`
		Note      *string `json:"note"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Quantity <= 0 {
		return errf(http.StatusUnprocessableEntity, "quantity must be > 0")
	}
	var mid int64
	err := withTx(s.db, func(tx *sql.Tx) (err error) {
		if _, err = activeBranch(tx, in.BranchID); err != nil {
			return
		}
		if _, err = activeProduct(tx, in.ProductID); err != nil {
			return
		}
		mid, err = changeStock(tx, in.BranchID, in.ProductID, in.Quantity, "receive", in.Reference, in.Note)
		return
	})
	if err != nil {
		return err
	}
	ms, err := movementsByID(s.db, mid)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, ms)
}

func (s *Server) adjustStock(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		BranchID       int64  `json:"branch_id"`
		ProductID      int64  `json:"product_id"`
		QuantityChange int64  `json:"quantity_change"`
		Note           string `json:"note"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.QuantityChange == 0 {
		return errf(http.StatusUnprocessableEntity, "quantity_change must be non-zero")
	}
	if strings.TrimSpace(in.Note) == "" {
		return errf(http.StatusUnprocessableEntity, "note (reason) is required")
	}
	var mid int64
	err := withTx(s.db, func(tx *sql.Tx) (err error) {
		if _, err = activeBranch(tx, in.BranchID); err != nil {
			return
		}
		if _, err = getProduct(tx, in.ProductID); err != nil {
			return
		}
		mid, err = changeStock(tx, in.BranchID, in.ProductID, in.QuantityChange, "adjust", "", &in.Note)
		return
	})
	if err != nil {
		return err
	}
	ms, err := movementsByID(s.db, mid)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, ms)
}

func (s *Server) transferStock(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		FromBranchID int64   `json:"from_branch_id"`
		ToBranchID   int64   `json:"to_branch_id"`
		ProductID    int64   `json:"product_id"`
		Quantity     int64   `json:"quantity"`
		Note         *string `json:"note"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Quantity <= 0 {
		return errf(http.StatusUnprocessableEntity, "quantity must be > 0")
	}
	if in.FromBranchID == in.ToBranchID {
		return errf(http.StatusUnprocessableEntity, "source and destination branch must differ")
	}
	ref := fmt.Sprintf("TRF-%d", time.Now().UnixNano())
	var outID, inID int64
	err := withTx(s.db, func(tx *sql.Tx) (err error) {
		if _, err = activeBranch(tx, in.FromBranchID); err != nil {
			return
		}
		if _, err = activeBranch(tx, in.ToBranchID); err != nil {
			return
		}
		if _, err = activeProduct(tx, in.ProductID); err != nil {
			return
		}
		if outID, err = changeStock(tx, in.FromBranchID, in.ProductID, -in.Quantity, "transfer_out", ref, in.Note); err != nil {
			return
		}
		inID, err = changeStock(tx, in.ToBranchID, in.ProductID, in.Quantity, "transfer_in", ref, in.Note)
		return
	})
	if err != nil {
		return err
	}
	ms, err := movementsByID(s.db, outID, inID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, ms)
}

func (s *Server) listMovements(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r)
	branchID, productID := p.id("branch_id"), p.id("product_id")
	mtype := p.str("type")
	limit, offset := p.page(100, 1000)
	if p.err != nil {
		return p.err
	}
	var wh where
	wh.optID("branch_id", branchID)
	wh.optID("product_id", productID)
	if mtype != "" {
		wh.add("type = ?", mtype)
	}
	out, err := queryAll(s.db, scanMovement,
		`SELECT `+movementCols+` FROM stock_movements`+wh.sql()+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(wh.args, limit, offset)...)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}
