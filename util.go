package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------- errors

// apiError is returned by handlers to produce a specific HTTP status.
type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return e.Msg }

func errf(status int, format string, a ...any) error {
	return &apiError{status, fmt.Sprintf(format, a...)}
}

// wrap adapts an error-returning handler and maps errors to JSON responses.
func wrap(h func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := h(w, r)
		if err == nil {
			return
		}
		var ae *apiError
		var se sqlite3.Error

switch {
case errors.As(err, &ae):
    _ = writeJSON(w, ae.Status, map[string]string{
        "error": ae.Msg,
    })

case errors.As(err, &se) && se.ExtendedCode == sqlite3.ErrConstraintUnique:
    _ = writeJSON(w, http.StatusConflict, map[string]string{
        "error": "conflict: duplicate value",
    })

case errors.As(err, &se) && se.Code == sqlite3.ErrConstraint:
    _ = writeJSON(w, http.StatusConflict, map[string]string{
        "error": "conflict: constraint violation",
    })

default:
    log.Printf("ERROR %s %s: %v", r.Method, r.URL.Path, err)
    _ = writeJSON(w, http.StatusInternalServerError, map[string]string{
        "error": "internal server error",
    })
}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errf(http.StatusBadRequest, "invalid JSON body: %v", err)
	}
	return nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, errf(http.StatusBadRequest, "invalid id %q", r.PathValue("id"))
	}
	return id, nil
}

// ---------------------------------------------------------------- database

// dbtx is satisfied by both *sql.DB and *sql.Tx.
type dbtx interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

type scanner interface{ Scan(dest ...any) error }

func withTx(db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// queryAll runs a query and scans every row. Always returns a non-nil slice
// so empty results serialise as [] rather than null.
func queryAll[T any](q dbtx, scan func(*sql.Rows) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]T, 0)
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// where builds an AND-joined WHERE clause with positional args.
type where struct {
	clauses []string
	args    []any
}

func (w *where) add(clause string, args ...any) {
	w.clauses = append(w.clauses, clause)
	w.args = append(w.args, args...)
}

func (w *where) optID(col string, v *int64) {
	if v != nil {
		w.add(col+" = ?", *v)
	}
}

func (w *where) sql() string {
	if len(w.clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.clauses, " AND ")
}

// setter builds a dynamic UPDATE ... SET list for PATCH endpoints.
type setter struct {
	cols []string
	args []any
}

func (s *setter) add(col string, v any) {
	s.cols = append(s.cols, col+" = ?")
	s.args = append(s.args, v)
}

func (s *setter) apply(q dbtx, table string, id int64) error {
	if len(s.cols) == 0 {
		return nil
	}
	_, err := q.Exec("UPDATE "+table+" SET "+strings.Join(s.cols, ", ")+" WHERE id = ?",
		append(s.args, id)...)
	return err
}

// ---------------------------------------------------------------- money & time

// Money is stored as integer cents to avoid floating-point drift in sums.
func toCents(f float64) int64 { return int64(math.Round(f * 100)) }
func money(c int64) float64   { return float64(c) / 100 }

func validMoney(f float64) bool { return !math.IsNaN(f) && f >= 0 && f < 1e12 }

// Timestamps are stored as fixed-width UTC strings so they sort lexicographically.
const tsFmt = "2006-01-02T15:04:05.000Z"

func ts(t time.Time) string { return t.UTC().Format(tsFmt) }

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------- query params

type params struct {
	v   url.Values
	err error
}

func newParams(r *http.Request) *params { return &params{v: r.URL.Query()} }

func (p *params) fail(format string, a ...any) {
	if p.err == nil {
		p.err = errf(http.StatusBadRequest, format, a...)
	}
}

func (p *params) str(k string) string { return strings.TrimSpace(p.v.Get(k)) }

func (p *params) id(k string) *int64 {
	s := p.str(k)
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		p.fail("%s must be an integer", k)
		return nil
	}
	return &n
}

func (p *params) boolean(k string, def bool) bool {
	s := p.str(k)
	if s == "" {
		return def
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		p.fail("%s must be true or false", k)
		return def
	}
	return b
}

func (p *params) page(defLimit, maxLimit int) (limit, offset int) {
	limit, offset = defLimit, 0
	if s := p.str("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxLimit {
			p.fail("limit must be between 1 and %d", maxLimit)
		} else {
			limit = n
		}
	}
	if s := p.str("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			p.fail("offset must be >= 0")
		} else {
			offset = n
		}
	}
	return
}

func (p *params) enum(k, def string, allowed ...string) string {
	s := p.str(k)
	if s == "" {
		return def
	}
	for _, a := range allowed {
		if s == a {
			return s
		}
	}
	p.fail("%s must be one of %s", k, strings.Join(allowed, ", "))
	return def
}

// when parses RFC 3339 or YYYY-MM-DD. For an upper bound given as a plain date,
// the whole day is included.
func (p *params) when(k string, upper bool) *time.Time {
	s := p.str(k)
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			if layout == "2006-01-02" && upper {
				t = t.Add(24 * time.Hour)
			}
			return &t
		}
	}
	p.fail("%s must be RFC 3339 or YYYY-MM-DD", k)
	return nil
}

// dateRange returns the [lo, hi) bounds as storage-format strings.
func (p *params) dateRange() (lo, hi string) {
	lo, hi = ts(time.Unix(0, 0)), ts(time.Now().Add(24*time.Hour))
	if t := p.when("date_from", false); t != nil {
		lo = ts(*t)
	}
	if t := p.when("date_to", true); t != nil {
		hi = ts(*t)
	}
	return
}
