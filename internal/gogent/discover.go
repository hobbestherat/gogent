package gogent

import (
	"context"
	"fmt"
	"strings"

	"gogent/internal/config"
	"gogent/internal/model"
	"gogent/internal/modelsdev"
)

// This file holds the discovery orchestration: it merges a connection's LIVE model
// listing with the models.dev CATALOG into one availability-aware, capability-rich
// result (see internal/model/discover.go for the pure merge primitives and
// docs/model-discovery-redesign.md for the design).

// catalogClient lazily builds (once) the models.dev client rooted at the user's
// home dir. It is safe to call without the catalog ever being fetched.
func (g *Gogent) catalogClient() *modelsdev.Client {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.catalog == nil {
		g.catalog = modelsdev.NewClient(g.homeDir)
	}
	return g.catalog
}

// ModelCatalog returns the models.dev catalog (cached; force bypasses the TTL).
// It is the single catalog entry point the handlers expose to the UI.
func (g *Gogent) ModelCatalog(ctx context.Context, force bool) (modelsdev.Catalog, error) {
	cat, err := g.catalogClient().Catalog(ctx, force)
	if err != nil {
		return nil, fmt.Errorf("models.dev catalog: %w", err)
	}
	return cat, nil
}

// catalogLookup adapts a models.dev catalog (filtered to one api_type) to the
// model.CatalogLookup interface the merge consumes. It is built per discovery call.
type catalogLookup struct {
	exact  map[string]config.ModelCapabilities
	family map[string]config.ModelCapabilities
	all    []model.CatalogEntry
}

func (l *catalogLookup) Exact(_ model.APIType, normID string) (config.ModelCapabilities, bool) {
	c, ok := l.exact[normID]
	return c, ok
}

func (l *catalogLookup) Family(_ model.APIType, fam string) (config.ModelCapabilities, bool) {
	c, ok := l.family[fam]
	return c, ok
}

func (l *catalogLookup) All(_ model.APIType) []model.CatalogEntry { return l.all }

// newCatalogLookup gathers every catalog model whose provider maps to apiType
// (e.g. zai ← zai/zai-coding-plan/zhipuai) into exact-id, family, and full lists.
// Returns nil when cat is empty so discovery degrades to live-only/manual.
func newCatalogLookup(cat modelsdev.Catalog, apiType string) *catalogLookup {
	if len(cat) == 0 {
		return nil
	}
	at := model.StringToAPIType(apiType)
	want := catalogAPITypeFor(apiType)
	l := &catalogLookup{
		exact:  map[string]config.ModelCapabilities{},
		family: map[string]config.ModelCapabilities{},
	}
	for _, p := range cat {
		if modelsdev.ProviderAPIType(p) != want {
			continue
		}
		for _, m := range p.Models {
			caps := modelsdev.ToModelCapabilities(m)
			norm := model.NormalizeModelID(at, m.ID)
			if norm == "" {
				continue
			}
			if _, dup := l.exact[norm]; !dup {
				l.exact[norm] = caps
			}
			if fam := model.FamilyKey(norm); fam != "" {
				if _, dup := l.family[fam]; !dup {
					l.family[fam] = caps
				}
			}
			l.all = append(l.all, model.CatalogEntry{
				ID:          m.ID,
				DisplayName: m.Name,
				NormID:      norm,
				Caps:        caps,
			})
		}
	}
	if len(l.exact) == 0 && len(l.all) == 0 {
		return nil
	}
	return l
}

// catalogAPITypeFor maps a connection's api_type to the api_type its catalog
// models are filed under (models.dev catalog providers). The native Gemini route
// (vertex-native) and the OpenAI-compat shim (vertex) both draw Gemini caps from
// the catalog's google-vertex provider, which ProviderAPIType files under "vertex".
func catalogAPITypeFor(connAPIType string) string {
	if connAPIType == "vertex-native" {
		return "vertex"
	}
	return connAPIType
}

// DiscoverModels merges the named connection's live model listing with the
// models.dev catalog into a capability-rich, availability-aware list. Live ids are
// flagged Available; catalog-only models (not returned by the listing) are included
// flagged unavailable so the user can still pick one. A listing error is tolerated
// when the catalog can still populate the list (local/credential-less browse); it is
// only surfaced when neither source yields anything.
func (g *Gogent) DiscoverModels(ctx context.Context, connName string) ([]model.DiscoveredModel, error) {
	g.mu.RLock()
	pc := g.config.GetConnection(connName)
	g.mu.RUnlock()
	if pc == nil {
		return nil, fmt.Errorf("unknown connection %q", connName)
	}

	// Live listing: probe with a model-less connection (only api_type/endpoint/auth
	// matter). A listing failure is captured, not fatal — the catalog may still fill in.
	conn := model.NewProbeConnection(pc)
	live, listErr := conn.ListModels()

	cat, _ := g.ModelCatalog(ctx, false) // catalog errors are non-fatal for discovery
	// Assign through an interface var only when non-nil: a typed-nil *catalogLookup
	// stored in a model.CatalogLookup interface is NOT == nil and would panic on use.
	var lookup model.CatalogLookup
	if l := newCatalogLookup(cat, strings.TrimSpace(pc.APIType)); l != nil {
		lookup = l
	}

	merged := model.MergeDiscovery(model.StringToAPIType(pc.APIType), live, lookup)
	if len(merged) == 0 {
		if listErr != nil {
			return nil, fmt.Errorf("list models: %w", listErr)
		}
		return nil, fmt.Errorf("no models found for connection %q", connName)
	}
	return merged, nil
}

// ProbeConnection lists the raw advertised model ids for a connection (no catalog
// merge). It is the lightweight "can I reach this backend?" check used when the
// caller only needs availability, e.g. validating credentials.
func (g *Gogent) ProbeConnection(connName string) ([]string, error) {
	g.mu.RLock()
	pc := g.config.GetConnection(connName)
	g.mu.RUnlock()
	if pc == nil {
		return nil, fmt.Errorf("unknown connection %q", connName)
	}
	conn := model.NewProbeConnection(pc)
	infos, err := conn.ListModels()
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		if info.ID != "" {
			ids = append(ids, info.ID)
		}
	}
	return ids, nil
}
