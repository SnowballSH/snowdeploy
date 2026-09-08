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

// scopePrefix marks the optional trailing field of a hash line that narrows
// what the token may do.
const scopePrefix = "scope="

// action is one mutating operation the API offers. The daemon authorizes the
// action, not merely the identity: a token may be issued for converge alone.
type action string

const (
	actionDeploy   action = "deploy"
	actionRollback action = "rollback"
	actionConverge action = "converge"
)

// scope is the set of actions an identity may take. A nil scope is unscoped
// and allows every action, which is what an unlabelled hash line, an operator
// token predating scopes, and the proxy-authenticated browser all carry.
type scope map[action]bool

func (s scope) allows(a action) bool { return s == nil || s[a] }

// authenticator resolves a request to the actor the journal will record, and
// to what that actor is allowed to do.
type authenticator struct {
	tokenHashFile string
}

// actor returns the authenticated identity and its scope, or false. A bearer
// token is checked against SHA-256 hashes on disk, so no token is ever stored;
// the proxy header is the browser path. Neither present means no identity.
func (a *authenticator) actor(r *http.Request) (string, scope, bool) {
	if token, ok := bearerToken(r); ok {
		return a.matchToken(token)
	}
	if user := strings.TrimSpace(r.Header.Get(remoteUserHeader)); user != "" {
		return user, nil, true
	}
	return "", nil, false
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
func (a *authenticator) matchToken(token string) (string, scope, bool) {
	if a.tokenHashFile == "" {
		return "", nil, false
	}
	data, err := os.ReadFile(a.tokenHashFile) // #nosec G304 -- operator-configured path
	if err != nil {
		return "", nil, false
	}

	sum := sha256.Sum256([]byte(token))
	want := []byte(hex.EncodeToString(sum[:]))

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		hash, rest, _ := strings.Cut(line, " ")
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(hash)), want) == 1 {
			label, allowed := splitScope(rest)
			if label == "" {
				label = cliActorFallback
			}
			return label, allowed, true
		}
	}
	return "", nil, false
}

// splitScope separates a trailing scope field from the label it follows. A
// label may itself contain spaces, so only the last field counts, and only
// when it carries the prefix: everything else is label, and a line without a
// scope field is unscoped.
func splitScope(rest string) (string, scope) {
	rest = strings.TrimSpace(rest)
	cut := strings.LastIndexAny(rest, " \t")
	last := rest[cut+1:]
	if !strings.HasPrefix(last, scopePrefix) {
		return rest, nil
	}
	label := ""
	if cut >= 0 {
		label = strings.TrimSpace(rest[:cut])
	}
	return label, parseScope(strings.TrimPrefix(last, scopePrefix))
}

// parseScope reads the comma-separated action names of a scope field. An
// unrecognised or empty field authorizes nothing, so a typo denies rather
// than widens.
func parseScope(field string) scope {
	allowed := scope{}
	for name := range strings.SplitSeq(field, ",") {
		if name = strings.TrimSpace(name); name != "" {
			allowed[action(name)] = true
		}
	}
	return allowed
}
