package api

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
)

// remoteUserHeader is set by the fronting proxy after it has authenticated the
// browser session. It is trustworthy only because the API listens on loopback
// and nothing but the proxy can reach it.
const remoteUserHeader = "Remote-User"

// cliActorFallback labels a bearer token whose hash line carries no label.
const cliActorFallback = "cli"

// authenticator resolves a request to the actor the journal will record.
type authenticator struct {
	tokenHashFile string
}

// actor returns the authenticated identity, or false. A bearer token is
// checked against SHA-256 hashes on disk, so no token is ever stored; the
// proxy header is the browser path. Neither present means no identity.
func (a *authenticator) actor(r *http.Request) (string, bool) {
	if token, ok := bearerToken(r); ok {
		return a.matchToken(token)
	}
	if user := strings.TrimSpace(r.Header.Get(remoteUserHeader)); user != "" {
		return user, true
	}
	return "", false
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	return token, token != ""
}

// matchToken compares the token's hash against every hash on file. A missing
// or unreadable file authenticates nobody.
func (a *authenticator) matchToken(token string) (string, bool) {
	if a.tokenHashFile == "" {
		return "", false
	}
	data, err := os.ReadFile(a.tokenHashFile) // #nosec G304 -- operator-configured path
	if err != nil {
		return "", false
	}

	sum := sha256.Sum256([]byte(token))
	want := []byte(hex.EncodeToString(sum[:]))

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		hash, label, _ := strings.Cut(line, " ")
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(hash)), want) == 1 {
			label = strings.TrimSpace(label)
			if label == "" {
				label = cliActorFallback
			}
			return label, true
		}
	}
	return "", false
}
