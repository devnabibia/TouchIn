# Inventory Management API (Go)

Multi-branch inventory: stock tracking, sales, products, branches, reporting.
Standard library `net/http` (Go 1.22 routing) + SQLite (`mattn/go-sqlite3`, needs a C compiler).

```bash
go run .                 # http://localhost:8080
go test -race ./...
```

| Env | Default | Meaning |
|---|---|---|
| `ADDR` | `:8080` | listen address |
| `DB_PATH` | `inventory.db` | SQLite file |
| `API_KEY` | _(unset)_ | if set, send `X-API-Key` on every request except `/health` |

## Endpoints

| Area | Endpoints |
|---|---|
| Branches | `POST/GET /branches`, `GET/PATCH /branches/{id}` |
| Products | `POST/GET /products`, `GET/PATCH/DELETE /products/{id}` (DELETE = soft) |
| Stock | `GET /stock`, `POST /stock/receive`, `/stock/adjust`, `/stock/transfer`, `GET /stock/movements` |
| Sales | `POST/GET /sales`, `GET /sales/{id}`, `POST /sales/{id}/void` |
| Reports | `/reports/sales-summary`, `top-products`, `branch-performance`, `low-stock`, `inventory-valuation` |

List endpoints take `limit`/`offset`. Date filters (`date_from`, `date_to`) accept RFC 3339 or `YYYY-MM-DD`;
a plain-date `date_to` includes that whole day. Errors are `{"error": "..."}`.

## Example

```bash
curl -XPOST :8080/branches -d '{"code":"NBO","name":"Nairobi"}'
curl -XPOST :8080/products -d '{"sku":"A1","name":"Widget","unit_price":10.10,"cost_price":6.05,"reorder_level":5}'
curl -XPOST :8080/stock/receive -d '{"branch_id":1,"product_id":1,"quantity":20}'
curl -XPOST :8080/sales -d '{"branch_id":1,"items":[{"product_id":1,"quantity":3}]}'
curl ':8080/reports/sales-summary?group_by=month'
```

## Design notes
- **No overselling:** decrements are one conditional `UPDATE ... WHERE quantity >= n` inside a transaction,
  backed by a `CHECK (quantity >= 0)`. A sale with any short line is rolled back entirely.
- **Ledger:** every stock change writes a row to `stock_movements`.
- **Money** is stored as integer cents, so sums never drift; the API exposes decimals.
- **Sales snapshot** the price and cost at sale time, so later price edits don't rewrite history or profit.
- SQLite runs with one connection (single writer, WAL, immediate transactions).
