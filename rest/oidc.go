package rest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OIDCConfig configures JWT bearer authentication against an OpenID
// Connect / OAuth2 identity provider (Keycloak, Auth0, Entra ID, Okta,
// Dex, ...). Tokens are verified with the provider's published JWKS —
// pure stdlib crypto, no external JWT library.
type OIDCConfig struct {
	// Issuer must equal the token's iss claim ("" skips the check).
	Issuer string
	// Audience must appear in the token's aud claim ("" skips the check).
	Audience string
	// JWKSURL is the provider's JWKS endpoint (usually
	// <issuer>/.well-known/jwks.json or /protocol/openid-connect/certs).
	// Keys are fetched lazily and cached for CacheTTL.
	JWKSURL string
	// JWKS is a static JWKS document, used instead of (or as a seed
	// before) JWKSURL. Handy for air-gapped setups and tests.
	JWKS []byte

	// RolesClaim names the claim carrying Fliable roles, e.g. "roles" or
	// "realm_access.roles" (one dot of nesting is supported). Values map
	// through RoleMap when set, otherwise they must already be
	// viewer/operator/admin. Default "roles".
	RolesClaim string
	// RoleMap translates provider role/group names to Fliable roles.
	RoleMap map[string]Role
	// DefaultRole is granted when no roles claim matches ("" = reject
	// tokens that resolve to no role).
	DefaultRole Role
	// TenantClaim names the claim carrying the tenant ID (optional).
	TenantClaim string

	// CacheTTL bounds how long fetched JWKS keys are reused before a
	// refresh (default 5m). An unknown kid always forces a refresh.
	CacheTTL time.Duration
	// HTTPClient overrides the client used to fetch the JWKS.
	HTTPClient *http.Client
	// Clock overrides time.Now (tests).
	Clock func() time.Time
}

// OIDCAuth builds an Authenticator that validates RS256/RS384/RS512 and
// ES256/ES384/ES512 bearer tokens against the configured JWKS. It abstains
// (nil, nil) when the request carries no bearer token, so it chains with
// other authenticators.
func OIDCAuth(cfg OIDCConfig) (Authenticator, error) {
	if cfg.JWKSURL == "" && len(cfg.JWKS) == 0 {
		return nil, errors.New("rest: OIDC needs JWKSURL or a static JWKS")
	}
	if cfg.RolesClaim == "" {
		cfg.RolesClaim = "roles"
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	o := &oidc{cfg: cfg, keys: map[string]crypto.PublicKey{}}
	if len(cfg.JWKS) > 0 {
		if err := o.addJWKS(cfg.JWKS); err != nil {
			return nil, fmt.Errorf("rest: static JWKS: %w", err)
		}
	}
	return o, nil
}

type oidc struct {
	cfg OIDCConfig

	mu      sync.Mutex
	keys    map[string]crypto.PublicKey // kid -> key
	fetched time.Time
}

// Authenticate implements Authenticator.
func (o *oidc) Authenticate(r *http.Request) (*Principal, error) {
	tok := bearer(r)
	if tok == "" {
		return nil, nil
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, nil // not a JWT — let another authenticator try
	}
	headBytes, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, nil
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headBytes, &head); err != nil {
		return nil, nil
	}

	key, err := o.key(head.Kid)
	if err != nil {
		return nil, errAuth("oidc: " + err.Error())
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, errAuth("oidc: malformed signature")
	}
	if err := verifyJWT(head.Alg, key, parts[0]+"."+parts[1], sig); err != nil {
		return nil, errAuth("oidc: " + err.Error())
	}

	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, errAuth("oidc: malformed claims")
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, errAuth("oidc: malformed claims")
	}
	return o.principal(claims)
}

// principal validates the standard claims and maps the rest onto a
// Fliable principal.
func (o *oidc) principal(claims map[string]any) (*Principal, error) {
	now := o.cfg.Clock().Unix()
	if exp, ok := claimInt(claims["exp"]); ok && now > exp {
		return nil, errAuth("oidc: token expired")
	}
	if nbf, ok := claimInt(claims["nbf"]); ok && now < nbf {
		return nil, errAuth("oidc: token not yet valid")
	}
	if o.cfg.Issuer != "" {
		if iss, _ := claims["iss"].(string); iss != o.cfg.Issuer {
			return nil, errAuth("oidc: wrong issuer")
		}
	}
	if o.cfg.Audience != "" && !hasAudience(claims["aud"], o.cfg.Audience) {
		return nil, errAuth("oidc: wrong audience")
	}

	p := &Principal{}
	p.Subject, _ = claims["sub"].(string)
	if o.cfg.TenantClaim != "" {
		p.Tenant, _ = claimPath(claims, o.cfg.TenantClaim).(string)
	}
	for _, raw := range claimStrings(claimPath(claims, o.cfg.RolesClaim)) {
		role := Role(raw)
		if mapped, ok := o.cfg.RoleMap[raw]; ok {
			role = mapped
		}
		if _, known := roleRank[role]; known {
			p.Roles = append(p.Roles, role)
		}
	}
	if len(p.Roles) == 0 {
		if o.cfg.DefaultRole == "" {
			return nil, errAuth("oidc: token grants no role")
		}
		p.Roles = []Role{o.cfg.DefaultRole}
	}
	if exp, ok := claimInt(claims["exp"]); ok {
		p.Expires = exp
	}
	return p, nil
}

