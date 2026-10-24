package adminauth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeIdP is a minimal OpenID provider: discovery, JWKS, an authorize
// endpoint that approves immediately, and a token endpoint that checks PKCE.
type fakeIdP struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	clientID  string
	challenge string
	username  string
	// basicAuth records whether the last token request authenticated with
	// HTTP Basic, and formClient the client_id it sent in the form.
	basicAuth  bool
	formClient string
}

func newFakeIdP(t *testing.T, clientID string) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key, clientID: clientID, username: "jd"}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.challenge = q.Get("code_challenge")
		if q.Get("code_challenge_method") != "S256" || f.challenge == "" {
			http.Error(w, "pkce required", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=abc&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_, _, f.basicAuth = r.BasicAuth()
		f.formClient = r.Form.Get("client_id")
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("code") != "abc" || b64(sum[:]) != f.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 300,
			"id_token": f.idToken(t),
		})
	})
	return f
}

func (f *fakeIdP) idToken(t *testing.T) string {
	header := b64([]byte(`{"alg":"RS256","kid":"k1","typ":"JWT"}`))
	now := time.Now().Unix()
	claims, _ := json.Marshal(map[string]any{
		"iss": f.srv.URL, "aud": f.clientID, "sub": "u-1", "iat": now, "exp": now + 300,
		"preferred_username": f.username, "email": "jd@example.com",
	})
	signing := header + "." + b64(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newServer(t *testing.T) (*httptest.Server, *fakeIdP) {
	return newServerWithSecret(t, "s3cret")
}

func newServerWithSecret(t *testing.T, secret string) (*httptest.Server, *fakeIdP) {
	t.Helper()
	var app *httptest.Server
	idp := newFakeIdP(t, "session-platform")
	mux := http.NewServeMux()
	app = httptest.NewServer(mux)
	t.Cleanup(app.Close)
	a, err := New(Config{
		Issuer: idp.srv.URL, ClientID: "session-platform", ClientSecret: secret,
		RedirectURL: app.URL + "/api/v1/auth/callback", CookieKey: []byte(strings.Repeat("k", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Routes(mux)
	mux.HandleFunc("GET /admin", a.Require(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFrom(r.Context())
		_, _ = w.Write([]byte("hello " + u.Name))
	}))
	mux.HandleFunc("GET /access", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("landing")) })
	return app, idp
}

func client(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func TestSignInFlowSetsSessionAndRedirectsBack(t *testing.T) {
	app, _ := newServer(t)
	c := client(t)

	res, err := c.Get(app.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("before sign-in: %d, want 401", res.StatusCode)
	}

	res, err = c.Get(app.URL + "/api/v1/auth/login?return=/access")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(res)
	if res.StatusCode != http.StatusOK || body != "landing" {
		t.Fatalf("after sign-in landed on %s with %d %q", res.Request.URL, res.StatusCode, body)
	}

	res, _ = c.Get(app.URL + "/admin")
	if got := readAll(res); res.StatusCode != http.StatusOK || got != "hello jd" {
		t.Fatalf("after sign-in: %d %q", res.StatusCode, got)
	}
	res, _ = c.Get(app.URL + "/api/v1/auth/me")
	var me User
	_ = json.NewDecoder(res.Body).Decode(&me)
	res.Body.Close()
	if me.Name != "jd" || me.Email != "jd@example.com" {
		t.Fatalf("me = %+v", me)
	}

	req, _ := http.NewRequest(http.MethodPost, app.URL+"/api/v1/auth/logout", nil)
	res, _ = c.Do(req)
	res.Body.Close()
	res, _ = c.Get(app.URL + "/admin")
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout: %d, want 401", res.StatusCode)
	}
}

func TestPublicClientSendsNoClientAuthentication(t *testing.T) {
	app, idp := newServerWithSecret(t, "")
	c := client(t)
	res, err := c.Get(app.URL + "/api/v1/auth/login?return=/access")
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(res); body != "landing" {
		t.Fatalf("public client sign-in landed on %s: %d %q", res.Request.URL, res.StatusCode, body)
	}
	if idp.basicAuth || idp.formClient != "session-platform" {
		t.Fatalf("token request: basic auth = %v, form client_id = %q; want none and the client id", idp.basicAuth, idp.formClient)
	}
}

func TestTamperedSessionCookieIsRejected(t *testing.T) {
	app, _ := newServer(t)
	c := client(t)
	res, _ := c.Get(app.URL + "/api/v1/auth/login?return=/access")
	res.Body.Close()
	u, _ := url.Parse(app.URL)
	var forged []*http.Cookie
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == sessionCookie {
			body, sig, _ := strings.Cut(ck.Value, ".")
			payload, _ := base64.RawURLEncoding.DecodeString(body)
			payload = []byte(strings.Replace(string(payload), `"jd"`, `"admin"`, 1))
			forged = append(forged, &http.Cookie{Name: sessionCookie, Value: b64(payload) + "." + sig})
		}
	}
	if len(forged) != 1 {
		t.Fatal("no session cookie after sign-in")
	}
	jar, _ := cookiejar.New(nil)
	jar.SetCookies(u, forged)
	res, _ = (&http.Client{Jar: jar}).Get(app.URL + "/admin")
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged cookie: %d, want 401", res.StatusCode)
	}
}

func TestCallbackWithoutLoginIsRefused(t *testing.T) {
	app, _ := newServer(t)
	res, err := client(t).Get(app.URL + "/api/v1/auth/callback?code=abc&state=x")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback without login cookie: %d, want 400", res.StatusCode)
	}
}

func TestNilAuthAnswers503(t *testing.T) {
	var a *Auth
	rec := httptest.NewRecorder()
	a.Require(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") })(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil auth: %d, want 503", rec.Code)
	}
}

func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"/access": "/access", "": "/", "https://evil.example/": "/", "//evil.example": "/", "/\\evil": "/", "/a\r\nx": "/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	if _, err := New(Config{Issuer: "https://idp", ClientID: "c", ClientSecret: "s", RedirectURL: "https://x/cb", CookieKey: []byte("short")}); err == nil {
		t.Fatal("short cookie key accepted")
	}
	if _, err := New(Config{Issuer: "https://idp", ClientID: "c", RedirectURL: "https://x/cb", CookieKey: make([]byte, 32)}); err != nil {
		t.Fatalf("public client (no secret) refused: %v", err)
	}
	if _, err := New(Config{ClientID: "c", ClientSecret: "s", RedirectURL: "https://x/cb", CookieKey: make([]byte, 32)}); err == nil {
		t.Fatal("missing issuer accepted")
	}
}

func readAll(res *http.Response) string {
	defer res.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}
