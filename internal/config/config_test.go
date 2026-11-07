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

func TestLoad_BackupDefaults(t *testing.T) {
	cfg, err := Load(envFromMap(map[string]string{
		"TS_HOST": "h", "TS_USER": "u", "TS_PASSWORD": "p",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BackupDir != "" || cfg.BackupInterval != 24*time.Hour || cfg.BackupKeep != 14 || cfg.BackupRate != 2 {
		t.Errorf("backup defaults: %+v", cfg)
	}
}

func TestLoad_BackupValidation(t *testing.T) {
	base := map[string]string{"TS_HOST": "h", "TS_USER": "u", "TS_PASSWORD": "p"}
	for _, tc := range []struct {
		key, val, wantErr string
	}{
		{"BACKUP_INTERVAL", "daily", "BACKUP_INTERVAL"},
		{"BACKUP_INTERVAL", "30s", "at least 1m"},
		{"BACKUP_KEEP", "-1", "BACKUP_KEEP"},
		{"BACKUP_KEEP", "x", "BACKUP_KEEP"},
		{"BACKUP_RATE", "-2", "BACKUP_RATE"},
	} {
		env := map[string]string{tc.key: tc.val}
		for k, v := range base {
			env[k] = v
		}
		_, err := Load(envFromMap(env))
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s=%s: got %v, want error containing %q", tc.key, tc.val, err, tc.wantErr)
		}
	}
	env := map[string]string{"BACKUP_DIR": "/backups", "BACKUP_INTERVAL": "6h", "BACKUP_KEEP": "0", "BACKUP_RATE": "0"}
	for k, v := range base {
		env[k] = v
	}
	cfg, err := Load(envFromMap(env))
	if err != nil || cfg.BackupDir != "/backups" || cfg.BackupInterval != 6*time.Hour || cfg.BackupKeep != 0 || cfg.BackupRate != 0 {
		t.Errorf("explicit values: %+v, %v", cfg, err)
	}
}
