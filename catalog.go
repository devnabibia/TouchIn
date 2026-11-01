package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

// ================================================================ Branches

type Branch struct {
	ID       int64   `json:"id"`
	Code     string  `json:"code"`
	Name     string  `json:"name"`
	Address  *string `json:"address"`
	IsActive bool    `json:"is_active"`
}

const branchCols = `id, code, name, address, is_active`

func scanBranch(sc scanner) (*Branch, error) {
	var b Branch
	err := sc.Scan(&b.ID, &b.Code, &b.Name, &b.Address, &b.IsActive)
	return &b, err
}

func getBranch(q dbtx, id int64) (*Branch, error) {
	b, err := scanBranch(q.QueryRow(`SELECT `+branchCols+` FROM branches WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errf(http.StatusNotFound, "branch %d not found", id)
	}
	return b, err
}

func activeBranch(q dbtx, id int64) (*Branch, error) {
	b, err := getBranch(q, id)
	if err == nil && !b.IsActive {
		return nil, errf(http.StatusConflict, "branch %d is inactive", id)
	}
	return b, err
}

func (s *Server) createBranch(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Code    string  `json:"code"`
		Name    string  `json:"name"`
		Address *string `json:"address"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	in.Code, in.Name = strings.TrimSpace(in.Code), strings.TrimSpace(in.Name)
	if in.Code == "" || len(in.Code) > 20 {
		return errf(http.StatusUnprocessableEntity, "code is required (max 20 characters)")
	}
	if in.Name == "" {
		return errf(http.StatusUnprocessableEntity, "name is required")
	}
	res, err := s.db.Exec(`INSERT INTO branches(code, name, address) VALUES (?, ?, ?)`,
		in.Code, in.Name, in.Address)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	b, err := getBranch(s.db, id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, b)
}

func (s *Server) listBranches(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r)
	activeOnly := p.boolean("active_only", false)
	if p.err != nil {
		return p.err
	}
	var wh where
	if activeOnly {
		wh.add("is_active = 1")
	}
	out, err := queryAll(s.db, func(r *sql.Rows) (*Branch, error) { return scanBranch(r) },
		`SELECT `+branchCols+` FROM branches`+wh.sql()+` ORDER BY name`, wh.args...)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) getBranchH(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	b, err := getBranch(s.db, id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, b)
}

func (s *Server) updateBranch(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		Name     *string `json:"name"`
		Address  *string `json:"address"`
		IsActive *bool   `json:"is_active"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if _, err := getBranch(s.db, id); err != nil {
		return err
	}
	var set setter
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			return errf(http.StatusUnprocessableEntity, "name cannot be empty")
		}
		set.add("name", strings.TrimSpace(*in.Name))
	}
	if in.Address != nil {
		set.add("address", *in.Address)
	}
	if in.IsActive != nil {
		set.add("is_active", *in.IsActive)
	}
	if err := set.apply(s.db, "branches", id); err != nil {
		return err
	}
	b, err := getBranch(s.db, id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, b)
}

// ================================================================ Products

// Product prices are exposed as decimals but stored as integer cents.
type Product struct {
	ID           int64   `json:"id"`
	SKU          string  `json:"sku"`
	Name         string  `json:"name"`
	Category     *string `json:"category"`
	UnitPrice    float64 `json:"unit_price"`
	CostPrice    float64 `json:"cost_price"`
	ReorderLevel int64   `json:"reorder_level"`
	IsActive     bool    `json:"is_active"`

	unitCents, costCents int64
}

const productCols = `id, sku, name, category, unit_price_cents, cost_price_cents, reorder_level, is_active`

func scanProduct(sc scanner) (*Product, error) {
	var p Product
	err := sc.Scan(&p.ID, &p.SKU, &p.Name, &p.Category, &p.unitCents, &p.costCents, &p.ReorderLevel, &p.IsActive)
	p.UnitPrice, p.CostPrice = money(p.unitCents), money(p.costCents)
	return &p, err
}

func getProduct(q dbtx, id int64) (*Product, error) {
	p, err := scanProduct(q.QueryRow(`SELECT `+productCols+` FROM products WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errf(http.StatusNotFound, "product %d not found", id)
	}
	return p, err
}

func activeProduct(q dbtx, id int64) (*Product, error) {
	p, err := getProduct(q, id)
	if err == nil && !p.IsActive {
		return nil, errf(http.StatusConflict, "product %d is inactive", id)
	}
	return p, err
}

