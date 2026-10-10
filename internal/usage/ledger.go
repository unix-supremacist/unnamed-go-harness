package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The ledger sums API-reported cost per provider across ugh processes
// (~/.local/state/ugh/cost.jsonl). It is the fallback usage source for
// providers whose account usage cannot be queried, e.g. a BYOK connection.

type ledgerEntry struct {
	Provider string    `json:"p"`
	Cost     float64   `json:"c"`
	At       time.Time `json:"t"`
}

var (
	ledgerMu   sync.Mutex
	ledgerPath = defaultLedgerPath
)

func defaultLedgerPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(base) {
		base = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(base, "ugh", "cost.jsonl")
}

// Record appends one reported cost (USD). Failures are ignored: accounting
// must never break a turn.
func Record(provider string, cost float64) {
	if provider == "" || cost <= 0 {
		return
	}
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	path := ledgerPath()
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(ledgerEntry{provider, cost, time.Now().UTC()})
	_, _ = f.Write(append(b, '\n'))
}

// Spent totals the recorded cost for a provider and the number of calls.
func Spent(provider string) (total float64, calls int) {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	f, err := os.Open(ledgerPath())
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e ledgerEntry
		if json.Unmarshal([]byte(strings.TrimSpace(sc.Text())), &e) == nil && e.Provider == provider {
			total += e.Cost
			calls++
		}
	}
	return total, calls
}
