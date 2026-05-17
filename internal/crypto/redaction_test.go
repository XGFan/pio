package crypto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// TestLogRedaction is the canonical "no plaintext secret in logs" check.
// It exercises every documented path a Secret can take into slog output
// and asserts the cleartext value never appears in the captured buffer.
//
// Plan §7 "Log redaction policy": API keys, upstream passwords, local
// passwords, Proxy-Authorization values, passphrase derivatives — none
// of these may surface in slog or %v / error messages.
func TestLogRedaction(t *testing.T) {
	const cleartext = "TOP-SECRET-DO-NOT-LEAK-12345"
	secret := Secret(cleartext)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)

	// 1. Logged as %v / direct value.
	logger.Info("plain attr", "key", secret)
	// 2. Logged inside an error.
	logger.Error("error path", "err", errors.New("wrapping "+secret.String()))
	// 3. Logged inside a struct field.
	type carrier struct {
		Public string
		Token  Secret
	}
	logger.Info("struct path", "carrier", carrier{Public: "ok", Token: secret})
	// 4. Marshaled to JSON via slog's JSON handler (the production daemon's
	// configuration).
	js, err := json.Marshal(carrier{Public: "ok", Token: secret})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(js, []byte(cleartext)) {
		t.Errorf("json.Marshal leaked cleartext: %s", js)
	}

	// The captured slog output MUST NOT contain the cleartext.
	out := buf.String()
	if strings.Contains(out, cleartext) {
		t.Fatalf("cleartext leaked through slog: %s", out)
	}

	// Defensive: the same applies to fmt.Sprintf calls inside slog message bodies.
	logger.Info("fmt path: " + secret.String())
	if strings.Contains(buf.String(), cleartext) {
		t.Fatalf("cleartext leaked through fmt-style logging: %s", buf.String())
	}

	// Sanity: at least one "<redacted>" must appear in the output to prove
	// we exercised the Secret's String() / MarshalJSON paths.
	if !strings.Contains(buf.String(), redacted) {
		t.Errorf("captured output never contained %q (test may not have exercised Secret)", redacted)
	}
}

// TestLogRedactionContext verifies redaction also works for context-scoped
// loggers — the v4.1 daemon uses logger.With(...) at startup so we want
// to confirm pre-bound attributes don't bypass the wrapper.
func TestLogRedactionContext(t *testing.T) {
	const cleartext = "another-secret-payload"
	secret := Secret(cleartext)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil)).With("session_token", secret)
	logger.LogAttrs(context.Background(), slog.LevelInfo, "test")
	if strings.Contains(buf.String(), cleartext) {
		t.Fatalf("cleartext leaked through .With() pre-binding: %s", buf.String())
	}
}
