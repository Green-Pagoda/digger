package github

import "log/slog"

// Token is a GitHub access token wrapped so accidental formatting (fmt "%v"
// / "%+v", json.Marshal, slog structured logging) cannot leak the secret
// value. Call-sites that genuinely need the raw token string must convert
// explicitly with string(t) — the explicit cast is a deliberate tripwire for
// code review, since any such cast is a place that must NOT feed its output
// to a log or error message.
type Token string

// String masks the token when printed via fmt verbs that invoke String (%s,
// %v, %+v when the type implements Stringer).
func (Token) String() string { return "[REDACTED]" }

// GoString masks the token when printed via %#v (fmt's debug verb).
func (Token) GoString() string { return "[REDACTED]" }

// MarshalJSON masks the token when the containing struct is serialized to
// JSON (structured logs, test fixtures, error envelopes).
func (Token) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED]"`), nil
}

// LogValue masks the token in slog output. Handlers resolve LogValuer before
// delegating to other formatting, so this locks the contract regardless of
// handler kind (text, JSON, custom) — relying on Stringer alone depends on
// handler-specific behavior.
func (Token) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }
