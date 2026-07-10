package session

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/google/uuid"
)

// NewID returns a short unique sandbox session ID.
func NewID() string {
	id := uuid.NewString()
	return "sbx-" + id[:8]
}

// CanonicalID derives a deterministic, Kubernetes-safe identifier from a raw,
// caller-supplied session reference (the X-Session-Id header). The same raw
// value always maps to the same canonical id, which is what makes the sandbox
// get-or-create by header id work.
//
// Raw ids are untrusted and may contain characters that are illegal in pod /
// PVC names and label values (which must be DNS-1123 labels). We slugify the
// raw value and append a short hash of the ORIGINAL so that distinct raw ids
// can never collide onto the same canonical id after slugification.
func CanonicalID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	suffix := hex.EncodeToString(sum[:])[:10]

	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, raw)
	slug = strings.Trim(slug, "-")
	// Collapse runs of '-' introduced by slugification.
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	// Bound the slug so the full id (slug + '-' + 10-char suffix) stays well
	// within the 63-char DNS-1123 label limit even after runtime prefixes
	// ("sbx-", "pvc-", "ca-").
	const maxSlug = 40
	if len(slug) > maxSlug {
		slug = strings.Trim(slug[:maxSlug], "-")
	}
	if slug == "" {
		return "sbx-" + suffix
	}
	return slug + "-" + suffix
}