func (s *Server) createProduct(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		SKU          string  `json:"sku"`
		Name         string  `json:"name"`
		Category     *string `json:"category"`
		UnitPrice    float64 `json:"unit_price"`
		CostPrice    float64 `json:"cost_price"`
		ReorderLevel *int64  `json:"reorder_level"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	in.SKU, in.Name = strings.TrimSpace(in.SKU), strings.TrimSpace(in.Name)
	reorder := int64(10)
	if in.ReorderLevel != nil {
		reorder = *in.ReorderLevel
	}
	switch {
	case in.SKU == "" || len(in.SKU) > 50:
		return errf(http.StatusUnprocessableEntity, "sku is required (max 50 characters)")
	case in.Name == "":
		return errf(http.StatusUnprocessableEntity, "name is required")
	case !validMoney(in.UnitPrice) || !validMoney(in.CostPrice):
		return errf(http.StatusUnprocessableEntity, "unit_price and cost_price must be >= 0")
	case reorder < 0:
		return errf(http.StatusUnprocessableEntity, "reorder_level must be >= 0")
	}
	res, err := s.db.Exec(`INSERT INTO products(sku, name, category, unit_price_cents, cost_price_cents, reorder_level)
		VALUES (?, ?, ?, ?, ?, ?)`,
		in.SKU, in.Name, in.Category, toCents(in.UnitPrice), toCents(in.CostPrice), reorder)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	p, err := getProduct(s.db, id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, p)
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Server) listProducts(w http.ResponseWriter, r *http.Request) error {
	p := newParams(r)
	q, category := p.str("q"), p.str("category")
	activeOnly := p.boolean("active_only", true)
	limit, offset := p.page(50, 500)
	if p.err != nil {
		return p.err
	}
	var wh where
	if q != "" {
		like := "%" + escapeLike(q) + "%"
		wh.add(`(name LIKE ? ESCAPE '\' OR sku LIKE ? ESCAPE '\')`, like, like)
	}
	if category != "" {
		wh.add("category = ?", category)
	}
	if activeOnly {
		wh.add("is_active = 1")
	}
	out, err := queryAll(s.db, func(r *sql.Rows) (*Product, error) { return scanProduct(r) },
		`SELECT `+productCols+` FROM products`+wh.sql()+` ORDER BY name LIMIT ? OFFSET ?`,
		append(wh.args, limit, offset)...)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) getProductH(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	p, err := getProduct(s.db, id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, p)
}

func (s *Server) updateProduct(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		Name         *string  `json:"name"`
		Category     *string  `json:"category"`
		UnitPrice    *float64 `json:"unit_price"`
		CostPrice    *float64 `json:"cost_price"`
		ReorderLevel *int64   `json:"reorder_level"`
		IsActive     *bool    `json:"is_active"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if _, err := getProduct(s.db, id); err != nil {
		return err
	}
	var set setter
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			return errf(http.StatusUnprocessableEntity, "name cannot be empty")
		}
		set.add("name", strings.TrimSpace(*in.Name))
	}
	if in.Category != nil {
		set.add("category", *in.Category)
	}
	if in.UnitPrice != nil {
		if !validMoney(*in.UnitPrice) {
			return errf(http.StatusUnprocessableEntity, "unit_price must be >= 0")
		}
		set.add("unit_price_cents", toCents(*in.UnitPrice))
	}
	if in.CostPrice != nil {
		if !validMoney(*in.CostPrice) {
			return errf(http.StatusUnprocessableEntity, "cost_price must be >= 0")
		}
		set.add("cost_price_cents", toCents(*in.CostPrice))
	}
	if in.ReorderLevel != nil {
		if *in.ReorderLevel < 0 {
			return errf(http.StatusUnprocessableEntity, "reorder_level must be >= 0")
		}
		set.add("reorder_level", *in.ReorderLevel)
	}
	if in.IsActive != nil {
		set.add("is_active", *in.IsActive)
	}
	if err := set.apply(s.db, "products", id); err != nil {
		return err
	}
	p, err := getProduct(s.db, id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, p)
}

// deactivateProduct is a soft delete so historical sales keep their product reference.
func (s *Server) deactivateProduct(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if _, err := getProduct(s.db, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE products SET is_active = 0 WHERE id = ?`, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
