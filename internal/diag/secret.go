package diag

import "log/slog"

// redacted is the placeholder substituted for a non-empty secret in any log
// record or formatted string.
const redacted = "[REDACTED]"

// Secret wraps a sensitive string — an API key, token or password — so it is
// never written to the logs. It implements slog.LogValuer (redacting the value
// in structured records) and fmt.Stringer (redacting it under %s/%v), while the
// real value stays available to non-logging code via a plain string conversion:
//
//	key := diag.Secret(cfg.APIKey)
//	log.Info("auth", "key", key)   // key=[REDACTED]
//	req.Header.Set("Authorization", "Bearer "+string(key)) // real value
//
// An empty Secret renders as the empty string so "unset" is distinguishable from
// "present but hidden" in the logs.
type Secret string

// LogValue redacts the secret when it is logged through slog.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue(s.String())
}

// String redacts the secret when it is formatted with fmt, so an accidental
// %s/%v cannot leak it either.
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return redacted
}
