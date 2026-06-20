package diag

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestSecretRedactedInLog(t *testing.T) {
	var buf bytes.Buffer
	lg := New(&buf)
	key := Secret("sk-supersecret-1234567890")
	lg.Info("authenticating", "api_key", key)

	out := buf.String()
	if strings.Contains(out, "supersecret") {
		t.Errorf("secret leaked into log:\n%s", out)
	}
	if !strings.Contains(out, redacted) {
		t.Errorf("expected %q placeholder, got:\n%s", redacted, out)
	}
}

func TestSecretRedactedUnderFmt(t *testing.T) {
	key := Secret("token-abc")
	if got := fmt.Sprintf("%s", key); got != redacted {
		t.Errorf("Sprintf %%s = %q, want %q", got, redacted)
	}
	if got := fmt.Sprintf("%v", key); got != redacted {
		t.Errorf("Sprintf %%v = %q, want %q", got, redacted)
	}
	// The underlying value is still recoverable for non-logging use.
	if string(key) != "token-abc" {
		t.Errorf("string conversion should expose the real value, got %q", string(key))
	}
}

func TestEmptySecretRendersEmpty(t *testing.T) {
	if got := Secret("").String(); got != "" {
		t.Errorf("empty secret should render empty, got %q", got)
	}
}
