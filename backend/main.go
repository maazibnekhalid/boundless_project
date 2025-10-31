package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

/*
ENV:
  PORT=8080
  DB_DSN=postgres://postgres:postgres@postgres:5432/boundless?sslmode=disable
  RPC_URL=https://mainnet.base.org
*/

var (
	rpcURL = env("RPC_URL", "https://mainnet.base.org")
)

type Prover struct {
	Label string
	Addr  string
}

var provers = []Prover{
	{Label: "Prover 1", Addr: "0x6220892679110898abd78847d6f0a639e3408dc7"},
	{Label: "Prover 2", Addr: "0x34a2df023b535c1bd79a791b259adea947f603e3"},
	{Label: "Prover 3", Addr: "0x11973257c9210d852084f7b97f672080c1dbbb53"},
}

// ---- DB (collateral only) ----

func mustMigrate(db *sql.DB) {
	ddl := `
create table if not exists collateral_balances (
  id bigserial primary key,
  captured_at timestamptz not null,    -- when we captured (UTC)
  prover_addr text not null,
  balance_wei numeric not null,
  balance_eth numeric not null
);

create index if not exists idx_collateral_time on collateral_balances(captured_at);
create index if not exists idx_collateral_addr on collateral_balances(prover_addr);
`
	if _, err := db.Exec(ddl); err != nil {
		log.Fatalf("migrate: %v", err)
	}
}

// ---------- Main ----------

func main() {
	dsn := env("DB_DSN", "")
	if dsn == "skip" || strings.TrimSpace(dsn) == "" {
		log.Println("[mock mode] Skipping Postgres connection.")
		runServerWithoutDB()
		return
	}

	ctx := context.Background()

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping db: %v", err)
	}

	mustMigrate(db)

	// scrape now and then every aligned hour (…06:00, …07:00, etc.)
	go runEveryAligned(time.Hour, func(now time.Time) {
		if err := scrapeCollateral(ctx, db, now); err != nil {
			log.Printf("scrapeCollateral: %v", err)
			return
		}
	})

	// HTTP API
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	// Returns the most recent snapshot rows (one per prover) if available
	mux.HandleFunc("/api/collateral", func(w http.ResponseWriter, r *http.Request) {
		type row struct {
			CapturedAt string  `json:"captured_at"`
			Label      string  `json:"label"`
			Addr       string  `json:"addr"`
			BalanceWei float64 `json:"balance_wei"`
			BalanceEth float64 `json:"balance_eth"`
		}

		q := `
select captured_at, prover_addr, balance_wei, balance_eth
from collateral_balances
where captured_at = (select max(captured_at) from collateral_balances)
order by prover_addr;
`
		rows, err := db.QueryContext(r.Context(), q)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var out []row
		for rows.Next() {
			var captured time.Time
			var addr string
			var wei, eth float64
			if err := rows.Scan(&captured, &addr, &wei, &eth); err != nil {
				http.Error(w, "scan error", http.StatusInternalServerError)
				return
			}
			out = append(out, row{
				CapturedAt: captured.UTC().Format(time.RFC3339),
				Label:      labelFor(addr),
				Addr:       strings.ToLower(addr),
				BalanceWei: wei,
				BalanceEth: eth,
			})
		}
		writeJSON(w, out)
	})

	port := env("PORT", "8080")
	log.Printf("collateral backend listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, cors(mux)))
}

// ---------- Collateral scraper ----------

