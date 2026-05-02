package config

import (
	"strings"
	"testing"
	"time"
)

func envFromMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(envFromMap(map[string]string{
		"TS_HOST":     "ts.example.com",
		"TS_USER":     "serveradmin",
		"TS_PASSWORD": "secret",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TSPort != "10022" {
		t.Errorf("TSPort default: got %q, want %q", cfg.TSPort, "10022")
	}
	if cfg.TSSID != 1 {
		t.Errorf("TSSID default: got %d, want 1", cfg.TSSID)
	}
	if cfg.HTTPAddr != ":9988" {
		t.Errorf("HTTPAddr default: got %q, want %q", cfg.HTTPAddr, ":9988")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel default: got %q", cfg.LogLevel)
	}
	if cfg.DockerHubRepo != "teamspeaksystems/teamspeak6-server" {
		t.Errorf("DockerHubRepo default: got %q", cfg.DockerHubRepo)
	}
	if cfg.UpdateCheckTTL != 15*time.Minute {
		t.Errorf("UpdateCheckTTL default: got %s, want 15m", cfg.UpdateCheckTTL)
	}
}

func TestLoad_MissingRequired(t *testing.T) {
	_, err := Load(envFromMap(map[string]string{
		"TS_USER":     "serveradmin",
		"TS_PASSWORD": "secret",
	}))
	if err == nil {
		t.Fatal("expected error for missing TS_HOST")
	}
	if !strings.Contains(err.Error(), "TS_HOST") {
		t.Errorf("error should mention TS_HOST: %v", err)
	}
}

func TestLoad_InvalidSID(t *testing.T) {
	_, err := Load(envFromMap(map[string]string{
		"TS_HOST":     "ts.example.com",
		"TS_USER":     "serveradmin",
		"TS_PASSWORD": "secret",
		"TS_SID":      "not-a-number",
	}))
	if err == nil || !strings.Contains(err.Error(), "TS_SID") {
		t.Fatalf("expected TS_SID error, got: %v", err)
	}
}

func TestLoad_InvalidTTL(t *testing.T) {
	_, err := Load(envFromMap(map[string]string{
		"TS_HOST":          "ts.example.com",
		"TS_USER":          "serveradmin",
		"TS_PASSWORD":      "secret",
		"UPDATE_CHECK_TTL": "fifteen-minutes",
	}))
	if err == nil || !strings.Contains(err.Error(), "UPDATE_CHECK_TTL") {
		t.Fatalf("expected UPDATE_CHECK_TTL error, got: %v", err)
	}
}

func TestLoad_NegativeTTLRejected(t *testing.T) {
	_, err := Load(envFromMap(map[string]string{
		"TS_HOST":          "ts.example.com",
		"TS_USER":          "serveradmin",
		"TS_PASSWORD":      "secret",
		"UPDATE_CHECK_TTL": "-5m",
	}))
	if err == nil {
		t.Fatal("expected error for negative TTL")
	}
}
