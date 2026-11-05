package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	cfg := New()
	cfg.APIKey = "sk-test-0123456789"
	retries := 5
	cfg.Retries = &retries
	cfg.Timeout = "45s"
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %#o, want 0600", perm)
	}
	if err := PermWarning(path); err != nil {
		t.Fatalf("PermWarning on a 0600 file: %v", err)
	}

	loaded, gotPath, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != path || loaded.APIKey != cfg.APIKey {
		t.Fatalf("loaded = %+v from %s", loaded, gotPath)
	}
	timeout, retriesOut, ttl := loaded.ResolvedTimeouts()
	if timeout != 45*time.Second || retriesOut != 5 || ttl != DefaultCacheTTL {
		t.Fatalf("resolved = %v %d %v", timeout, retriesOut, ttl)
	}
}

func TestPermWarningFlagsLooseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PermWarning(path); err == nil {
		t.Fatal("expected a warning for a 0644 config file")
	}
}

func TestMissingConfigIsNotAnError(t *testing.T) {
	cfg, _, err := LoadFrom(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if cfg.APIKey != "" {
		t.Fatalf("cfg = %+v", cfg)
	}
	timeout, retries, ttl := cfg.ResolvedTimeouts()
	if timeout != DefaultTimeout || retries != DefaultRetries || ttl != DefaultCacheTTL {
		t.Fatalf("defaults = %v %d %v", timeout, retries, ttl)
	}
}

func TestSetValidation(t *testing.T) {
	cfg := New()
	valid := map[string]string{
		"api_key":   "sk-abc",
		"base_url":  "https://api.deepseek.com/",
		"timeout":   "10s",
		"retries":   "0",
		"cache_ttl": "0s",
	}
	for k, v := range valid {
		if err := cfg.Set(k, v); err != nil {
			t.Fatalf("Set(%q,%q): %v", k, v, err)
		}
	}
	if cfg.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("base_url should be trimmed, got %q", cfg.BaseURL)
	}
	invalid := [][2]string{
		{"base_url", "not-a-url"},
		{"base_url", "ftp://example.com"},
		{"timeout", "-5s"},
		{"retries", "-1"},
		{"retries", "x"},
		{"cache_ttl", "soon"},
		{"api_key", "  "},
		{"unknown", "1"},
	}
	for _, c := range invalid {
		if err := cfg.Set(c[0], c[1]); err == nil {
			t.Fatalf("Set(%q,%q) should fail", c[0], c[1])
		}
	}
}

func TestUnset(t *testing.T) {
	cfg := New()
	_ = cfg.Set("api_key", "sk-abc")
	_ = cfg.Set("retries", "3")
	if err := cfg.Unset("api_key"); err != nil || cfg.APIKey != "" {
		t.Fatalf("api_key not cleared: %+v %v", cfg, err)
	}
	if err := cfg.Unset("retries"); err != nil || cfg.Retries != nil {
		t.Fatalf("retries not cleared: %+v %v", cfg, err)
	}
	if err := cfg.Unset("nope"); err == nil {
		t.Fatal("unknown key should fail")
	}
}

func TestMaskNeverLeaksTheSecret(t *testing.T) {
	cases := map[string]string{
		"":                    "(none)",
		"sk-short":            "****",
		"sk-1234567890abcdef": "sk-123…cdef",
	}
	for in, want := range cases {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}
