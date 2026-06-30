package modelsdev

import (
	"reflect"
	"strings"
	"testing"

	"gogent/internal/model"
)

// TestReasoningCapable pins the review form's reasoning-capable indicator (issue
// #542 aspect F + note 2). It must honor Model.Reasoning directly, so a model that
// is reasoning:true with only a toggle (or no reasoning option at all) is still
// flagged — the picker badge infers reasoning only from an effort option.
func TestReasoningCapable(t *testing.T) {
	cases := []struct {
		name string
		m    Model
		want bool
	}{
		{"reasoning flag alone", Model{Reasoning: true}, true},
		{"reasoning with only a toggle (note 2)", Model{Reasoning: true, ReasoningOptions: []ReasoningOption{{Type: "toggle"}}}, true},
		{"reasoning with no options (note 2)", Model{Reasoning: true}, true},
		{"no flag but has effort option", Model{ReasoningOptions: []ReasoningOption{{Type: "effort", Values: []string{"low"}}}}, true},
		{"no flag but has toggle", Model{ReasoningOptions: []ReasoningOption{{Type: "toggle"}}}, true},
		{"bare model", Model{}, false},
		{"only an unrelated reasoning option type", Model{ReasoningOptions: []ReasoningOption{{Type: "weather", Values: []string{"sunny"}}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReasoningCapable(tc.m); got != tc.want {
				t.Errorf("ReasoningCapable = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCapabilityLabels covers the display-only capability row (aspect I): stable
// order, only the flags the model has, and the bare-model empty case.
func TestCapabilityLabels(t *testing.T) {
	t.Run("full model in stable order", func(t *testing.T) {
		m := Model{Reasoning: true, ToolCall: true, Attachment: true, StructuredOutput: true, Temperature: true, OpenWeights: true}
		want := []string{"reasoning", "tool calling", "vision", "structured output", "custom temperature", "open weights"}
		if got := CapabilityLabels(m); !reflect.DeepEqual(got, want) {
			t.Errorf("CapabilityLabels = %v, want %v", got, want)
		}
	})
	t.Run("structured output and open weights appear only when flagged", func(t *testing.T) {
		// Neither flag set: neither label present, even on an otherwise-capable model.
		bare := Model{ToolCall: true}
		if got := CapabilityLabels(bare); reflect.DeepEqual(got, []string{"tool calling"}) != true {
			t.Errorf("CapabilityLabels(no structured/open) = %v, want [tool calling] only", got)
		}
		// structured_output alone surfaces just that label.
		so := CapabilityLabels(Model{StructuredOutput: true})
		if !reflect.DeepEqual(so, []string{"structured output"}) {
			t.Errorf("CapabilityLabels(structured only) = %v, want [structured output]", so)
		}
		// open_weights alone surfaces just that label.
		ow := CapabilityLabels(Model{OpenWeights: true})
		if !reflect.DeepEqual(ow, []string{"open weights"}) {
			t.Errorf("CapabilityLabels(open weights only) = %v, want [open weights]", ow)
		}
	})
	t.Run("reasoning inferred from toggle only", func(t *testing.T) {
		// Reasoning flag false but a toggle present: ReasoningCapable is true, so
		// "reasoning" must still appear.
		m := Model{ReasoningOptions: []ReasoningOption{{Type: "toggle"}}}
		got := CapabilityLabels(m)
		if len(got) == 0 || got[0] != "reasoning" {
			t.Errorf("CapabilityLabels = %v, want leading \"reasoning\" (ReasoningCapable via toggle)", got)
		}
	})
	t.Run("partial: tool + vision, no reasoning/temp", func(t *testing.T) {
		m := Model{ToolCall: true, Attachment: true}
		want := []string{"tool calling", "vision"}
		if got := CapabilityLabels(m); !reflect.DeepEqual(got, want) {
			t.Errorf("CapabilityLabels = %v, want %v", got, want)
		}
	})
	t.Run("bare model has no capabilities", func(t *testing.T) {
		if got := CapabilityLabels(Model{}); len(got) != 0 {
			t.Errorf("CapabilityLabels(bare) = %v, want empty", got)
		}
	})
}

// TestThinkingBudgetMin covers the informational thinking-budget hint: the parsed
// reasoning_options[type=budget_tokens].Min is returned when present (so it is no
// longer silently dropped), 0 otherwise. It is display-only — gogent has no
// thinking-budget input — so this just feeds the review-form hint.
func TestThinkingBudgetMin(t *testing.T) {
	cases := []struct {
		name string
		m    Model
		want int
	}{
		{"budget_tokens with min", Model{ReasoningOptions: []ReasoningOption{{Type: "budget_tokens", Min: 1024}}}, 1024},
		{"budget_tokens type is case-insensitive", Model{ReasoningOptions: []ReasoningOption{{Type: "Budget_Tokens", Min: 2048}}}, 2048},
		{"no budget option", Model{ReasoningOptions: []ReasoningOption{{Type: "effort", Values: []string{"low"}}}}, 0},
		{"toggle only, no min", Model{ReasoningOptions: []ReasoningOption{{Type: "toggle"}}}, 0},
		{"bare model", Model{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ThinkingBudgetMin(tc.m); got != tc.want {
				t.Errorf("ThinkingBudgetMin = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCostSummary covers the Cost row (aspect D): free detection, integer pricing,
// and — critically for an issue about clarity — sub-dollar pricing that the majority
// of real catalog models use (Haiku, Gemini Flash, GPT-4o-mini). The formatter must
// not round those to "$0".
func TestCostSummary(t *testing.T) {
	cases := []struct {
		name string
		cost Cost
		want string
	}{
		{"both zero is free", Cost{Input: 0, Output: 0}, "Free"},
		{"integer pricing", Cost{Input: 5, Output: 25}, "$5 in / $25 out per M"},
		{"sub-dollar input must not round to zero", Cost{Input: 0.15, Output: 0.6}, "$0.15 in / $0.6 out per M"},
		{"sub-dollar Gemini-Flash-style", Cost{Input: 0.075, Output: 0.3}, "$0.075 in / $0.3 out per M"},
		{"half-dollar keeps the .5", Cost{Input: 2.5, Output: 10}, "$2.5 in / $10 out per M"},
		{"input zero, output nonzero is NOT free", Cost{Input: 0, Output: 5}, "$0 in / $5 out per M"},
		{"input nonzero, output zero is NOT free", Cost{Input: 3, Output: 0}, "$3 in / $0 out per M"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Model{Cost: tc.cost}
			if got := CostSummary(m); got != tc.want {
				t.Errorf("CostSummary(%+v) = %q, want %q", tc.cost, got, tc.want)
			}
			// "Free" must only be returned when both sides are truly zero; any priced
			// summary must carry a "$" so the row can never read as free by accident.
			if tc.cost.Input != 0 || tc.cost.Output != 0 {
				if !strings.Contains(tc.want, "$") {
					t.Fatalf("test bug: priced case %q has no $ in want", tc.name)
				}
			}
		})
	}
}

// TestDerivesBaseAgreesWithDeriveBaseAPITypes locks the cross-package invariant the
// fixes-round addressed: modelsdev.deriveBaseAPITypes (the set ToModelConfig blanks
// Endpoint for) and model.DerivesBase (the set the review form branches on) must be
// the SAME set. A drift would make the form show a "derived:" indicator for a
// provider whose persisted Endpoint is p.API, or an editable p.API box for one whose
// Endpoint is blank — i.e. the indicator would lie about the persisted config.
func TestDerivesBaseAgreesWithDeriveBaseAPITypes(t *testing.T) {
	for apiType := range deriveBaseAPITypes {
		if !model.DerivesBase(model.APIType(apiType)) {
			t.Errorf("deriveBaseAPITypes has %q (ToModelConfig blanks its Endpoint) but model.DerivesBase is false — the review form would prefill p.API for a provider whose persisted endpoint is empty", apiType)
		}
	}
	// The generic gateway type is the canonical NON-derive-base: ToModelConfig copies
	// p.API, so the form must show the editable prefilled box, not a derived indicator.
	if model.DerivesBase(model.APITypeOpenAI) {
		t.Error("model.DerivesBase(openai) = true, want false — gateways must keep the p.API editable-box path")
	}
}
