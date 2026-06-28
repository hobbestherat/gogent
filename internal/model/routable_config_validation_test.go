package model

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gogent/internal/config"
)

// Tests for the issue #505 fix: a ModelConfig that cannot be routed must fail fast
// on first use with a clear, model-named, field-naming error surfaced via
// conn.configErr (the Vertex validateConfig precedent), instead of silently
// targeting the OpenAI provider's http://localhost:8080 placeholder and failing
// with an opaque "unexpected error: status 404".
//
// These tests live in package `model` so they can inspect the private configErr
// field directly and exercise the public completion / listing paths without any
// network (configErr short-circuits before any HTTP dial).

// configErrOf builds a connection from cfg and returns its deferred config error
// (nil when the config is valid). This mirrors how the Vertex deferred error is
// observed: validation runs in the constructor and is exposed via configErr.
func configErrOf(t *testing.T, cfg *config.ModelConfig) error {
	t.Helper()
	conn := NewModelConnectionFromConfig(cfg)
	if conn.configErr == nil {
		return nil
	}
	return conn.configErr
}

// requireMisconfig fails the test if err is nil, and returns err as a *ModelError.
func requireMisconfig(t *testing.T, err error) *ModelError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a misconfiguration error, got nil")
	}
	var me *ModelError
	if !errors.As(err, &me) {
		t.Fatalf("misconfiguration error must be a *ModelError, got %T: %v", err, err)
	}
	return me
}

// assertNoLocalhost404 guards the central invariant of the fix: the error must
// be the clear "is misconfigured" message, never the opaque generic 404 that the
// silent localhost fallback used to produce.
func assertNoLocalhost404(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "status 404") || strings.Contains(msg, "unexpected error: status") {
		t.Errorf("error must not be an opaque localhost 404, got: %q", msg)
	}
	if !strings.Contains(msg, "is misconfigured") {
		t.Errorf("error must explain the misconfiguration, got: %q", msg)
	}
}

// ---------------------------------------------------------------------------
// Criterion 1 + 2: unroutable configs are rejected with a clear, model-named,
// field-naming error (configErr set).
// ---------------------------------------------------------------------------

// TestRoutableValidation_EmptyAPITypeAndEndpoint_Rejected is the issue's
// headline case: an entry with neither api_type nor endpoint must not silently
// fall back to localhost:8080.
func TestRoutableValidation_EmptyAPITypeAndEndpoint_Rejected(t *testing.T) {
	cfg := &config.ModelConfig{
		Name:  "openrouter-glm-5.2",
		Model: "glm-5.2", // model is set; the problem is routing, not the model id
	}
	me := requireMisconfig(t, configErrOf(t, cfg))

	want := `model "openrouter-glm-5.2" is misconfigured: api_type and endpoint are both empty (cannot determine where to send requests)`
	if me.Message != want {
		t.Errorf("message = %q\nwant %q", me.Message, want)
	}
	if me.Type != ErrorGeneric {
		t.Errorf("Type = %v, want ErrorGeneric", me.Type)
	}
	if me.HTTPStatusCode != 0 {
		t.Errorf("HTTPStatusCode = %d, want 0 (a config error is not an HTTP status)", me.HTTPStatusCode)
	}
	assertNoLocalhost404(t, me)
}

// TestRoutableValidation_RoutabilityPrecedesModelEmpty: when api_type and
// endpoint are both empty AND the model is also empty, the routability failure
// (clearest, most actionable) wins, not the model-empty message.
func TestRoutableValidation_RoutabilityPrecedesModelEmpty(t *testing.T) {
	cfg := &config.ModelConfig{Name: "blank"} // api_type, endpoint, model all empty
	me := requireMisconfig(t, configErrOf(t, cfg))
	if !strings.Contains(me.Message, "api_type and endpoint are both empty") {
		t.Errorf("routability error should take precedence over model-empty; got: %q", me.Message)
	}
	if strings.Contains(me.Message, "model is empty") {
		t.Errorf("model-empty message must not win over routability; got: %q", me.Message)
	}
}

