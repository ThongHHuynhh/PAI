// Package catalog is what connectors offer people to choose: the providers a
// server may have and the presets, a full session config each, they show as
// models. The server never reads it; it takes whatever full config a
// connector sends. build/ makes catalog.json from source.json and
// models.dev, and every binary carries the one it was built with.
package catalog

import (
	_ "embed"
	"encoding/json"
	"maps"
	"slices"
)

//go:embed catalog.json
var catalogJSON []byte

type Catalog struct {
	Version   string              `json:"version"`
	Providers map[string]Provider `json:"providers"` // by id: the name a harness config gives it
	Presets   map[string]Preset   `json:"presets"`   // by id: provider/model
	Groups    map[string][]string `json:"groups"`    // preset ids, in the order to show
}

type Provider struct {
	Name    string `json:"name"`
	Shape   string `json:"shape"`
	URL     string `json:"url,omitempty"`     // the config's url, where it is not the shape's default
	Session string `json:"session,omitempty"` // the config's session header
	Group   string `json:"group,omitempty"`   // providers that take one key, one per shape a vendor's models span: a connector asks once and sets it on each
}

// Keyed is the ids of the providers that take provider id's key: its group,
// or id alone.
func (c Catalog) Keyed(id string) []string {
	g := c.Providers[id].Group
	if g == "" {
		return []string{id}
	}
	var ids []string
	for _, k := range slices.Sorted(maps.Keys(c.Providers)) {
		if c.Providers[k].Group == g {
			ids = append(ids, k)
		}
	}
	return ids
}

// Preset is a full session config but its workers.
type Preset struct {
	Name         string                     `json:"name"`
	Provider     string                     `json:"provider"`
	Model        string                     `json:"model"`
	Tools        string                     `json:"tools"`
	ContextLimit int                        `json:"context_limit"`
	Input        []string                   `json:"input"`
	Params       map[string]json.RawMessage `json:"params"`
	Cost         Cost                       `json:"cost"`
	Settings     []Setting                  `json:"settings,omitempty"`
}

// Cost is USD per million tokens.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Setting is one choice a connector may offer, such as effort. Each value is
// params merged over the preset's; the value whose params the preset's hold is
// the current one.
type Setting struct {
	Name   string  `json:"name"`
	Values []Value `json:"values"`
}

type Value struct {
	Name   string                     `json:"name"`
	Params map[string]json.RawMessage `json:"params"`
}

// Load is the catalog this binary was built with.
func Load() (Catalog, error) {
	var c Catalog
	return c, json.Unmarshal(catalogJSON, &c)
}
