package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
)

type client struct {
	t    *testing.T
	base string
	key  string
}

func newTestClient(t *testing.T, key string) *client {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newServer(db, key).routes())
	t.Cleanup(func() { srv.Close(); db.Close() })
	return &client{t: t, base: srv.URL, key: key}
}

// do sends a request, decodes a 2xx JSON body into out, and returns the status code.
func (c *client) do(method, path string, body, out any) int {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("X-API-Key", c.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Errorf("%s %s: %v", method, path, err)
		return -1
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			c.t.Errorf("decode %s %s: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func (c *client) expect(want int, method, path string, body, out any) {
	c.t.Helper()
	if got := c.do(method, path, body, out); got != want {
		c.t.Fatalf("%s %s: got status %d, want %d", method, path, got, want)
	}
}

type m = map[string]any

func TestFullFlow(t *testing.T) {
	c := newTestClient(t, "")
	var nbo, ksm Branch
	var prod Product
	c.expect(201, "POST", "/branches", m{"code": "NBO", "name": "Nairobi"}, &nbo)
	c.expect(201, "POST", "/branches", m{"code": "KSM", "name": "Kisumu"}, &ksm)
	c.expect(409, "POST", "/branches", m{"code": "NBO", "name": "Dup"}, nil)
	c.expect(422, "POST", "/branches", m{"code": "", "name": "x"}, nil)
	c.expect(400, "POST", "/branches", m{"code": "Z", "name": "x", "bogus": 1}, nil)
	c.expect(404, "GET", "/branches/999", nil, nil)

	c.expect(201, "POST", "/products", m{"sku": "A1", "name": "Widget", "unit_price": 10.10,
		"cost_price": 6.05, "reorder_level": 5}, &prod)
	c.expect(409, "POST", "/products", m{"sku": "A1", "name": "Dup", "unit_price": 1}, nil)
	if prod.UnitPrice != 10.10 || prod.CostPrice != 6.05 {
		t.Fatalf("price round-trip wrong: %+v", prod)
	}

	stockQty := func(branch int64) int64 {
		var st []StockOut
		c.expect(200, "GET", "/stock?branch_id="+itoa(branch), nil, &st)
		if len(st) == 0 {
			return 0
		}
		return st[0].Quantity
	}

	c.expect(201, "POST", "/stock/receive", m{"branch_id": nbo.ID, "product_id": prod.ID, "quantity": 20}, nil)

	// Overselling is rejected and nothing is persisted.
	c.expect(409, "POST", "/sales", m{"branch_id": nbo.ID, "items": []m{{"product_id": prod.ID, "quantity": 21}}}, nil)
	var sales []Sale
	c.expect(200, "GET", "/sales", nil, &sales)
	if len(sales) != 0 {
		t.Fatalf("failed sale was persisted: %+v", sales)
	}

	// Duplicate lines merge; totals use cents.
	var sale Sale
	c.expect(201, "POST", "/sales", m{"branch_id": nbo.ID, "items": []m{
		{"product_id": prod.ID, "quantity": 4}, {"product_id": prod.ID, "quantity": 1}}}, &sale)
	if sale.Total != 50.50 || len(sale.Items) != 1 || sale.Items[0].Quantity != 5 {
		t.Fatalf("unexpected sale: %+v", sale)
	}
	if q := stockQty(nbo.ID); q != 15 {
		t.Fatalf("stock after sale = %d, want 15", q)
	}

	// Transfer 12 to Kisumu -> Nairobi has 3 left (<= reorder level 5).
	var moves []Movement
	c.expect(201, "POST", "/stock/transfer", m{"from_branch_id": nbo.ID, "to_branch_id": ksm.ID,
		"product_id": prod.ID, "quantity": 12}, &moves)
	if len(moves) != 2 {
		t.Fatalf("transfer should return 2 movements, got %d", len(moves))
	}
	c.expect(422, "POST", "/stock/transfer", m{"from_branch_id": nbo.ID, "to_branch_id": nbo.ID,
		"product_id": prod.ID, "quantity": 1}, nil)
	c.expect(409, "POST", "/stock/transfer", m{"from_branch_id": nbo.ID, "to_branch_id": ksm.ID,
		"product_id": prod.ID, "quantity": 99}, nil)
	var low []LowStockRow
	c.expect(200, "GET", "/reports/low-stock", nil, &low)
	if len(low) != 1 || low[0].Branch != "Nairobi" || low[0].SuggestedReorder != 7 {
		t.Fatalf("low stock report wrong: %+v", low)
	}

	// Void returns items; second void conflicts.
	c.expect(200, "POST", "/sales/"+itoa(sale.ID)+"/void", nil, &sale)
	if sale.Status != "voided" {
		t.Fatalf("status = %s", sale.Status)
	}
	c.expect(409, "POST", "/sales/"+itoa(sale.ID)+"/void", nil, nil)
	if q := stockQty(nbo.ID); q != 8 {
		t.Fatalf("stock after void = %d, want 8", q)
	}

	// Adjustment (damaged goods) and the ledger.
	c.expect(201, "POST", "/stock/adjust", m{"branch_id": nbo.ID, "product_id": prod.ID,
		"quantity_change": -2, "note": "damaged"}, nil)
	c.expect(409, "POST", "/stock/adjust", m{"branch_id": nbo.ID, "product_id": prod.ID,
		"quantity_change": -100, "note": "oops"}, nil)
	c.expect(200, "GET", "/stock/movements?branch_id="+itoa(nbo.ID)+"&type=adjust", nil, &moves)
	if len(moves) != 1 || moves[0].QuantityChange != -2 {
		t.Fatalf("movements wrong: %+v", moves)
	}

	// Reports: only the Kisumu sale (3 x 10.10) counts; the voided sale is excluded.
	c.expect(201, "POST", "/sales", m{"branch_id": ksm.ID, "customer": "Amina",
		"items": []m{{"product_id": prod.ID, "quantity": 3}}}, nil)
	var sum SalesSummary
	c.expect(200, "GET", "/reports/sales-summary?group_by=month", nil, &sum)
	if sum.Totals.Revenue != 30.30 || sum.Totals.Profit != 12.15 || sum.Totals.Orders != 1 || sum.Totals.AvgOrderValue != 30.30 {
		t.Fatalf("summary wrong: %+v", sum)
	}
	var top []TopProduct
	c.expect(200, "GET", "/reports/top-products?by=units", nil, &top)
	if len(top) != 1 || top[0].UnitsSold != 3 {
		t.Fatalf("top products wrong: %+v", top)
	}
	var perf []BranchPerformance
	c.expect(200, "GET", "/reports/branch-performance", nil, &perf)
	if len(perf) != 2 || perf[0].Code != "KSM" || perf[1].Revenue != 0 {
		t.Fatalf("branch performance wrong: %+v", perf)
	}
	var val Valuation
	c.expect(200, "GET", "/reports/inventory-valuation", nil, &val)
	if val.Totals.Units != 6+9 || val.Totals.CostValue != 15*6.05 {
		t.Fatalf("valuation wrong: %+v", val)
	}

	// Soft delete: product disappears from default listing and can't be sold.
	c.expect(204, "DELETE", "/products/"+itoa(prod.ID), nil, nil)
	var plist []Product
	c.expect(200, "GET", "/products", nil, &plist)
	if len(plist) != 0 {
		t.Fatalf("inactive product listed: %+v", plist)
	}
	c.expect(409, "POST", "/sales", m{"branch_id": nbo.ID, "items": []m{{"product_id": prod.ID, "quantity": 1}}}, nil)
}

// 25 concurrent buyers compete for 10 units: exactly 10 must succeed.
func TestConcurrentSalesNeverOversell(t *testing.T) {
	c := newTestClient(t, "")
	var b Branch
	var p Product
	c.expect(201, "POST", "/branches", m{"code": "B1", "name": "Main"}, &b)
	c.expect(201, "POST", "/products", m{"sku": "X", "name": "X", "unit_price": 1}, &p)
	c.expect(201, "POST", "/stock/receive", m{"branch_id": b.ID, "product_id": p.ID, "quantity": 10}, nil)

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, conflict := 0, 0
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code := c.do("POST", "/sales", m{"branch_id": b.ID, "items": []m{{"product_id": p.ID, "quantity": 1}}}, nil)
			mu.Lock()
			defer mu.Unlock()
			switch code {
			case 201:
				ok++
			case 409:
				conflict++
			}
		}()
	}
	wg.Wait()
	if ok != 10 || conflict != 15 {
		t.Fatalf("ok=%d conflict=%d, want 10/15", ok, conflict)
	}
	var st []StockOut
	c.expect(200, "GET", "/stock", nil, &st)
	if st[0].Quantity != 0 {
		t.Fatalf("final stock = %d, want 0", st[0].Quantity)
	}
}

func TestAPIKey(t *testing.T) {
	c := newTestClient(t, "secret")
	good := c.key
	c.key = ""
	c.expect(401, "GET", "/branches", nil, nil)
	c.expect(200, "GET", "/health", nil, nil) // health stays open for load balancers
	c.key = "wrong"
	c.expect(401, "GET", "/branches", nil, nil)
	c.key = good
	c.expect(200, "GET", "/branches", nil, nil)
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
