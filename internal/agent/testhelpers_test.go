package agent

import (
	"gogent/internal/model"
	"net/http"
	"strings"
	"testing"
	"time"
)

// requireModel skips the calling test unless the connector's default endpoint is
// reachable. These tests build connections via model.NewModelConnection(), which
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
	resp.Body.Close()
}
