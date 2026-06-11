package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("Server.Host = %q, want %q", cfg.Server.Host, "127.0.0.1")
	}
	if cfg.Server.Port != 8787 {
		t.Fatalf("Server.Port = %d, want %d", cfg.Server.Port, 8787)
	}
	if cfg.Server.DataDir != ".docgraph" {
		t.Fatalf("Server.DataDir = %q, want %q", cfg.Server.DataDir, ".docgraph")
	}
	wantDSN := "sqlite://.docgraph/docgraph.db"
	if cfg.Storage.DSN != wantDSN {
		t.Fatalf("Storage.DSN = %q, want %q", cfg.Storage.DSN, wantDSN)
	}
	if cfg.Search.DSN != wantDSN {
		t.Fatalf("Search.DSN = %q, want %q", cfg.Search.DSN, wantDSN)
	}
	if cfg.Vector.DSN != "none://" {
		t.Fatalf("Vector.DSN = %q, want %q", cfg.Vector.DSN, "none://")
	}
	if cfg.Auth.Mode != "none" {
		t.Fatalf("Auth.Mode = %q, want %q", cfg.Auth.Mode, "none")
	}
}

func TestLoadEmptyPathReturnsDefault(t *testing.T) {
	got, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") returned error: %v", err)
	}
	want := Default()
	if got != want {
		t.Fatalf("Load(\"\") = %#v, want %#v", got, want)
	}
}

func TestLoadAppliesOverridesAndKeepsDefaults(t *testing.T) {
	path := writeConfig(t, `
server:
  host: "0.0.0.0" # listen on all interfaces
  port: 9797
  data_dir: '/var/lib/docgraph'
storage:
  dsn: sqlite:///var/lib/docgraph/docgraph.db
search:
  dsn: sqlite:///var/lib/docgraph/docgraph.db
auth:
  mode: token
  token: test-token
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) returned error: %v", path, err)
	}

	if cfg.Server.Host != "0.0.0.0" {
		t.Fatalf("Server.Host = %q, want %q", cfg.Server.Host, "0.0.0.0")
	}
	if cfg.Server.Port != 9797 {
		t.Fatalf("Server.Port = %d, want %d", cfg.Server.Port, 9797)
	}
	if cfg.Server.DataDir != "/var/lib/docgraph" {
		t.Fatalf("Server.DataDir = %q, want %q", cfg.Server.DataDir, "/var/lib/docgraph")
	}
	if cfg.Storage.DSN != "sqlite:///var/lib/docgraph/docgraph.db" {
		t.Fatalf("Storage.DSN = %q", cfg.Storage.DSN)
	}
	if cfg.Search.DSN != cfg.Storage.DSN {
		t.Fatalf("Search.DSN = %q, want storage DSN %q", cfg.Search.DSN, cfg.Storage.DSN)
	}
	if cfg.Vector.DSN != "none://" {
		t.Fatalf("Vector.DSN = %q, want default %q", cfg.Vector.DSN, "none://")
	}
	if cfg.Auth.Mode != "token" {
		t.Fatalf("Auth.Mode = %q, want %q", cfg.Auth.Mode, "token")
	}
	if cfg.Auth.Token != "test-token" {
		t.Fatalf("Auth.Token = %q, want %q", cfg.Auth.Token, "test-token")
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "top level key",
			content: `
host: 0.0.0.0
`,
			want: "top-level key",
		},
		{
			name: "unknown section",
			content: `
unknown:
  dsn: sqlite://db.sqlite
`,
			want: "unknown section",
		},
		{
			name: "unknown key",
			content: `
server:
  scheme: http
`,
			want: "unknown server key",
		},
		{
			name: "invalid port",
			content: `
server:
  port: eighty
`,
			want: "invalid server.port",
		},
		{
			name: "malformed line",
			content: `
server
`,
			want: "expected key: value",
		},
		{
			name: "empty storage dsn",
			content: `
storage:
  dsn: ''
`,
			want: "storage.dsn is required",
		},
		{
			name: "token mode without token",
			content: `
auth:
  mode: token
`,
			want: "auth.token is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, tt.content)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load returned nil error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %q, want substring %q", err.Error(), tt.want)
			}
		})
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "docgraph.yaml")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