func scrapeCollateral(ctx context.Context, db *sql.DB, now time.Time) error {
	type bal struct {
		Addr string
		Wei  *big.Int
	}
	var results []bal
	for _, p := range provers {
		wei, err := ethCallBalanceOfCollateral(ctx, p.Addr)
		if err != nil {
			return fmt.Errorf("collateral %s: %w", p.Label, err)
		}
		results = append(results, bal{Addr: strings.ToLower(p.Addr), Wei: wei})
	}

	utc := now.UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt := `insert into collateral_balances(captured_at, prover_addr, balance_wei, balance_eth)
	         values ($1,$2,$3,$4)`
	for _, r := range results {
		// wei -> ETH
		eth := new(big.Rat).SetFrac(r.Wei, big.NewInt(0).Exp(big.NewInt(10), big.NewInt(18), nil))
		ethF, _ := eth.Float64()
		weiF, _ := new(big.Float).SetInt(r.Wei).Float64()

		if _, err := tx.ExecContext(ctx, stmt, utc, r.Addr, weiF, ethF); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	log.Printf("[collateral] captured_at=%s inserted=%d", utc.Format(time.RFC3339), len(results))
	return nil
}

// ---------- ETH JSON-RPC balanceOfCollateral(address) ----------

const fnSelectorBalanceOfCollateral = "b09c980b" // 0xb09c980b

func ethCallBalanceOfCollateral(ctx context.Context, addr string) (*big.Int, error) {
	addrHex := strings.TrimPrefix(strings.ToLower(addr), "0x")
	if len(addrHex) != 40 {
		return nil, fmt.Errorf("invalid address: %s", addr)
	}
	// pad to 32 bytes (64 hex chars)
	data := "0x" + fnSelectorBalanceOfCollateral + strings.Repeat("0", 64-len(addrHex)) + addrHex

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_call",
		"params": []any{map[string]any{
			"from": "0x0000000000000000000000000000000000000000",
			"to":   "0xfd152dadc5183870710fe54f939eae3ab9f0fe82",
			"data": data,
		}, "latest"},
	}
	b, _ := json.Marshal(payload)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var respBody struct {
		Result string `json:"result"`
		Error  any    `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return nil, err
	}
	if respBody.Result == "" {
		return nil, fmt.Errorf("eth_call empty result (err=%v)", respBody.Error)
	}
	hexStr := strings.TrimPrefix(respBody.Result, "0x")
	if hexStr == "" {
		return big.NewInt(0), nil
	}
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, err
	}
	// EVM returns 32-byte big-endian uint
	return new(big.Int).SetBytes(raw), nil
}

// ---------- Helpers ----------

func env(k, def string) string {
	if v := os.Getenv(k); strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func labelFor(addr string) string {
	a := strings.ToLower(addr)
	for _, p := range provers {
		if strings.ToLower(p.Addr) == a {
			return p.Label
		}
	}
	return "Unknown"
}

// Aligned scheduler: runs fn at exact aligned UTC slots (e.g., 06:00, 07:00, ...).
func runEveryAligned(interval time.Duration, fn func(now time.Time)) {
	// fire once at startup
	fn(time.Now().UTC())

	for {
		now := time.Now().UTC()
		next := nextAligned(now, interval)
		time.Sleep(time.Until(next))
		fn(next)
	}
}

func nextAligned(t time.Time, d time.Duration) time.Time {
	epoch := time.Unix(0, 0).UTC()
	elapsed := t.Sub(epoch)
	n := elapsed / d
	next := epoch.Add((n + 1) * d)
	return next.Truncate(d)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ---------- Mock server (no DB) ----------

func runServerWithoutDB() {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok (mock mode)")
	})

	mux.HandleFunc("/api/collateral", func(w http.ResponseWriter, r *http.Request) {
		fake := []map[string]any{
			{
				"captured_at": time.Now().UTC().Format(time.RFC3339),
				"label":       "Prover 1",
				"addr":        "0x6220892679110898abd78847d6f0a639e3408dc7",
				"balance_wei": "1000000000000000000",
				"balance_eth": 1.0,
			},
			{
				"captured_at": time.Now().UTC().Format(time.RFC3339),
				"label":       "Prover 2",
				"addr":        "0x34a2df023b535c1bd79a791b259adea947f603e3",
				"balance_wei": "500000000000000000",
				"balance_eth": 0.5,
			},
			{
				"captured_at": time.Now().UTC().Format(time.RFC3339),
				"label":       "Prover 3",
				"addr":        "0x11973257c9210d852084f7b97f672080c1dbbb53",
				"balance_wei": "2500000000000000000",
				"balance_eth": 2.5,
			},
		}
		writeJSON(w, fake)
	})

	port := env("PORT", "8080")
	log.Printf("[mock mode] collateral backend listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, cors(mux)))
}

// package main

// import (
// 	"bytes"
// 	"context"
// 	"database/sql"
// 	"encoding/hex"
// 	"encoding/json"
// 	"fmt"
// 	"io"
// 	"log"
// 	"math/big"
// 	"net/http"
// 	"net/url"
// 	"os"
// 	"strings"
// 	"time"

// 	_ "github.com/lib/pq"
// )

// /*
// ENV:
//   PORT=8080
//   DB_DSN=postgres://postgres:postgres@postgres:5432/boundless?sslmode=disable
//   RPC_URL=https://mainnet.base.org
//   RAILWAY_SQL=https://boundless.up.railway.app/sql/db
// */

// var (
// 	rpcURL      = env("RPC_URL", "https://mainnet.base.org")
// 	railwayBase = env("RAILWAY_SQL", "https://boundless.up.railway.app/sql/db")
// )

// type Prover struct {
// 	Label string
// 	Addr  string
// }

// var provers = []Prover{
// 	{Label: "Prover 1", Addr: "0x6220892679110898abd78847d6f0a639e3408dc7"},
// 	{Label: "Prover 2", Addr: "0x34a2df023b535c1bd79a791b259adea947f603e3"},
// 	{Label: "Prover 3", Addr: "0x11973257c9210d852084f7b97f672080c1dbbb53"},
// }

// // ---- DB ----

// func mustMigrate(db *sql.DB) {
// 	ddl := `
// create table if not exists prover_snapshots (
//   id bigserial primary key,
//   bucket text not null,                -- "YYYY-MM-DDTHH:00Z"
//   captured_at timestamptz not null,    -- when we captured (UTC)
//   prover_addr text not null,
//   orders_taken bigint not null,
//   cycles_proved numeric not null
// );

// create index if not exists idx_snapshots_bucket on prover_snapshots(bucket);
// create index if not exists idx_snapshots_addr on prover_snapshots(prover_addr);

// create table if not exists collateral_balances (
//   id bigserial primary key,
//   captured_at timestamptz not null,    -- when we captured (UTC)
//   prover_addr text not null,
//   balance_wei numeric not null,
//   balance_eth numeric not null
// );

// create index if not exists idx_collateral_time on collateral_balances(captured_at);
// create index if not exists idx_collateral_addr on collateral_balances(prover_addr);
// `
// 	if _, err := db.Exec(ddl); err != nil {
// 		log.Fatalf("migrate: %v", err)
// 	}
// }

// // ---- Types matching the Railway SQL response ----

// type apiResponse struct {
// 	Rows []row `json:"rows"`
// 	Data []row `json:"data"`
// }
// type row struct {
// 	ProverAddr   any `json:"prover_addr"`
// 	OrdersTaken  any `json:"orders_taken"`
// 	CyclesProved any `json:"cycles_proved"`
// }

// type metric struct {
// 	OrdersTaken  int64
// 	CyclesProved float64
// }

// type OutRow struct {
// 	Label        string  `json:"label"`
// 	Addr         string  `json:"addr"`
// 	OrdersTaken  int64   `json:"orders_taken"`
// 	CyclesProved float64 `json:"cycles_proved"`
// }

// func main() {
// 	// Mock mode: skip Postgres entirely for testing
// 	dsn := env("DB_DSN", "")
// 	if dsn == "skip" || dsn == "" {
// 		log.Println("[mock mode] Skipping Postgres connection.")
// 		runServerWithoutDB()
// 		return
// 	}

// 	// --- Normal mode (real Postgres path for the one-shot CLI output) ---
// 	ctx := context.Background()
// 	ctxNow := time.Now().UTC()
// 	bucket := snapshotBucketKey(ctxNow)

// 	byAddr, err := fetchMetrics(ctx)
// 	if err != nil {
// 		logStderr("ERROR fetching metrics: %v", err)
// 		os.Exit(1)
// 	}

// 	// Ensure all provers are present in output even if 0
// 	out := struct {
// 		Bucket     string   `json:"bucket"`
// 		CapturedAt string   `json:"captured_at"`
// 		Provers    []OutRow `json:"provers"`
// 	}{
// 		Bucket:     bucket,
// 		CapturedAt: ctxNow.Format(time.RFC3339),
// 	}

// 	for _, p := range provers {
// 		m := byAddr[strings.ToLower(p.Addr)]
// 		out.Provers = append(out.Provers, OutRow{
// 			Label:        p.Label,
// 			Addr:         p.Addr,
// 			OrdersTaken:  m.OrdersTaken,
// 			CyclesProved: m.CyclesProved,
// 		})
// 	}

// 	// Log compact lines + JSON
// 	fmt.Printf("[boundless] bucket=%s captured_at=%s\n", out.Bucket, out.CapturedAt)
// 	for _, r := range out.Provers {
// 		cyclesRounded := int64(r.CyclesProved + 0.5)
// 		cyclesShort := fmt.Sprintf("%d", cyclesRounded)
// 		if len(cyclesShort) > 4 {
// 			cyclesShort = cyclesShort[:4]
// 		}
// 		fmt.Printf("[boundless] %s addr=%s orders_taken=%d cycles_proved=%d cycles_short=%s\n",
// 			r.Label, r.Addr, r.OrdersTaken, cyclesRounded, cyclesShort)
// 	}
// 	if b, err := json.Marshal(out); err == nil {
// 		fmt.Printf("[boundless-json] %s\n", string(b))
// 	} else {
// 		logStderr("WARN: failed to marshal JSON output: %v", err)
// 	}

// 	// Pretty ASCII table -> file
// 	table := renderTable(out.Bucket, out.CapturedAt, out.Provers)
// 	outPath := os.Getenv("OUTPUT_FILE")
// 	if strings.TrimSpace(outPath) == "" {
// 		outPath = fmt.Sprintf("boundless_metrics_%s.txt", out.Bucket)
// 	}
// 	if err := os.WriteFile(outPath, []byte(table), 0644); err != nil {
// 		logStderr("ERROR writing table file: %v", err)
// 	} else {
// 		fmt.Printf("[boundless-file] wrote %s\n", outPath)
// 	}
// }

// // ---------- Mock server (no DB) ----------

// func runServerWithoutDB() {
// 	mux := http.NewServeMux()

// 	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
// 		fmt.Fprintln(w, "ok (mock mode)")
// 	})

// 	mux.HandleFunc("/api/snapshots", func(w http.ResponseWriter, r *http.Request) {
// 		fake := []map[string]any{
// 			{
// 				"bucket":        "2025-10-30T06:00Z",
// 				"captured_at":   time.Now().UTC().Format(time.RFC3339),
// 				"label":         "Prover 1",
// 				"addr":          "0x6220892679110898abd78847d6f0a639e3408dc7",
// 				"orders_taken":  123,
// 				"cycles_proved": 45678,
// 			},
// 			{
// 				"bucket":        "2025-10-30T06:00Z",
// 				"captured_at":   time.Now().UTC().Format(time.RFC3339),
// 				"label":         "Prover 2",
// 				"addr":          "0x34a2df023b535c1bd79a791b259adea947f603e3",
// 				"orders_taken":  234,
// 				"cycles_proved": 67890,
// 			},
// 			{
// 				"bucket":        "2025-10-30T06:00Z",
// 				"captured_at":   time.Now().UTC().Format(time.RFC3339),
// 				"label":         "Prover 3",
// 				"addr":          "0x11973257c9210d852084f7b97f672080c1dbbb53",
// 				"orders_taken":  345,
// 				"cycles_proved": 78901,
// 			},
// 		}
// 		writeJSON(w, fake)
// 	})

// 	mux.HandleFunc("/api/collateral", func(w http.ResponseWriter, r *http.Request) {
// 		fake := []map[string]any{
// 			{
// 				"captured_at": time.Now().UTC().Format(time.RFC3339),
// 				"label":       "Prover 1",
// 				"addr":        "0x6220892679110898abd78847d6f0a639e3408dc7",
// 				"balance_wei": "1000000000000000000",
// 				"balance_eth": 1.0,
// 			},
// 			{
// 				"captured_at": time.Now().UTC().Format(time.RFC3339),
// 				"label":       "Prover 2",
// 				"addr":        "0x34a2df023b535c1bd79a791b259adea947f603e3",
// 				"balance_wei": "500000000000000000",
// 				"balance_eth": 0.5,
// 			},
// 			{
// 				"captured_at": time.Now().UTC().Format(time.RFC3339),
// 				"label":       "Prover 3",
// 				"addr":        "0x11973257c9210d852084f7b97f672080c1dbbb53",
// 				"balance_wei": "2500000000000000000",
// 				"balance_eth": 2.5,
// 			},
// 		}
// 		writeJSON(w, fake)
// 	})

// 	port := env("PORT", "8080")
// 	log.Printf("[mock mode] backend listening on :%s", port)
// 	log.Fatal(http.ListenAndServe(":"+port, cors(mux)))
// }

// // ---------- Scrapers (used in real mode) ----------

// func scrapeOrdersCycles(ctx context.Context, db *sql.DB, now time.Time) error {
// 	ctxNow := now.UTC()
// 	bucket := snapshotBucketKey(ctxNow)

// 	byAddr, err := fetchMetrics(ctx)
// 	if err != nil {
// 		return err
// 	}

// 	tx, err := db.BeginTx(ctx, nil)
// 	if err != nil {
// 		return err
// 	}
// 	defer tx.Rollback()

// 	stmt := `insert into prover_snapshots(bucket, captured_at, prover_addr, orders_taken, cycles_proved)
// 	         values ($1,$2,$3,$4,$5)`
// 	for _, p := range provers {
// 		m := byAddr[strings.ToLower(p.Addr)]
// 		if _, err := tx.ExecContext(ctx, stmt, bucket, ctxNow, strings.ToLower(p.Addr), m.OrdersTaken, m.CyclesProved); err != nil {
// 			return err
// 		}
// 	}
// 	if err := tx.Commit(); err != nil {
// 		return err
// 	}
// 	log.Printf("[snapshots] bucket=%s captured_at=%s inserted=%d", bucket, ctxNow.Format(time.RFC3339), len(provers))
// 	return nil
// }

// func scrapeCollateral(ctx context.Context, db *sql.DB, now time.Time) error {
// 	type bal struct {
// 		Addr string
// 		Wei  *big.Int
// 	}
// 	var results []bal
// 	for _, p := range provers {
// 		wei, err := ethCallBalanceOfCollateral(ctx, p.Addr)
// 		if err != nil {
// 			return fmt.Errorf("collateral %s: %w", p.Label, err)
// 		}
// 		results = append(results, bal{Addr: strings.ToLower(p.Addr), Wei: wei})
// 	}
// 	utc := now.UTC()
// 	tx, err := db.BeginTx(ctx, nil)
// 	if err != nil {
// 		return err
// 	}
// 	defer tx.Rollback()
// 	stmt := `insert into collateral_balances(captured_at, prover_addr, balance_wei, balance_eth)
// 	         values ($1,$2,$3,$4)`
// 	for _, r := range results {
// 		eth := new(big.Rat).SetFrac(r.Wei, big.NewInt(0).Exp(big.NewInt(10), big.NewInt(18), nil)) // wei/1e18
// 		f, _ := eth.Float64()
// 		weiFloat, _ := new(big.Float).SetInt(r.Wei).Float64()
// 		if _, err := tx.ExecContext(ctx, stmt, utc, r.Addr, weiFloat, f); err != nil {
// 			return err
// 		}
// 	}
// 	if err := tx.Commit(); err != nil {
// 		return err
// 	}
// 	log.Printf("[collateral] captured_at=%s inserted=%d", utc.Format(time.RFC3339), len(results))
// 	return nil
// }

// // ---------- Railway metrics (orders/cycles) ----------

// func fetchMetrics(ctx context.Context) (map[string]metric, error) {
// 	sqlText := strings.TrimSpace(`
//     select
//       "orders"."prover_addr" as prover_addr,
//       count("orders"."order_id")::bigint as orders_taken,
//       coalesce(sum(coalesce("orders_executions"."total_cycles", 0)), 0)::bigint as cycles_proved
//     from "orders"
//     left join "orders_executions"
//       on "orders_executions"."order_id" = "orders"."order_id"
//     where "orders"."chain" in ($1)
//       and "orders"."prover_addr" in ($2, $3, $4)
//     group by "orders"."prover_addr"
// 	`)
// 	params := []any{
// 		"base_mainnet",
// 		strings.ToLower(provers[0].Addr),
// 		strings.ToLower(provers[1].Addr),
// 		strings.ToLower(provers[2].Addr),
// 	}
// 	payload := map[string]any{
// 		"json": map[string]any{
// 			"sql":     sqlText,
// 			"params":  params,
// 			"typings": []string{"none", "none", "none", "none"},
// 		},
// 	}
// 	b, _ := json.Marshal(payload)
// 	q := url.QueryEscape(string(b))
// 	reqURL := railwayBase + "?sql=" + q

// 	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
// 	req.Header.Set("Cache-Control", "no-store")
// 	resp, err := http.DefaultClient.Do(req)
// 	if err != nil {
// 		return nil, err
// 	}
// 	defer resp.Body.Close()
// 	if resp.StatusCode < 200 || resp.StatusCode > 299 {
// 		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
// 		return nil, fmt.Errorf("railway bad status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
// 	}

// 	var api apiResponse
// 	if err := json.NewDecoder(resp.Body).Decode(&api); err != nil {
// 		return nil, err
// 	}
// 	rows := api.Rows
// 	if len(rows) == 0 && len(api.Data) > 0 {
// 		rows = api.Data
// 	}
// 	byAddr := make(map[string]metric, len(provers))
// 	for _, r := range rows {
// 		addr := strings.ToLower(asString(r.ProverAddr))
// 		if addr == "" {
// 			continue
// 		}
// 		byAddr[addr] = metric{
// 			OrdersTaken:  asInt64(r.OrdersTaken),
// 			CyclesProved: asFloat64(r.CyclesProved),
// 		}
// 	}
// 	// ensure entries for all provers
// 	for _, p := range provers {
// 		key := strings.ToLower(p.Addr)
// 		if _, ok := byAddr[key]; !ok {
// 			byAddr[key] = metric{}
// 		}
// 	}
// 	return byAddr, nil
// }

// // ---------- ETH JSON-RPC balanceOfCollateral(address) ----------

// const fnSelectorBalanceOfCollateral = "b09c980b" // 0xb09c980b

// func ethCallBalanceOfCollateral(ctx context.Context, addr string) (*big.Int, error) {
// 	addrHex := strings.TrimPrefix(strings.ToLower(addr), "0x")
// 	if len(addrHex) != 40 {
// 		return nil, fmt.Errorf("invalid address: %s", addr)
// 	}
// 	// pad to 32 bytes (64 hex chars)
// 	data := "0x" + fnSelectorBalanceOfCollateral + strings.Repeat("0", 64-len(addrHex)) + addrHex

// 	payload := map[string]any{
// 		"jsonrpc": "2.0",
// 		"id":      1,
// 		"method":  "eth_call",
// 		"params": []any{map[string]any{
// 			"from": "0x0000000000000000000000000000000000000000",
// 			"to":   "0xfd152dadc5183870710fe54f939eae3ab9f0fe82",
// 			"data": data,
// 		}, "latest"},
// 	}
// 	b, _ := json.Marshal(payload)

// 	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(b))
// 	req.Header.Set("Content-Type", "application/json")
// 	resp, err := http.DefaultClient.Do(req)
// 	if err != nil {
// 		return nil, err
// 	}
// 	defer resp.Body.Close()

// 	var respBody struct {
// 		Result string `json:"result"`
// 		Error  any    `json:"error"`
// 	}
// 	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
// 		return nil, err
// 	}
// 	if respBody.Result == "" {
// 		return nil, fmt.Errorf("eth_call empty result (err=%v)", respBody.Error)
// 	}
// 	hexStr := strings.TrimPrefix(respBody.Result, "0x")
// 	if hexStr == "" {
// 		return big.NewInt(0), nil
// 	}
// 	raw, err := hex.DecodeString(hexStr)
// 	if err != nil {
// 		return nil, err
// 	}
// 	// EVM returns 32-byte big-endian uint
// 	return new(big.Int).SetBytes(raw), nil
// }

// // ---------- Helpers ----------

// func env(k, def string) string {
// 	if v := os.Getenv(k); strings.TrimSpace(v) != "" {
// 		return v
// 	}
// 	return def
// }

// func snapshotBucketKey(t time.Time) string { // YYYY-MM-DDTHH:00Z
// 	return fmt.Sprintf("%04d-%02d-%02dT%02d:00Z", t.UTC().Year(), t.UTC().Month(), t.UTC().Day(), t.UTC().Hour())
// }

// func labelFor(addr string) string {
// 	a := strings.ToLower(addr)
// 	for _, p := range provers {
// 		if strings.ToLower(p.Addr) == a {
// 			return p.Label
// 		}
// 	}
// 	return "Unknown"
// }

// // Aligned scheduler: runs fn at exact aligned UTC slots (e.g., 06:00, 08:00, ...).
// func runEveryAligned(interval time.Duration, fn func(now time.Time)) {
// 	// fire once at startup (optional)
// 	fn(time.Now().UTC())

// 	for {
// 		now := time.Now().UTC()
// 		next := nextAligned(now, interval)
// 		time.Sleep(time.Until(next))
// 		fn(next)
// 	}
// }
// func nextAligned(t time.Time, d time.Duration) time.Time {
// 	epoch := time.Unix(0, 0).UTC()
// 	elapsed := t.Sub(epoch)
// 	n := elapsed / d
// 	next := epoch.Add((n + 1) * d)
// 	return next.Truncate(d)
// }

// func writeJSON(w http.ResponseWriter, v any) {
// 	w.Header().Set("Content-Type", "application/json")
// 	enc := json.NewEncoder(w)
// 	enc.SetIndent("", "  ")
// 	_ = enc.Encode(v)
// }

// func asString(v any) string {
// 	switch x := v.(type) {
// 	case string:
// 		return x
// 	case fmt.Stringer:
// 		return x.String()
// 	case float64:
// 		return fmt.Sprintf("%.0f", x)
// 	case json.Number:
// 		return x.String()
// 	default:
// 		return fmt.Sprintf("%v", x)
// 	}
// }
// func asInt64(v any) int64 {
// 	switch x := v.(type) {
// 	case float64:
// 		return int64(x)
// 	case int64:
// 		return x
// 	case int:
// 		return int64(x)
// 	case json.Number:
// 		i, _ := x.Int64()
// 		return i
// 	case string:
// 		var b strings.Builder
// 		for _, r := range x {
// 			if (r >= '0' && r <= '9') || r == '-' {
// 				b.WriteRune(r)
// 			} else {
// 				break
// 			}
// 		}
// 		if b.Len() == 0 {
// 			return 0
// 		}
// 		var i int64
// 		_, err := fmt.Sscan(b.String(), &i)
// 		if err != nil {
// 			return 0
// 		}
// 		return i
// 	default:
// 		return 0
// 	}
// }
// func asFloat64(v any) float64 {
// 	switch x := v.(type) {
// 	case float64:
// 		return x
// 	case int64:
// 		return float64(x)
// 	case int:
// 		return float64(x)
// 	case json.Number:
// 		f, _ := x.Float64()
// 		return f
// 	case string:
// 		var b strings.Builder
// 		dotSeen := false
// 		for _, r := range strings.TrimSpace(x) {
// 			if r >= '0' && r <= '9' {
// 				b.WriteRune(r)
// 				continue
// 			}
// 			if r == '.' && !dotSeen {
// 				dotSeen = true
// 				b.WriteRune(r)
// 				continue
// 			}
// 			break
// 		}
// 		if b.Len() == 0 {
// 			return 0
// 		}
// 		var f float64
// 		_, err := fmt.Sscan(b.String(), &f)
// 		if err != nil {
// 			return 0
// 		}
// 		return f
// 	default:
// 		return 0
// 	}
// }

// func logStderr(format string, a ...any) {
// 	_, _ = fmt.Fprintf(os.Stderr, format+"\n", a...)
// }

// func renderTable(bucket, capturedAt string, rows []OutRow) string {
// 	var sb strings.Builder
// 	// Header
// 	sb.WriteString("Boundless Prover Metrics\n")
// 	sb.WriteString(fmt.Sprintf("Bucket:        %s\n", bucket))
// 	sb.WriteString(fmt.Sprintf("Captured (UTC): %s\n\n", capturedAt))
// 	// Table
// 	sb.WriteString(fmt.Sprintf("%-10s %-42s %12s %14s\n", "Label", "Addr", "OrdersTaken", "CyclesProved"))
// 	sb.WriteString(strings.Repeat("-", 10) + " " + strings.Repeat("-", 42) + " " +
// 		strings.Repeat("-", 12) + " " + strings.Repeat("-", 14) + "\n")
// 	for _, r := range rows {
// 		sb.WriteString(fmt.Sprintf("%-10s %-42s %12d %14.0f\n",
// 			r.Label, r.Addr, r.OrdersTaken, r.CyclesProved))
// 	}
// 	return sb.String()
// }

// // cors wraps an http.Handler to allow requests from the frontend (e.g., localhost:5173)
// func cors(h http.Handler) http.Handler {
// 	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
// 		w.Header().Set("Access-Control-Allow-Origin", "*")
// 		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
// 		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
// 		if r.Method == "OPTIONS" {
// 			return
// 		}
// 		h.ServeHTTP(w, r)
// 	})
// }
