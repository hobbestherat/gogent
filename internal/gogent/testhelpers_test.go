package gogent

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"gogent/internal/config"
)

// requireModel skips the calling test unless the configured model endpoint
// (GOGENT_MODEL_URL or the default) is reachable. This keeps the offline test
// run green while still exercising the real model when one is available.
func requireModel(t *testing.T) {
	t.Helper()
	url := config.DefaultEndpoint()
	probe := strings.Replace(url, "/chat/completions", "/models", 1)
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(probe)
	if err != nil {
		t.Skipf("model endpoint %s not reachable, skipping integration test: %v", url, err)
		return
	}
	resp.Body.Close()
}