// TestRoutableValidation_UnrecognizedAPIType_Rejected: an unrecognized api_type
// token silently maps to the OpenAI/localhost placeholder today; the fix
// surfaces it as a misconfiguration that echoes the user's raw (typo'd) token.
func TestRoutableValidation_UnrecognizedAPIType_Rejected(t *testing.T) {
	cfg := &config.ModelConfig{Name: "typo", APIType: "opnai", Model: "x"}
	me := requireMisconfig(t, configErrOf(t, cfg))
	if !strings.Contains(me.Message, `model "typo"`) {
		t.Errorf("message must name the model; got: %q", me.Message)
	}
	if !strings.Contains(me.Message, `api_type "opnai" is unrecognized`) {
		t.Errorf("message must echo the raw unrecognized api_type token; got: %q", me.Message)
	}
	assertNoLocalhost404(t, me)
}

// TestRoutableValidation_NamesModelViaDisplayNameAndUnnamed: the error always
// names something — DisplayName when Name is absent, "<unnamed>" when both are.
func TestRoutableValidation_NamesModelViaDisplayNameAndUnnamed(t *testing.T) {
	t.Run("display_name", func(t *testing.T) {
		cfg := &config.ModelConfig{DisplayName: "My Local GPT", Model: "x"}
		me := requireMisconfig(t, configErrOf(t, cfg))
		if !strings.Contains(me.Message, `model "My Local GPT"`) {
			t.Errorf("message must use DisplayName when Name is empty; got: %q", me.Message)
		}
	})
	t.Run("unnamed", func(t *testing.T) {
		cfg := &config.ModelConfig{Model: "x"} // no Name, no DisplayName
		me := requireMisconfig(t, configErrOf(t, cfg))
		if !strings.Contains(me.Message, `model "<unnamed>"`) {
			t.Errorf("message must fall back to <unnamed>; got: %q", me.Message)
		}
	})
}

// TestRoutableValidation_HostedGatewayEmptyModel_Rejected: an empty model on a
// known hosted gateway (openrouter/zai) is almost certainly wrong.
func TestRoutableValidation_HostedGatewayEmptyModel_Rejected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		apiType string
	}{
		{"openrouter", "openrouter"},
		{"zai", "zai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// endpoint empty is fine — these derive their base — but the model is empty.
			cfg := &config.ModelConfig{Name: "gw", APIType: tc.apiType, Model: ""}
			me := requireMisconfig(t, configErrOf(t, cfg))
			want := `model "gw" is misconfigured: model is empty (api_type "` + tc.apiType + `" requires a model name)`
			// The deriving providers never trip routability, so the model-empty
			// check is what fires; confirm it did not instead report routing.
			if !strings.Contains(me.Message, "model is empty") {
				t.Errorf("expected the model-empty message; got: %q", me.Message)
			}
			if me.Message != want {
				t.Errorf("message = %q\nwant %q", me.Message, want)
			}
			if strings.Contains(me.Message, "api_type and endpoint are both empty") {
				t.Errorf("deriving gateway must not trip the routability check; got: %q", me.Message)
			}
		})
	}
}

// TestRoutableValidation_WhitespaceAndCaseNormalization locks down the input
// normalization the check depends on: endpoint is trimmed before the emptiness
// test (whitespace-only == empty), and the api_type comparison / resolution is
// case- and whitespace-insensitive. A regression here would either over-reject
// ("OpenAI" not matching the openai exception) or under-reject (whitespace
// endpoint slipping through to localhost).
func TestRoutableValidation_WhitespaceAndCaseNormalization(t *testing.T) {
	t.Run("whitespace-only endpoint counts as empty (rejected)", func(t *testing.T) {
		// empty api_type + a whitespace-only endpoint must still be rejected.
		cfg := &config.ModelConfig{Name: "ws", Endpoint: "   \t  ", Model: "x"}
		me := requireMisconfig(t, configErrOf(t, cfg))
		if !strings.Contains(me.Message, "api_type and endpoint are both empty") {
			t.Errorf("whitespace endpoint must be treated as empty; got: %q", me.Message)
		}
	})
	t.Run("mixed-case OpenAI still accepted", func(t *testing.T) {
		cfg := &config.ModelConfig{Name: "c", APIType: "OpenAI", Model: "m"} // empty endpoint
		if err := configErrOf(t, cfg); err != nil {
			t.Errorf("mixed-case OpenAI must be accepted via the openai exception; got: %v", err)
		}
	})
	t.Run("whitespace-padded openrouter derives base (accepted)", func(t *testing.T) {
		cfg := &config.ModelConfig{Name: "p", APIType: "  openrouter  ", Model: "google/gemma-3-27b-it:free"}
		if err := configErrOf(t, cfg); err != nil {
			t.Errorf("whitespace-padded openrouter must resolve to a deriving provider; got: %v", err)
		}
	})
}

