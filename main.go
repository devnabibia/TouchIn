// Multi-branch Inventory Management API.
//
//	go run .            # listens on :8080
//
// Environment:
//
//	ADDR     listen address            (default ":8080")
//	DB_PATH  SQLite file               (default "inventory.db")
//	API_KEY  if set, every request except /health must send  X-API-Key: <value>
package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const schema = `
CREATE TABLE IF NOT EXISTS branches (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	code      TEXT NOT NULL UNIQUE,
	name      TEXT NOT NULL,
	address   TEXT,
	is_active INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS products (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	sku              TEXT NOT NULL UNIQUE,
	name             TEXT NOT NULL,
	category         TEXT,
	unit_price_cents INTEGER NOT NULL CHECK (unit_price_cents >= 0),
	cost_price_cents INTEGER NOT NULL DEFAULT 0 CHECK (cost_price_cents >= 0),
	reorder_level    INTEGER NOT NULL DEFAULT 10 CHECK (reorder_level >= 0),
	is_active        INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_products_category ON products(category);
CREATE TABLE IF NOT EXISTS stock (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	branch_id  INTEGER NOT NULL REFERENCES branches(id),
	product_id INTEGER NOT NULL REFERENCES products(id),
	quantity   INTEGER NOT NULL DEFAULT 0 CHECK (quantity >= 0),
	UNIQUE (branch_id, product_id)
);
CREATE TABLE IF NOT EXISTS stock_movements (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	branch_id       INTEGER NOT NULL REFERENCES branches(id),
	product_id      INTEGER NOT NULL REFERENCES products(id),
	type            TEXT NOT NULL,
	quantity_change INTEGER NOT NULL,
	reference       TEXT,
	note            TEXT,
	created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mov_branch_product ON stock_movements(branch_id, product_id);
CREATE INDEX IF NOT EXISTS idx_mov_ref ON stock_movements(reference);
CREATE TABLE IF NOT EXISTS sales (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	branch_id   INTEGER NOT NULL REFERENCES branches(id),
	customer    TEXT,
	status      TEXT NOT NULL DEFAULT 'completed',
	total_cents INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sales_branch_date ON sales(branch_id, created_at);
CREATE INDEX IF NOT EXISTS idx_sales_created ON sales(created_at);
CREATE TABLE IF NOT EXISTS sale_items (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	sale_id          INTEGER NOT NULL REFERENCES sales(id) ON DELETE CASCADE,
	product_id       INTEGER NOT NULL REFERENCES products(id),
	quantity         INTEGER NOT NULL CHECK (quantity > 0),
	unit_price_cents INTEGER NOT NULL,
	unit_cost_cents  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_items_sale ON sale_items(sale_id);
CREATE INDEX IF NOT EXISTS idx_items_product ON sale_items(product_id);
`

func openDB(path string) (*sql.DB, error) {
	// _txlock=immediate takes the write lock at BEGIN, avoiding lock-upgrade deadlocks.
	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows a single writer; one connection keeps transactions strictly serial.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type Server struct {
	db     *sql.DB
	apiKey string
}

func newServer(db *sql.DB, apiKey string) *Server { return &Server{db: db, apiKey: apiKey} }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		_ = writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /branches", wrap(s.createBranch))
	mux.HandleFunc("GET /branches", wrap(s.listBranches))
	mux.HandleFunc("GET /branches/{id}", wrap(s.getBranchH))
	mux.HandleFunc("PATCH /branches/{id}", wrap(s.updateBranch))

	mux.HandleFunc("POST /products", wrap(s.createProduct))
	mux.HandleFunc("GET /products", wrap(s.listProducts))
	mux.HandleFunc("GET /products/{id}", wrap(s.getProductH))
	mux.HandleFunc("PATCH /products/{id}", wrap(s.updateProduct))
	mux.HandleFunc("DELETE /products/{id}", wrap(s.deactivateProduct))

	mux.HandleFunc("GET /stock", wrap(s.listStock))
	mux.HandleFunc("POST /stock/receive", wrap(s.receiveStock))
	mux.HandleFunc("POST /stock/adjust", wrap(s.adjustStock))
	mux.HandleFunc("POST /stock/transfer", wrap(s.transferStock))
	mux.HandleFunc("GET /stock/movements", wrap(s.listMovements))

	mux.HandleFunc("POST /sales", wrap(s.createSale))
	mux.HandleFunc("GET /sales", wrap(s.listSales))
	mux.HandleFunc("GET /sales/{id}", wrap(s.getSaleH))
	mux.HandleFunc("POST /sales/{id}/void", wrap(s.voidSale))

	mux.HandleFunc("GET /reports/sales-summary", wrap(s.salesSummary))
	mux.HandleFunc("GET /reports/top-products", wrap(s.topProducts))
	mux.HandleFunc("GET /reports/branch-performance", wrap(s.branchPerformance))
	mux.HandleFunc("GET /reports/low-stock", wrap(s.lowStock))
	mux.HandleFunc("GET /reports/inventory-valuation", wrap(s.inventoryValuation))

	return recoverMW(logMW(s.authMW(mux)))
}

// ---------------------------------------------------------------- middleware

func (s *Server) authMW(next http.Handler) http.Handler {
	if s.apiKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("X-API-Key"))
		if r.URL.Path != "/health" && subtle.ConstantTimeCompare(got, []byte(s.apiKey)) != 1 {
			_ = writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing API key"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.RequestURI(), sr.status, time.Since(start).Round(time.Microsecond))
	})
}

func recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC %s %s: %v", r.Method, r.URL.Path, rec)
				_ = writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- main

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	db, err := openDB(getenv("DB_PATH", "inventory.db"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	srv := &http.Server{
		Addr:              getenv("ADDR", ":8080"),
		Handler:           newServer(db, os.Getenv("API_KEY")).routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("inventory API listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
