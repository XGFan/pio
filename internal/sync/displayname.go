package sync

import (
	"fmt"
	"regexp"
	"strconv"
)

const (
	displayLabelMax = 12
	displayLabelFallback = "key"
)

// sanitizeLabel strips an ApiKey label down to characters safe for the
// DisplayName slot — letters, digits, and hyphens — then truncates to 12
// chars. An empty result falls back to "key" so the slot always has a value.
func sanitizeLabel(label string) string {
	out := make([]byte, 0, len(label))
	for _, r := range label {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '-':
			out = append(out, byte(r))
		}
	}
	if len(out) == 0 {
		return displayLabelFallback
	}
	if len(out) > displayLabelMax {
		out = out[:displayLabelMax]
	}
	return string(out)
}

// displayNameRe matches the canonical form: "{label}-{CC}-{NN}".
// Label is sanitizeLabel's output; CC is two uppercase letters; NN is
// two or more digits.
var displayNameRe = regexp.MustCompile(`^([A-Za-z0-9-]{1,12})-([A-Z]{2})-(\d{2,})$`)

// legacyDisplayNameRe matches the pre-migration form "{CC}-{label}-{NN}".
// Used only to detect rows that need to be rewritten to the new layout
// when sync runs — so the country becomes a suffix instead of a prefix.
var legacyDisplayNameRe = regexp.MustCompile(`^([A-Z]{2})-([A-Za-z0-9-]{1,12})-(\d{2,})$`)

// parseDisplayName extracts (countryCode, label, seq) from an auto-form
// DisplayName. Returns ok=false for renamed names, which is the signal to
// preserve them across syncs.
func parseDisplayName(s string) (cc, label string, seq int, ok bool) {
	m := displayNameRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return "", "", 0, false
	}
	return m[2], m[1], n, true
}

// parseLegacyDisplayName returns (cc, label, seq, ok) for the old
// "{CC}-{label}-{NN}" form. Callers use it to migrate stored names to
// the new layout while leaving user-renamed names alone.
func parseLegacyDisplayName(s string) (cc, label string, seq int, ok bool) {
	m := legacyDisplayNameRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return "", "", 0, false
	}
	return m[1], m[2], n, true
}

// formatDisplayName produces the canonical "{label}-{CC}-{NN}" form.
func formatDisplayName(cc, sanitizedLabel string, seq int) string {
	return fmt.Sprintf("%s-%s-%02d", sanitizedLabel, cc, seq)
}

// seqAllocator hands out canonical seq numbers per country code, always
// picking the lowest one not currently in use. Slots vacated by upstreams that
// drop out of a sync (rotated away by Webshare, or swapped out by a replace)
// are reusable, so a same-country replacement reclaims the departing proxy's
// number instead of climbing to the next free one. That keeps the display name
// a client routes by — universal-password routing and the subscription #name
// fragment — stable across a replace.
type seqAllocator struct {
	used map[string]map[int]struct{}
}

func newSeqAllocator() *seqAllocator {
	return &seqAllocator{used: map[string]map[int]struct{}{}}
}

func (a *seqAllocator) ccSet(cc string) map[int]struct{} {
	m := a.used[cc]
	if m == nil {
		m = map[int]struct{}{}
		a.used[cc] = m
	}
	return m
}

// reserve marks seq as occupied for cc so next never hands it out.
func (a *seqAllocator) reserve(cc string, seq int) {
	a.ccSet(cc)[seq] = struct{}{}
}

// next returns the lowest positive seq not yet used for cc and reserves it.
func (a *seqAllocator) next(cc string) int {
	m := a.ccSet(cc)
	for n := 1; ; n++ {
		if _, ok := m[n]; !ok {
			m[n] = struct{}{}
			return n
		}
	}
}
