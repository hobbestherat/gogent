package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"gogent/internal/config"
	"gogent/internal/model"
	"gogent/internal/tool"
)

// TestBudgetUsesCostWeightedInput_Issue544 is the end-to-end proof that the agent
// budget charges the cost-weighted prompt (issue #544 gate 1), exercised through the
// REAL call site UserSession.modelRoundTrip -> Agent.AddTokensUsed (not just the
// model-layer CostWeightedInput unit). Two provider-backed connections, same usage,
// different per-(provider,model) multipliers, must yield different discounted budgets
// — proving both the per-provider and the per-model (DeepSeek) paths reach the budget.
//
// Note: a bare NewModelConnection() is NOT provider-less (it wires the OpenAI
// provider, read 0.5), so it is not a clean "raw" counter-example here; the
// provider-less/raw case is pinned in the model package (see cache_cost_issue544_test.go).
func TestBudgetUsesCostWeightedInput_Issue544(t *testing.T) {
	// One final-answer round-trip: 500 of 1000 prompt tokens served from cache.
	cachedFinal := func() map[string]interface{} {
		return map[string]interface{}{
			"choices": []map[string]interface{}{{
				"index":         0,
				"message":       map[string]interface{}{"role": "assistant", "content": "done"},
				"finish_reason": "stop",
			}},
			"usage": map[string]interface{}{
				"prompt_tokens": 1000, "completion_tokens": 20, "total_tokens": 1020,
				"prompt_tokens_details": map[string]interface{}{"cached_tokens": 500},
			},
		}
	}

	// runOne drives one cached-read round-trip through the loop and returns the
	// resulting budget total + number of model calls.
	runOne := func(t *testing.T, apiType, modelName string) (int, int) {
		t.Helper()
		fs := &fakeServer{responses: []map[string]interface{}{cachedFinal()}}
		server := httptest.NewServer(http.HandlerFunc(fs.handler))
		defer server.Close()

		conn := model.NewModelConnectionFromConfig(&config.ModelConfig{
			APIType: apiType, Model: modelName, Endpoint: server.URL,
		})
		sess := model.NewModelSession("test", conn)
		reg := tool.NewToolRegistry()
		reg.RegisterCalcTool()
		ag := NewAgent("root", sess)
		ag.SetToolRegistry(reg)
		us := NewUserSession("s1", ag)

		if _, err := us.ExecuteTaskLoop(context.Background(), "root", "go"); err != nil {
			t.Fatalf("ExecuteTaskLoop: %v", err)
		}
		return ag.GetTokensUsed(), fs.calls
	}

	t.Run("openai gpt-4o charges 0.5 read discount", func(t *testing.T) {
		// (1000-500)*1 + 500*0.5 = 750 prompt + 20 completion = 770. Raw would be 1020.
		got, calls := runOne(t, "openai", "gpt-4o")
		if calls != 1 {
			t.Fatalf("model calls = %d, want exactly 1 round-trip", calls)
		}
		if got != 770 {
			t.Errorf("GetTokensUsed = %d, want cost-weighted 770 (raw would be 1020)", got)
		}
	})

	t.Run("deepseek-chat charges 0.1 read discount via per-model override", func(t *testing.T) {
		// Same api_type openai, but the ModelCaps override makes reads 0.1:
		// (1000-500)*1 + 500*0.1 = 550 prompt + 20 completion = 570.
		got, calls := runOne(t, "openai", "deepseek-chat")
		if calls != 1 {
			t.Fatalf("model calls = %d, want exactly 1 round-trip", calls)
		}
		if got != 570 {
			t.Errorf("GetTokensUsed = %d, want cost-weighted 570 (deepseek 0.1 override)", got)
		}
	})
}
