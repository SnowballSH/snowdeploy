package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

const (
	// ProxySecretHeader carries the secret shared with the fronting proxy. It
	// is what proves a request came through that proxy: a loopback listener
	// is reachable from every process sharing the host's network, so the
	// connection alone proves nothing.
	ProxySecretHeader = "X-Snowdeploy-Proxy-Secret" // #nosec G101 -- a header name, not a credential

	// MinProxySecretBytes bounds how weak a configured secret may be.
	MinProxySecretBytes = 32
)

// ProxyBoundary admits a Remote-User identity only from the fronting proxy:
// the request must carry the shared secret, and the identity must be on the
// allowlist.
type ProxyBoundary struct {
	allowed      map[string]struct{}
	secretDigest [sha256.Size]byte
}

// NewProxyBoundary builds the boundary, refusing a secret too short to resist
// guessing and an allowlist that could never match an identity exactly.
func NewProxyBoundary(secret []byte, allowed []string) (*ProxyBoundary, error) {
	if len(secret) < MinProxySecretBytes {
		return nil, fmt.Errorf("proxy boundary: the secret is %d bytes, want at least %d",
			len(secret), MinProxySecretBytes)
	}
	if len(allowed) == 0 {
		return nil, errors.New("proxy boundary: the identity allowlist is empty")
	}
	b := &ProxyBoundary{
		allowed:      make(map[string]struct{}, len(allowed)),
		secretDigest: sha256.Sum256(secret),
	}
	for _, identity := range allowed {
		if identity == "" || identity != strings.TrimSpace(identity) || strings.Contains(identity, ",") {
			return nil, fmt.Errorf("proxy boundary: %q is not a usable identity", identity)
		}
		b.allowed[identity] = struct{}{}
	}
	return b, nil
}

// LoadProxySecret reads the shared secret file, trimming only the trailing
// line ending an editor or a secret renderer adds.
func LoadProxySecret(path string) ([]byte, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-configured path
	if err != nil {
		return nil, err
	}
	secret := []byte(strings.TrimRight(string(raw), "\r\n"))
	if len(secret) < MinProxySecretBytes {
		return nil, fmt.Errorf("%s holds a %d-byte secret, want at least %d",
			path, len(secret), MinProxySecretBytes)
	}
	return secret, nil
}

// identity returns the proxy-asserted identity, or false. Both headers must
// appear exactly once: a repeated header means some hop appended instead of
// replacing, and then neither value can be trusted. Comparing digests gives
// the secret comparison a fixed length, so its timing reveals nothing.
func (b *ProxyBoundary) identity(r *http.Request) (string, bool) {
	identities := r.Header.Values(remoteUserHeader)
	secrets := r.Header.Values(ProxySecretHeader)
	if len(identities) == 0 && len(secrets) == 0 {
		return "", false
	}

	refuse := func(reason string) (string, bool) {
		slog.Warn("refused a proxy identity", "reason", reason, "remote", r.RemoteAddr)
		return "", false
	}
	if len(secrets) != 1 {
		return refuse("the proxy secret is not present exactly once")
	}
	presented := sha256.Sum256([]byte(secrets[0]))
	if subtle.ConstantTimeCompare(presented[:], b.secretDigest[:]) != 1 {
		return refuse("the proxy secret does not match")
	}
	if len(identities) != 1 {
		return refuse("the identity is not present exactly once")
	}
	if _, ok := b.allowed[identities[0]]; !ok {
		return refuse("the identity is not on the allowlist")
	}
	return identities[0], true
}
