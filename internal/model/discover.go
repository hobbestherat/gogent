package model

import (
	"strings"

	"gogent/internal/config"
)

// Discovery merges two independent sources into one capability-rich, availability-
// aware model list: a backend's LIVE listing (what THIS connection can actually
// reach) and the models.dev CATALOG (what each model can do). Neither alone is
// sufficient — live endpoints often return ids only (Z.AI, Vertex), while the
// catalog cannot know which models a given key/project may use.
//
// This file holds the pure, modelsdev-independent primitives — the DiscoveredModel
// type, id normalization for the join, and capability merging by precedence. The
// orchestration that actually fetches the catalog and calls ListModels lives in the
// gogent layer (which already wires both), so the model package stays liftable.

// DiscoverySource records where a discovered model's capabilities came from.
const (
	SourceMerged  = "merged"  // live ids + catalog caps
	SourceLive    = "live"    // capabilities self-described by the live endpoint
	SourceCatalog = "catalog" // catalog-only (not seen in the live listing)
	SourceManual  = "manual"  // neither described it; user fills caps by hand
)

// DiscoveredModel is one entry in a discovery result: a backend model id, its
// merged capabilities, and whether the live listing confirmed it is available to
// this connection. Catalog-only entries (Available=false) are included so the user
// can pick a model the listing didn't surface (e.g. one needing access), clearly
// flagged in the UI.
type DiscoveredModel struct {
	ID          string
	DisplayName string
	APIType     APIType
	Available   bool
	InCatalog   bool
	Caps        config.ModelCapabilities
}

