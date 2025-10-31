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



