package agent

import (
	"gogent/internal/config"
	"gogent/internal/model"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newTestModelConnection builds a model connection equivalent to the old
// zero-argument model.NewModelConnection(): an OpenAI-compatible connection
// pointed at model.DefaultModelURL. Tests typically call conn.SetURL(...) to
// retarget it at an httptest server.
func newTestModelConnection() *model.ModelConnection {
	return model.NewModelConnection(
		&config.ProviderConnection{APIType: "openai", Endpoint: model.DefaultModelURL},
		nil,
	)
}

// requireModel skips the calling test unless the connector's default endpoint is
// reachable. These tests build connections via newTestModelConnection(), which
// targets model.DefaultModelURL, so that is what we probe.
func requireModel(t *testing.T) {
	t.Helper()
	probe := strings.Replace(model.DefaultModelURL, "/chat/completions", "/models", 1)
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(probe)
	if err != nil {
		t.Skipf("model endpoint %s not reachable, skipping integration test: %v", model.DefaultModelURL, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("model endpoint %s returned status %s, skipping integration test", model.DefaultModelURL, resp.Status)
		return
	}
}
