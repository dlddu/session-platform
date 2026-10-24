package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenRepoAccess(t *testing.T) {
	ctx := context.Background()
	full := oidcConfig{
		issuer: "https://auth.example", clientID: "session-platform", clientSecret: "s",
		redirectURL: "https://sp.example/api/v1/auth/callback", cookieKey: strings.Repeat("k", 32),
	}

	off, err := openRepoAccess(ctx, repoAccessConfig{})
	if err != nil || off.store != nil || off.auth != nil {
		t.Fatalf("unset = %+v, %v; want everything off", off, err)
	}
	if got := (repoAccessConfig{}).helperURL("ns"); got != "" {
		t.Fatalf("helper URL with the feature off = %q", got)
	}

	if _, err := openRepoAccess(ctx, repoAccessConfig{dbPath: filepath.Join(t.TempDir(), "a.db")}); err == nil {
		t.Fatal("allow list without sign-in was accepted")
	}
	half := full
	half.cookieKey = "short"
	if _, err := openRepoAccess(ctx, repoAccessConfig{oidc: half}); err == nil {
		t.Fatal("short cookie key was accepted")
	}

	signInOnly, err := openRepoAccess(ctx, repoAccessConfig{oidc: full})
	if err != nil || signInOnly.auth == nil || signInOnly.store != nil {
		t.Fatalf("sign-in only = %+v, %v", signInOnly, err)
	}

	cfg := repoAccessConfig{dbPath: filepath.Join(t.TempDir(), "a.db"), oidc: full}
	on, err := openRepoAccess(ctx, cfg)
	if err != nil || on.auth == nil || on.store == nil {
		t.Fatalf("on = %+v, %v", on, err)
	}
	defer on.close()
	if got := cfg.helperURL("session-platform"); got != "http://control-plane.session-platform.svc.cluster.local" {
		t.Errorf("default helper URL = %q", got)
	}
	cfg.internalURL = "http://cp:8080"
	if got := cfg.helperURL("x"); got != "http://cp:8080" {
		t.Errorf("override helper URL = %q", got)
	}
}
