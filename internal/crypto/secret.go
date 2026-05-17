// Package crypto provides AES-256-GCM column encryption, a redaction-safe
// Secret wrapper, and the master-key file abstraction used by the daemon.
package crypto

// Secret is a string-shaped wrapper whose String() and MarshalJSON() both
// return "<redacted>", so accidental inclusion in slog output, formatted
// errors, or JSON responses cannot leak the underlying value.
//
// Callers that genuinely need the cleartext use the Reveal method, which
// makes the leak point explicit and grep-able.
type Secret string

const redacted = "<redacted>"

// String satisfies fmt.Stringer with a fixed redacted value.
func (s Secret) String() string { return redacted }

// GoString matches the %#v formatter — also redacted.
func (s Secret) GoString() string { return redacted }

// MarshalJSON returns the JSON-quoted literal "<redacted>" regardless of
// the underlying value, so encoding a struct that embeds a Secret never
// leaks it.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redacted + `"`), nil
}

// Reveal returns the underlying cleartext. Call sites that use this should
// be the minimum required surface (e.g. the password-reveal REST endpoint).
func (s Secret) Reveal() string { return string(s) }