// key returns the verification key for kid, refreshing the JWKS when the
// kid is unknown or the cache expired.
func (o *oidc) key(kid string) (crypto.PublicKey, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if k, ok := o.keys[kid]; ok && o.cfg.Clock().Sub(o.fetched) < o.cfg.CacheTTL {
		return k, nil
	}
	if o.cfg.JWKSURL != "" {
		if _, ok := o.keys[kid]; !ok || o.cfg.Clock().Sub(o.fetched) >= o.cfg.CacheTTL {
			if err := o.fetchLocked(); err != nil {
				// A cached key can still serve if the refresh failed.
				if k, ok := o.keys[kid]; ok {
					return k, nil
				}
				return nil, err
			}
		}
	}
	if k, ok := o.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("no key for kid %q", kid)
}

func (o *oidc) fetchLocked() error {
	resp, err := o.cfg.HTTPClient.Get(o.cfg.JWKSURL)
	if err != nil {
		return fmt.Errorf("jwks fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks fetch: HTTP %d", resp.StatusCode)
	}
	doc, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("jwks read: %w", err)
	}
	if err := o.addJWKS(doc); err != nil {
		return err
	}
	o.fetched = o.cfg.Clock()
	return nil
}

// addJWKS parses a JWKS document and installs its keys (caller may hold
// o.mu; the map write is idempotent).
func (o *oidc) addJWKS(doc []byte) error {
	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(doc, &jwks); err != nil {
		return fmt.Errorf("malformed JWKS: %w", err)
	}
	added := 0
	for _, k := range jwks.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		switch k.Kty {
		case "RSA":
			n, err1 := b64.DecodeString(k.N)
			e, err2 := b64.DecodeString(k.E)
			if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 {
				continue
			}
			pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
			o.keys[k.Kid] = pub
			added++
		case "EC":
			x, err1 := b64.DecodeString(k.X)
			y, err2 := b64.DecodeString(k.Y)
			if err1 != nil || err2 != nil {
				continue
			}
			pub := &ecdsa.PublicKey{X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
			switch k.Crv {
			case "P-256":
				pub.Curve = elliptic.P256()
			case "P-384":
				pub.Curve = elliptic.P384()
			case "P-521":
				pub.Curve = elliptic.P521()
			default:
				continue
			}
			o.keys[k.Kid] = pub
			added++
		}
	}
	if added == 0 {
		return errors.New("JWKS contains no usable signing keys")
	}
	return nil
}

// verifyJWT checks sig over signingInput for the given JOSE alg.
func verifyJWT(alg string, key crypto.PublicKey, signingInput string, sig []byte) error {
	var h crypto.Hash
	switch alg {
	case "RS256", "ES256":
		h = crypto.SHA256
	case "RS384", "ES384":
		h = crypto.SHA384
	case "RS512", "ES512":
		h = crypto.SHA512
	default:
		return fmt.Errorf("unsupported alg %q", alg)
	}
	digest := hashSum(h, []byte(signingInput))

	switch alg[0] {
	case 'R':
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("key/alg mismatch")
		}
		if err := rsa.VerifyPKCS1v15(pub, h, digest, sig); err != nil {
			return errors.New("bad signature")
		}
		return nil
	case 'E':
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("key/alg mismatch")
		}
		// JOSE ECDSA signatures are r||s, fixed width per curve.
		if len(sig)%2 != 0 {
			return errors.New("bad signature")
		}
		half := len(sig) / 2
		r := new(big.Int).SetBytes(sig[:half])
		s := new(big.Int).SetBytes(sig[half:])
		if !ecdsa.Verify(pub, digest, r, s) {
			return errors.New("bad signature")
		}
		return nil
	}
	return fmt.Errorf("unsupported alg %q", alg)
}

func hashSum(h crypto.Hash, data []byte) []byte {
	switch h {
	case crypto.SHA256:
		d := sha256.Sum256(data)
		return d[:]
	case crypto.SHA384:
		d := sha512.Sum384(data)
		return d[:]
	default:
		d := sha512.Sum512(data)
		return d[:]
	}
}

// ---- claim helpers -----------------------------------------------------------

// claimPath resolves "a" or "a.b" inside the claims map.
func claimPath(claims map[string]any, path string) any {
	head, rest, nested := strings.Cut(path, ".")
	v := claims[head]
	if !nested {
		return v
	}
	if m, ok := v.(map[string]any); ok {
		return m[rest]
	}
	return nil
}

// claimStrings coerces a claim into a string list: JSON arrays, single
// strings and space-delimited scope strings all work.
func claimStrings(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.Fields(t)
	}
	return nil
}

func claimInt(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

func hasAudience(aud any, want string) bool {
	switch t := aud.(type) {
	case string:
		return t == want
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}
