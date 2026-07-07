package rest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

// mintRS256 builds a real RS256 JWT for tests.
func mintRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	head, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
	body, _ := json.Marshal(claims)
	signingInput := b64.EncodeToString(head) + "." + b64.EncodeToString(body)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + b64.EncodeToString(sig)
}

func mintES256(t *testing.T, key *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	head, _ := json.Marshal(map[string]any{"alg": "ES256", "typ": "JWT", "kid": kid})
	body, _ := json.Marshal(claims)
	signingInput := b64.EncodeToString(head) + "." + b64.EncodeToString(body)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + b64.EncodeToString(sig)
}

func rsaJWK(kid string, pub *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": b64.EncodeToString(pub.N.Bytes()),
		"e": b64.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func ecJWK(kid string, pub *ecdsa.PublicKey) map[string]any {
	x := make([]byte, 32)
	y := make([]byte, 32)
	pub.X.FillBytes(x)
	pub.Y.FillBytes(y)
	return map[string]any{
		"kty": "EC", "kid": kid, "use": "sig", "crv": "P-256",
		"x": b64.EncodeToString(x), "y": b64.EncodeToString(y),
	}
}

func TestOIDCVerifiesRS256AndES256(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwks, _ := json.Marshal(map[string]any{"keys": []any{
		rsaJWK("k-rsa", &rsaKey.PublicKey), ecJWK("k-ec", &ecKey.PublicKey),
	}})

	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		w.Write(jwks)
	}))
	defer srv.Close()

	auth, err := OIDCAuth(OIDCConfig{
		Issuer:      "https://idp.example",
		Audience:    "fliable",
		JWKSURL:     srv.URL,
		RolesClaim:  "realm_access.roles",
		RoleMap:     map[string]Role{"bpm-admin": RoleAdmin},
		TenantClaim: "tenant",
	})
	if err != nil {
		t.Fatal(err)
	}

	exp := float64(time.Now().Add(time.Hour).Unix())
	claims := map[string]any{
		"iss": "https://idp.example", "aud": "fliable", "sub": "alice", "exp": exp,
		"tenant":       "acme",
		"realm_access": map[string]any{"roles": []any{"bpm-admin", "unrelated"}},
	}

	for name, tok := range map[string]string{
		"rs256": mintRS256(t, rsaKey, "k-rsa", claims),
		"es256": mintES256(t, ecKey, "k-ec", claims),
	} {
		req := httptest.NewRequest("GET", "/v1/instances", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		p, err := auth.Authenticate(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.Subject != "alice" || p.Tenant != "acme" || !p.has(RoleAdmin) {
			t.Fatalf("%s: bad principal %+v", name, p)
		}
	}
	if fetches != 1 {
		t.Fatalf("JWKS fetched %d times, want 1 (cache)", fetches)
	}
}

func TestOIDCRejections(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks, _ := json.Marshal(map[string]any{"keys": []any{rsaJWK("kid1", &rsaKey.PublicKey)}})

	auth, err := OIDCAuth(OIDCConfig{
		Issuer: "https://idp.example", Audience: "fliable", JWKS: jwks, DefaultRole: RoleViewer,
	})
	if err != nil {
		t.Fatal(err)
	}
	good := map[string]any{
		"iss": "https://idp.example", "aud": "fliable", "sub": "bob",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}
	cases := map[string]string{
		"wrong key": mintRS256(t, otherKey, "kid1", good),
		"expired": mintRS256(t, rsaKey, "kid1", map[string]any{
			"iss": "https://idp.example", "aud": "fliable",
			"exp": float64(time.Now().Add(-time.Hour).Unix()),
		}),
		"wrong issuer": mintRS256(t, rsaKey, "kid1", map[string]any{
			"iss": "https://evil.example", "aud": "fliable",
			"exp": float64(time.Now().Add(time.Hour).Unix()),
		}),
		"wrong audience": mintRS256(t, rsaKey, "kid1", map[string]any{
			"iss": "https://idp.example", "aud": "other",
			"exp": float64(time.Now().Add(time.Hour).Unix()),
		}),
	}
	for name, tok := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if p, err := auth.Authenticate(req); err == nil && p != nil {
			t.Fatalf("%s: token accepted", name)
		}
	}

	// Valid token, default role fallback.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+mintRS256(t, rsaKey, "kid1", good))
	p, err := auth.Authenticate(req)
	if err != nil || p == nil || !p.has(RoleViewer) || p.has(RoleOperator) {
		t.Fatalf("default role: %+v %v", p, err)
	}

	// Non-JWT bearer abstains so the chain can continue.
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer opaque-token")
	if p, err := auth.Authenticate(req); p != nil || err != nil {
		t.Fatalf("expected abstain, got %+v %v", p, err)
	}
}

func TestOIDCEndToEndWithServer(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks, _ := json.Marshal(map[string]any{"keys": []any{rsaJWK("kid1", &rsaKey.PublicKey)}})
	auth, err := OIDCAuth(OIDCConfig{JWKS: jwks, RolesClaim: "roles"})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(engine.New(store.NewMemory()), WithAuth(auth))

	// No token → 401.
	req := httptest.NewRequest("GET", "/v1/instances", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon: %d", rec.Code)
	}

	// Viewer can read but cannot deploy.
	tok := mintRS256(t, rsaKey, "kid1", map[string]any{
		"sub": "eve", "roles": []any{"viewer"},
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	req = httptest.NewRequest("GET", "/v1/instances", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer read: %d %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest("POST", "/v1/definitions", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer deploy: %d", rec.Code)
	}
}
