// Package pricing computes USD cost from token usage.
package pricing

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/gremlord/gremlord/internal/config"
)

//go:embed prices.json
var embedded []byte

// Table maps upstream model ID -> price. Merge order (later wins):
// embedded -> ~/.gremlord/prices.json -> config `pricing:` -> per-model override.
type Table struct {
	prices map[string]config.Price
}

func Load(dataDir string, cfg *config.Config) *Table {
	t := &Table{prices: map[string]config.Price{}}
	t.mergeJSON(embedded)
	if data, err := os.ReadFile(filepath.Join(dataDir, "prices.json")); err == nil {
		t.mergeJSON(data)
	}
	if cfg != nil {
		for id, p := range cfg.Pricing {
			t.prices[id] = p
		}
		for _, m := range cfg.Models {
			if m.Pricing != nil {
				t.prices[m.ID] = *m.Pricing
			}
		}
	}
	return t
}

func (t *Table) mergeJSON(data []byte) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return
	}
	for id, v := range raw {
		if id == "_comment" {
			continue
		}
		var p config.Price
		if json.Unmarshal(v, &p) == nil {
			t.prices[id] = p
		}
	}
}

// Cost returns the USD cost for a usage row and whether the model was
// priced. Zero-price entries (local models) count as priced.
func (t *Table) Cost(model string, in, out, cacheRead, cacheWrite int64) (float64, bool) {
	p, ok := t.prices[model]
	if !ok {
		return 0, false
	}
	const m = 1e6
	cost := float64(in)/m*p.Input +
		float64(out)/m*p.Output +
		float64(cacheRead)/m*p.CacheRead +
		float64(cacheWrite)/m*p.CacheWrite
	return cost, true
}

func (t *Table) Has(model string) bool { _, ok := t.prices[model]; return ok }

func (t *Table) Get(model string) (config.Price, bool) { p, ok := t.prices[model]; return p, ok }
