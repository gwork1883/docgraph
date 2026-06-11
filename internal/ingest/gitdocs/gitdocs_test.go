package gitdocs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncUsesLocalCheckoutWithoutDefaultCache(t *testing.T) {
	ctx := context.Background()
	workdir := t.TempDir()
	t.Chdir(workdir)

	repo := initTestGitRepo(t, map[string]string{
		"docs/guide.md": "# Guide\nlocal checkout content\n",
	})

	checkout, err := Sync(ctx, "file://"+filepath.ToSlash(repo), `{"branch":"main","path":"docs"}`)
	if err != nil {
		t.Fatalf("Sync returned error: %v", err)
	}
	wantRoot := filepath.Join(repo, "docs")
	if checkout.Root != wantRoot {
		t.Fatalf("checkout.Root = %q, want %q", checkout.Root, wantRoot)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".docgraph", "cache", "git")); !os.IsNotExist(err) {
		t.Fatalf("default git cache exists or stat returned unexpected error: %v", err)
	}
}

func TestSyncRemoteRequiresExplicitCache(t *testing.T) {
	_, err := Sync(context.Background(), "https://example.com/repo.git", `{}`)
	if err == nil {
		t.Fatal("Sync returned nil error for remote source without cache")
	}
	if !strings.Contains(err.Error(), "configure cache explicitly") {
		t.Fatalf("Sync error = %q, want explicit cache guidance", err.Error())
	}
}

func initTestGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()

	repo := t.TempDir()
	runTestGit(t, repo, "init", "-b", "main")
	runTestGit(t, repo, "config", "user.email", "docgraph-tests@example.com")
	runTestGit(t, repo, "config", "user.name", "DocGraph Tests")
	for name, content := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll returned error: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile returned error: %v", err)
		}
	}
	runTestGit(t, repo, "add", ".")
	runTestGit(t, repo, "commit", "-m", "initial docs")
	return repo
}

func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s returned error: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
}