// NormalizeModelID reduces a model id to a stable key for matching a live listing
// entry against a catalog entry, per the provider's id conventions:
//
//   - strip a dated/pinned snapshot suffix ("@20251101", ":free", trailing date)
//   - drop a publisher/vendor prefix so the vertex shim's "google/gemini-3.5-flash",
//     vertex-native's bare "gemini-3.5-flash", and OpenRouter's "google/gemini-…"
//     all collapse to the same key
//
// It is used ONLY for matching; the original id is preserved on the saved config.
func NormalizeModelID(apiType APIType, id string) string {
	s := strings.ToLower(strings.TrimSpace(id))
	// Strip an OpenRouter free/variant suffix.
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	// Strip a pinned snapshot suffix.
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	// Drop a single leading publisher/vendor segment (google/…, anthropic/…,
	// z-ai/…) so shim/native/gateway ids of the same model match.
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// FamilyKey reduces a normalized id to a coarse family key for the catalog
// family-match fallback (e.g. "llama-3.3-70b-instruct-turbo" and
// "llama-3.3-70b-versatile" share "llama-3.3-70b"). It keeps the leading
// alphanumeric/version segments and drops trailing qualifier words. Best-effort.
func FamilyKey(normID string) string {
	parts := strings.Split(normID, "-")
	// Keep segments up to and including the last that looks like a version/size
	// token (contains a digit); drop trailing pure-word qualifiers.
	last := 0
	for i, p := range parts {
		if strings.ContainsAny(p, "0123456789") {
			last = i
		}
	}
	if last == 0 {
		return normID
	}
	return strings.Join(parts[:last+1], "-")
}

// MergeCaps combines capabilities by precedence: a non-empty live value wins over
// the catalog (the live endpoint reflects the account's actual limits), and the
// catalog fills whatever the live listing didn't describe. Booleans OR together
// (either source asserting a capability is enough); numeric/string fields prefer
// live when set, else catalog. The returned Source reflects the strongest tier.
func MergeCaps(live *config.ModelCapabilities, catalog *config.ModelCapabilities) config.ModelCapabilities {
	var out config.ModelCapabilities
	switch {
	case live != nil && catalog != nil:
		out = *catalog
		mergeFrom(&out, live)
		out.Source = SourceMerged
	case live != nil:
		out = *live
		if out.Source == "" {
			out.Source = SourceLive
		}
	case catalog != nil:
		out = *catalog
		if out.Source == "" {
			out.Source = SourceCatalog
		}
	default:
		out.Source = SourceManual
	}
	return out
}

// mergeFrom overlays the set fields of src onto dst (live ▸ catalog).
func mergeFrom(dst *config.ModelCapabilities, src *config.ModelCapabilities) {
	if src.ContextWindow > 0 {
		dst.ContextWindow = src.ContextWindow
	}
	if src.MaxOutput > 0 {
		dst.MaxOutput = src.MaxOutput
	}
	dst.Reasoning = dst.Reasoning || src.Reasoning
	dst.ThinkingToggle = dst.ThinkingToggle || src.ThinkingToggle
	dst.Vision = dst.Vision || src.Vision
	dst.ToolCall = dst.ToolCall || src.ToolCall
	dst.StructuredOutput = dst.StructuredOutput || src.StructuredOutput
	dst.CustomTemp = dst.CustomTemp || src.CustomTemp
	if len(src.EffortOptions) > 0 {
		dst.EffortOptions = src.EffortOptions
	}
	if len(src.InputModalities) > 0 {
		dst.InputModalities = src.InputModalities
	}
	if len(src.OutputModalities) > 0 {
		dst.OutputModalities = src.OutputModalities
	}
	if src.InputCostPerM > 0 {
		dst.InputCostPerM = src.InputCostPerM
	}
	if src.OutputCostPerM > 0 {
		dst.OutputCostPerM = src.OutputCostPerM
	}
	if src.CacheReadPerM > 0 {
		dst.CacheReadPerM = src.CacheReadPerM
	}
	if src.CacheWritePerM > 0 {
		dst.CacheWritePerM = src.CacheWritePerM
	}
	if src.Knowledge != "" {
		dst.Knowledge = src.Knowledge
	}
	if src.ReleaseDate != "" {
		dst.ReleaseDate = src.ReleaseDate
	}
}

// CatalogLookup resolves a model id (already normalized) to catalog capabilities
// for a given api_type, returning false when the catalog has no entry. The gogent
// layer implements it over modelsdev; keeping it an interface here avoids coupling
// the model package to the catalog package.
type CatalogLookup interface {
	// Exact returns caps for an exact normalized-id match.
	Exact(apiType APIType, normID string) (config.ModelCapabilities, bool)
	// Family returns caps for a coarser family match (fallback).
	Family(apiType APIType, fam string) (config.ModelCapabilities, bool)
	// All returns every catalog model for the api_type, as (origID, normID, caps),
	// so catalog-only entries can be surfaced (flagged unavailable).
	All(apiType APIType) []CatalogEntry
}

// CatalogEntry is one catalog model exposed to discovery.
type CatalogEntry struct {
	ID          string
	DisplayName string
	NormID      string
	Caps        config.ModelCapabilities
}

// MergeDiscovery joins a live listing with a catalog lookup into the unified,
// availability-aware result. live may be empty (catalog-only browse) and cat may be
// nil (no catalog / local). Catalog-only models are appended with Available=false.
func MergeDiscovery(apiType APIType, live []ModelInfo, cat CatalogLookup) []DiscoveredModel {
	out := make([]DiscoveredModel, 0, len(live))
	seen := map[string]bool{}

	for _, mi := range live {
		if strings.TrimSpace(mi.ID) == "" {
			continue
		}
		norm := NormalizeModelID(apiType, mi.ID)
		seen[norm] = true
		var catalogCaps *config.ModelCapabilities
		display := mi.ID
		if cat != nil {
			if c, ok := cat.Exact(apiType, norm); ok {
				catalogCaps = &c
			} else if c, ok := cat.Family(apiType, FamilyKey(norm)); ok {
				catalogCaps = &c
			}
		}
		merged := MergeCaps(mi.Caps, catalogCaps)
		out = append(out, DiscoveredModel{
			ID:          mi.ID,
			DisplayName: display,
			APIType:     apiType,
			Available:   true,
			InCatalog:   catalogCaps != nil,
			Caps:        merged,
		})
	}

	if cat != nil {
		for _, ce := range cat.All(apiType) {
			if seen[ce.NormID] {
				continue
			}
			caps := ce.Caps
			if caps.Source == "" {
				caps.Source = SourceCatalog
			}
			out = append(out, DiscoveredModel{
				ID:          ce.ID,
				DisplayName: ce.DisplayName,
				APIType:     apiType,
				Available:   false,
				InCatalog:   true,
				Caps:        caps,
			})
		}
	}
	return out
}
