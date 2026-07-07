package rest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Role is a coarse authorization level. Endpoints require a minimum role.
type Role string

// Built-in roles, ordered by privilege.
const (
	RoleViewer   Role = "viewer"   // read-only: list/get/history/metrics/events
	RoleOperator Role = "operator" // act: start, complete, message, retry, cancel
	RoleAdmin    Role = "admin"    // manage: deploy, housekeeping, bulk, secrets
)

var roleRank = map[Role]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3}

// Principal is the authenticated caller. Tenant "" grants cross-tenant
// access (a platform admin); a non-empty Tenant confines the caller to
// that tenant, and the server rewrites the request's tenant scope to it.
type Principal struct {
	Subject string   `json:"sub"`
	Tenant  string   `json:"tenant,omitempty"`
	Roles   []Role   `json:"roles"`
	Expires int64    `json:"exp,omitempty"` // unix seconds
	Scopes  []string `json:"scopes,omitempty"`
}

func (p *Principal) has(min Role) bool {
	want := roleRank[min]
	for _, r := range p.Roles {
		if roleRank[r] >= want {
			return true
		}
	}
	return false
}

// Authenticator resolves the principal for a request, or returns an error
// to reject it. Returning (nil, nil) means "no opinion" so the next
// authenticator in a chain may answer.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// AuthFunc adapts a function to Authenticator.
type AuthFunc func(r *http.Request) (*Principal, error)

// Authenticate implements Authenticator.
func (f AuthFunc) Authenticate(r *http.Request) (*Principal, error) { return f(r) }

// ChainAuth tries each authenticator in order; the first non-nil principal
// wins. If all abstain, the request is anonymous.
func ChainAuth(as ...Authenticator) Authenticator {
	return AuthFunc(func(r *http.Request) (*Principal, error) {
		for _, a := range as {
			p, err := a.Authenticate(r)
			if err != nil {
				return nil, err
			}
			if p != nil {
				return p, nil
			}
		}
		return nil, nil
	})
}

// APIKeyAuth authenticates a shared secret via the X-Api-Key header and
// grants the given role (admin by default) across all tenants.
func APIKeyAuth(key string, role Role) Authenticator {
	if role == "" {
		role = RoleAdmin
	}
	return AuthFunc(func(r *http.Request) (*Principal, error) {
		got := r.Header.Get("X-Api-Key")
		if got == "" {
			return nil, nil
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			return nil, errAuth("invalid API key")
		}
		return &Principal{Subject: "api-key", Roles: []Role{role}}, nil
	})
}

// StaticTokenAuth maps opaque bearer tokens to fixed principals — the
// simplest multi-user setup with no signing key.
func StaticTokenAuth(tokens map[string]*Principal) Authenticator {
	return AuthFunc(func(r *http.Request) (*Principal, error) {
		tok := bearer(r)
		if tok == "" {
			return nil, nil
		}
		if p, ok := tokens[tok]; ok {
			return p, nil
		}
		return nil, nil
	})
}

// SignedTokenAuth verifies compact HMAC-SHA256 bearer tokens minted with
// MintToken and the same key. Stateless: no session store, no external
// dependency.
func SignedTokenAuth(signingKey []byte) Authenticator {
	return AuthFunc(func(r *http.Request) (*Principal, error) {
		tok := bearer(r)
		if tok == "" {
			return nil, nil
		}
		p, err := ParseToken(signingKey, tok)
		if err != nil {
			return nil, errAuth(err.Error())
		}
		return p, nil
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// MintToken creates a signed, self-describing bearer token:
// base64url(payload).base64url(hmac). Pure stdlib, no external JWT library.
func MintToken(signingKey []byte, p Principal, ttl time.Duration) (string, error) {
	if ttl != 0 {
		p.Expires = time.Now().Add(ttl).Unix()
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	payload := b64.EncodeToString(body)
	sig := sign(signingKey, payload)
	return payload + "." + sig, nil
}

// ParseToken verifies and decodes a token minted by MintToken.
func ParseToken(signingKey []byte, token string) (*Principal, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed token")
	}
	if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(sign(signingKey, parts[0]))) != 1 {
		return nil, errors.New("bad token signature")
	}
	body, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("malformed token payload")
	}
	var p Principal
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, errors.New("malformed token payload")
	}
	if p.Expires != 0 && time.Now().Unix() > p.Expires {
		return nil, errors.New("token expired")
	}
	return &p, nil
}

var b64 = base64.RawURLEncoding

func sign(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return b64.EncodeToString(mac.Sum(nil))
}

type authError struct{ msg string }

func (e authError) Error() string { return e.msg }
func errAuth(msg string) error    { return authError{msg} }

// principalKey types the request-context slot for the principal.
type principalKey struct{}

func principalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}
