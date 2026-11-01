package main

import "testing"

func TestLoadConfigReadsDropURL(t *testing.T) {
	t.Setenv("DROP_URL", "http://drop-external.drop.svc.cluster.local")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got, want := cfg.dropURL, "http://drop-external.drop.svc.cluster.local"; got != want {
		t.Fatalf("dropURL = %q, want %q", got, want)
	}
	t.Setenv("DROP_URL", "")
	if cfg, _ := loadConfig(); cfg.dropURL != "" {
		t.Fatalf("dropURL = %q, want off when unset", cfg.dropURL)
	}
}
