// Package adminauth signs operators in with OpenID Connect (Authelia in
// production) for the control plane's management surface only — the repo
// access APIs and screen (docs/prd/repo-access.md, AC-H2). Session APIs stay
// outside it.
package adminauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "sp_admin"
	loginCookie   = "sp_admin_login"
	sessionTTL    = 12 * time.Hour
	loginTTL      = 10 * time.Minute
	minKeyBytes   = 32
	authPath      = "/api/v1/auth"
)

type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURL must be registered with the provider and point at
	// /api/v1/auth/callback on the control plane's public origin.
	RedirectURL string
	CookieKey   []byte
	// Secure marks cookies HTTPS-only. Off only for plain-HTTP origins.
	Secure bool
}

type User struct {
	Name  string `json:"user"`
	Email string `json:"email,omitempty"`
}

// Auth is nil-safe: a nil *Auth is "sign-in not configured", which answers 503
// rather than letting anything through.
type Auth struct {
	cfg   Config
	now   func() time.Time
	dial  func(ctx context.Context) (*oidc.Provider, error)
	mu    sync.Mutex
	prov  *oidc.Provider
	oauth *oauth2.Config
}

func New(cfg Config) (*Auth, error) {
	var missing []string
	for name, v := range map[string]string{
		"issuer": cfg.Issuer, "client id": cfg.ClientID, "client secret": cfg.ClientSecret, "redirect URL": cfg.RedirectURL,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("OIDC sign-in is missing: %s", strings.Join(missing, ", "))
	}
	if len(cfg.CookieKey) < minKeyBytes {
		return nil, fmt.Errorf("OIDC cookie key must be at least %d bytes", minKeyBytes)
	}
	a := &Auth{cfg: cfg, now: time.Now}
	// Discovery is lazy so an identity provider that is briefly down does not
	// crash-loop the control plane, whose session APIs never needed it.
	a.dial = func(ctx context.Context) (*oidc.Provider, error) { return oidc.NewProvider(ctx, cfg.Issuer) }
	return a, nil
}

func (a *Auth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+authPath+"/login", a.login)
	mux.HandleFunc("GET "+authPath+"/callback", a.callback)
	mux.HandleFunc("POST "+authPath+"/logout", a.logout)
	mux.HandleFunc("GET "+authPath+"/me", a.me)
}

type ctxKey struct{}

// WithUser is what Require attaches; exported so callers can test handlers
// behind their own guard.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// Require admits only signed-in requests.
func (a *Auth) Require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a == nil {
			writeError(w, http.StatusServiceUnavailable, "sign-in is not configured on this deployment")
			return
		}
		u, ok := a.userFromCookie(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		next(w, r.WithContext(WithUser(r.Context(), u)))
	}
}

func (a *Auth) me(w http.ResponseWriter, r *http.Request) {
	a.Require(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFrom(r.Context())
		writeJSON(w, http.StatusOK, u)
	})(w, r)
}

type loginState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
	Exp      int64  `json:"e"`
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	if a == nil {
		writeError(w, http.StatusServiceUnavailable, "sign-in is not configured on this deployment")
		return
	}
	oc, err := a.client(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "identity provider unavailable: "+err.Error())
		return
	}
	state, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}
	ls := loginState{
		State:    state,
		Verifier: oauth2.GenerateVerifier(),
		Return:   safeReturn(r.URL.Query().Get("return")),
		Exp:      a.now().Add(loginTTL).Unix(),
	}
	if err := a.setSigned(w, loginCookie, authPath, ls, loginTTL); err != nil {
		writeError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}
	http.Redirect(w, r, oc.AuthCodeURL(state, oauth2.S256ChallengeOption(ls.Verifier)), http.StatusFound)
}

func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	if a == nil {
		writeError(w, http.StatusServiceUnavailable, "sign-in is not configured on this deployment")
		return
	}
	var ls loginState
	if !a.getSigned(r, loginCookie, &ls) || ls.Exp < a.now().Unix() {
		writeError(w, http.StatusBadRequest, "sign-in expired or was not started here — try again")
		return
	}
	a.clear(w, loginCookie, authPath)
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		writeError(w, http.StatusUnauthorized, "identity provider refused sign-in: "+e)
		return
	}
	if !hmac.Equal([]byte(q.Get("state")), []byte(ls.State)) {
		writeError(w, http.StatusBadRequest, "sign-in state mismatch — try again")
		return
	}
	oc, err := a.client(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "identity provider unavailable: "+err.Error())
		return
	}
	tok, err := oc.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "could not complete sign-in")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		writeError(w, http.StatusUnauthorized, "identity provider returned no ID token")
		return
	}
	a.mu.Lock()
	prov := a.prov
	a.mu.Unlock()
	idt, err := prov.Verifier(&oidc.Config{ClientID: a.cfg.ClientID}).Verify(r.Context(), raw)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "ID token rejected")
		return
	}
	var claims struct {
		PreferredUsername string `json:"preferred_username"`
		Email             string `json:"email"`
	}
	_ = idt.Claims(&claims)
	u := User{Name: firstNonEmpty(claims.PreferredUsername, claims.Email, idt.Subject), Email: claims.Email}
	if err := a.setSigned(w, sessionCookie, "/", sessionClaims{User: u, Exp: a.now().Add(sessionTTL).Unix()}, sessionTTL); err != nil {
		writeError(w, http.StatusInternalServerError, "could not finish sign-in")
		return
	}
	http.Redirect(w, r, ls.Return, http.StatusFound)
}

func (a *Auth) logout(w http.ResponseWriter, _ *http.Request) {
	if a != nil {
		a.clear(w, sessionCookie, "/")
	}
	w.WriteHeader(http.StatusNoContent)
}

type sessionClaims struct {
	User User  `json:"u"`
	Exp  int64 `json:"e"`
}

func (a *Auth) userFromCookie(r *http.Request) (User, bool) {
	var sc sessionClaims
	if !a.getSigned(r, sessionCookie, &sc) || sc.Exp < a.now().Unix() || sc.User.Name == "" {
		return User{}, false
	}
	return sc.User, true
}

func (a *Auth) client(ctx context.Context) (*oauth2.Config, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.oauth != nil {
		return a.oauth, nil
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	prov, err := a.dial(dctx)
	if err != nil {
		return nil, err
	}
	a.prov = prov
	a.oauth = &oauth2.Config{
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.ClientSecret,
		RedirectURL:  a.cfg.RedirectURL,
		Endpoint:     prov.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
	return a.oauth, nil
}

func (a *Auth) setSigned(w http.ResponseWriter, name, path string, v any, ttl time.Duration) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: body + "." + a.sign(name, body), Path: path,
		MaxAge: int(ttl.Seconds()), HttpOnly: true, Secure: a.cfg.Secure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (a *Auth) getSigned(r *http.Request, name string, dst any) bool {
	c, err := r.Cookie(name)
	if err != nil {
		return false
	}
	body, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.sign(name, body))) {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return false
	}
	return json.Unmarshal(payload, dst) == nil
}

// sign binds the cookie name into the MAC so a login cookie can never be
// replayed as a session cookie.
func (a *Auth) sign(name, body string) string {
	m := hmac.New(sha256.New, a.cfg.CookieKey)
	m.Write([]byte(name + "\x00" + body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *Auth) clear(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1, HttpOnly: true, Secure: a.cfg.Secure, SameSite: http.SameSiteLaxMode})
}

// safeReturn keeps the post-login redirect on this origin.
func safeReturn(raw string) string {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") || strings.ContainsAny(raw, "\r\n") {
		return "/"
	}
	return raw
}

func randomToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