// TestRoutableValidation_AnthropicEmptyModelAcceptedDocumentsScope: the
// hosted-gateway empty-model check is deliberately scoped to {openrouter, zai}
// only (design Open Q 2). Anthropic with an empty model is therefore accepted at
// config-validation time and left to surface a real API error instead. This test
// documents that conservative scope so it is not narrowed/expanded by accident.
func TestRoutableValidation_AnthropicEmptyModelAcceptedDocumentsScope(t *testing.T) {
	cfg := &config.ModelConfig{Name: "a", APIType: "anthropic", Model: ""}
	if err := configErrOf(t, cfg); err != nil {
		t.Errorf("anthropic empty-model must NOT be rejected by the scoped gateway check; got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 3: valid configs are unaffected (configErr stays nil).
// ---------------------------------------------------------------------------

// TestRoutableValidation_ValidConfigs_Accepted exercises every configuration
// the fix must NOT reject.
func TestRoutableValidation_ValidConfigs_Accepted(t *testing.T) {
	vertexReady := func(apiType string) *config.ModelConfig {
		// The OpenAI-compat shim ("vertex") addresses Gemini as "google/<model>";
		// the native and anthropic Vertex routes name the model bare (issue #574).
		// Each fixture uses the shape its route expects so it stays a *valid* config
		// under the #574 publisher-prefix rules.
		model := "gemini-2.5-flash"
		if StringToAPIType(apiType) == APITypeVertex {
			model = "google/gemini-2.5-flash"
		}
		return &config.ModelConfig{
			Name: "v", APIType: apiType, Model: model,
			Project: "my-proj", Location: "us-central1",
		}
	}
	for _, tc := range []struct {
		name string
		cfg  *config.ModelConfig
	}{
		// The maintainer-flagged legitimate edges:
		{"empty api_type with explicit endpoint", &config.ModelConfig{Name: "e", Endpoint: "https://api.example.com/v1", Model: "m"}},
		{"explicit openai empty endpoint (documented local default)", &config.ModelConfig{Name: "o", APIType: "openai", Model: "m"}},
		{"explicit openai empty endpoint empty model (local auto-select)", &config.ModelConfig{Name: "o", APIType: "openai", Model: ""}},
		{"explicit openai endpoint empty model", &config.ModelConfig{Name: "o", APIType: "openai", Endpoint: "http://127.0.0.1:8080/v1", Model: ""}},
		// Every base-URL-deriving provider with an empty endpoint (model set):
		{"zai empty endpoint", &config.ModelConfig{Name: "z", APIType: "zai", Model: "glm-4.6"}},
		{"openrouter empty endpoint", &config.ModelConfig{Name: "r", APIType: "openrouter", Model: "google/gemma-3-27b-it:free"}},
		{"anthropic empty endpoint", &config.ModelConfig{Name: "a", APIType: "anthropic", Model: "claude-opus-4-8"}},
		{"vertex empty endpoint", vertexReady("vertex")},
		{"vertex-native empty endpoint", vertexReady("vertex-native")},
		{"vertex-anthropic empty endpoint", vertexReady("vertex-anthropic")},
		// An unrecognized api_type WITH an explicit endpoint is routable (maps to
		// the generic OpenAI adapter) and must stay accepted.
		{"unrecognized api_type with explicit endpoint", &config.ModelConfig{Name: "u", APIType: "acme-llama", Endpoint: "https://api.acme.test/v1", Model: "m"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := configErrOf(t, tc.cfg); err != nil {
				t.Errorf("valid config must not be rejected, got: %v", err)
			}
		})
	}
}

// TestRoutableValidation_VertexMissingProjectLocation_StillUsesVertexValidate:
// a deriving provider (vertex) with an empty endpoint must NOT trip the
// routability check; Vertex's own validateConfig still demands project/location.
// The model is publisher-qualified so the #574 publisher-prefix rule passes and
// this test keeps isolating the project/location precedence it was written for.
func TestRoutableValidation_VertexMissingProjectLocation_StillUsesVertexValidate(t *testing.T) {
	cfg := &config.ModelConfig{Name: "v", APIType: "vertex", Model: "google/gemini-2.5-flash"}
	me := requireMisconfig(t, configErrOf(t, cfg))
	if !strings.Contains(me.Message, "project and location are required") {
		t.Errorf("expected Vertex's own project/location error, got: %q", me.Message)
	}
	if strings.Contains(me.Message, "publisher-qualified") {
		t.Errorf("publisher-prefix rule must not fire for a qualified model; got: %q", me.Message)
	}
}

// ---------------------------------------------------------------------------
// Criterion 2 / acceptance: the error surfaces on first use through the normal
// completion + listing paths, with no network (no localhost dial / 404).
// ---------------------------------------------------------------------------

func newRejectedConn(t *testing.T) *ModelConnection {
	t.Helper()
	return NewModelConnectionFromConfig(&config.ModelConfig{Name: "bad-entry", Model: "x"})
}

// TestRoutableValidation_CompleteSurfacesConfigErr_FirstUse_NoNetwork: the
// blocking completion returns the configErr immediately, before any HTTP dial.
func TestRoutableValidation_CompleteSurfacesConfigErr_FirstUse_NoNetwork(t *testing.T) {
	conn := newRejectedConn(t)
	want := conn.configErr

	start := time.Now()
	resp, err := conn.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Complete must return the configErr on first use, got nil")
	}
	if resp != nil {
		t.Errorf("resp must be nil on a config error, got %+v", resp)
	}
	// Same deferred error, surfaced verbatim — and it returned essentially
	// instantly, proving no localhost dial occurred.
	if err.Error() != want.Error() {
		t.Errorf("Complete returned %q, want the configErr %q", err.Error(), want.Error())
	}
	if elapsed > 2*time.Second {
		t.Errorf("Complete should short-circuit without dialing; took %v", elapsed)
	}
	assertNoLocalhost404(t, err)
}

// TestRoutableValidation_CompleteWithToolsSurfacesConfigErr mirrors the agent
// loop's actual entry point (complete with tools).
func TestRoutableValidation_CompleteWithToolsSurfacesConfigErr(t *testing.T) {
	conn := newRejectedConn(t)
	_, err := conn.CompleteWithTools(
		[]Message{{Role: RoleUser, Content: "hi"}},
		[]ToolDef{{Type: "function", Function: FunctionDef{Name: "read"}}},
	)
	if err == nil || err.Error() != conn.configErr.Error() {
		t.Fatalf("CompleteWithTools must surface configErr; got %v", err)
	}
	assertNoLocalhost404(t, err)
}

// TestRoutableValidation_CompleteStreamSurfacesConfigErr: the streaming path
// (completeStream) also short-circuits on configErr.
func TestRoutableValidation_CompleteStreamSurfacesConfigErr(t *testing.T) {
	conn := newRejectedConn(t)
	streamCh, errCh := conn.CompleteStream([]Message{{Role: RoleUser, Content: "hi"}})

	var got error
errChLoop:
	for {
		select {
		case err, ok := <-errCh:
			if !ok {
				break errChLoop
			}
			got = err
		case <-time.After(2 * time.Second):
			t.Fatal("CompleteStream did not surface the configErr within 2s (likely dialing localhost)")
		}
	}
	// Drain the stream channel so the goroutine can finish cleanly.
	for range streamCh {
	}
	if got == nil {
		t.Fatal("CompleteStream must deliver the configErr on the error channel")
	}
	if got.Error() != conn.configErr.Error() {
		t.Errorf("CompleteStream delivered %q, want configErr %q", got.Error(), conn.configErr.Error())
	}
	assertNoLocalhost404(t, got)
}

// TestRoutableValidation_ListModelsSurfacesConfigErr_NoNetwork: the Scan/listing
// path (doJSON) must also fail fast with the configErr rather than dialing the
// localhost placeholder. Covers the doJSON configErr gate.
func TestRoutableValidation_ListModelsSurfacesConfigErr_NoNetwork(t *testing.T) {
	conn := newRejectedConn(t)

	start := time.Now()
	_, err := conn.ListModels()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ListModels must return the configErr on a misconfigured entry, got nil")
	}
	if err.Error() != conn.configErr.Error() {
		t.Errorf("ListModels returned %q, want configErr %q", err.Error(), conn.configErr.Error())
	}
	if elapsed > 2*time.Second {
		t.Errorf("ListModels should short-circuit via the doJSON gate without dialing; took %v", elapsed)
	}
	assertNoLocalhost404(t, err)
}

// ---------------------------------------------------------------------------
// Criterion 3: the fix does not change 404 retryability (secondary analyzeError
// polish is message-only) and the registry is the single source of truth.
// ---------------------------------------------------------------------------

// TestAnalyzeError_404_DescriptiveButStillNonRetryable: the dedicated 404 case
// carries a clearer message and stays non-retryable, with retryability of other
// statuses unchanged.
func TestAnalyzeError_404_DescriptiveButStillNonRetryable(t *testing.T) {
	conn := NewModelConnection() // has Stats initialized
	me := conn.analyzeError(404, "not found body")

	if me.Type != ErrorGeneric {
		t.Errorf("Type = %v, want ErrorGeneric", me.Type)
	}
	if me.HTTPStatusCode != 404 {
		t.Errorf("HTTPStatusCode = %d, want 404", me.HTTPStatusCode)
	}
	if !strings.Contains(me.Message, "404") || !strings.Contains(me.Message, "endpoint") {
		t.Errorf("404 message should be descriptive (mention 404 + endpoint); got: %q", me.Message)
	}
	// 404 stays non-retryable (the fix must not change this).
	if isRetryableStatus(404) {
		t.Error("404 must remain non-retryable")
	}
	// Regression guard: the retryable set is unchanged.
	for _, code := range []int{408, 409, 429, 500, 503, 504} {
		if !isRetryableStatus(code) {
			t.Errorf("status %d should still be retryable", code)
		}
	}
	for _, code := range []int{400, 401, 403, 404, 422} {
		if isRetryableStatus(code) {
			t.Errorf("status %d should still be non-retryable", code)
		}
	}
	// The 404 case still counts as a generic error.
	if conn.Stats.GenericErrorCount == 0 {
		t.Error("404 should increment GenericErrorCount")
	}
}

// TestDerivesBase_RegistryIsSourceOfTruth locks the provider registry as the
// authority for "this api_type synthesizes its own base URL". A future provider
// author who forgets the flag would let an empty-endpoint entry silently fall
// back to localhost — this test catches that drift.
func TestDerivesBase_RegistryIsSourceOfTruth(t *testing.T) {
	for _, tc := range []struct {
		apiType     APIType
		wantDerives bool
	}{
		{APITypeOpenAI, false}, // the only placeholder-default provider
		{APITypeZAI, true},
		{APITypeOpenRouter, true},
		{APITypeAnthropic, true},
		{APITypeVertex, true},
		{APITypeVertexNative, true},
		{APITypeVertexAnthropic, true},
	} {
		got := providerFor(tc.apiType).derivesBase
		if got != tc.wantDerives {
			t.Errorf("derivesBase(%q) = %v, want %v", tc.apiType, got, tc.wantDerives)
		}
	}
}

// ---------------------------------------------------------------------------
// Criterion 3 regression guards: construction-only helpers the existing tests
// rely on still work even when configErr is set; the bare library default is
// untouched.
// ---------------------------------------------------------------------------

// TestRoutableValidation_BuildRequestStillWorksOnRejectedConfig: configErr gates
// network operations (complete/completeStream/doJSON) but NOT request building.
// Existing tests build requests from empty configs; that must keep working.
func TestRoutableValidation_BuildRequestStillWorksOnRejectedConfig(t *testing.T) {
	conn := newRejectedConn(t)
	if conn.configErr == nil {
		t.Fatal("precondition: config must be rejected")
	}
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil, nil)
	if len(req.Messages) != 1 {
		t.Fatalf("buildRequest must still work on a rejected config (configErr gates network, not building); got %d messages", len(req.Messages))
	}
}

// TestRoutableValidation_BareNewModelConnectionUntouched: the standalone library
// default (no config) is never validated and stays usable as-is.
func TestRoutableValidation_BareNewModelConnectionUntouched(t *testing.T) {
	conn := NewModelConnection()
	if conn.configErr != nil {
		t.Errorf("bare NewModelConnection() must not set a configErr, got: %v", conn.configErr)
	}
	if conn.URL != DefaultModelURL {
		t.Errorf("bare default URL changed: %q (want %q)", conn.URL, DefaultModelURL)
	}
}

// TestRoutableValidation_NilConfigSafe: validateRoutableConfig (the function the
// fix adds) has a nil guard and returns nil rather than panicking. (The
// constructor itself dereferences the config upstream of this function, so
// nil-config nil-safety of the constructor is pre-existing and out of scope.)
func TestRoutableValidation_NilConfigSafe(t *testing.T) {
	if err := validateRoutableConfig(nil); err != nil {
		t.Errorf("validateRoutableConfig(nil) = %v, want nil", err)
	}
}
