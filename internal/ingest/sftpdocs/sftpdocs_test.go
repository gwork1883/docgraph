package sftpdocs

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSyncMirrorsMatchingRemoteFilesToCacheAndRemovesStale(t *testing.T) {
	cache := t.TempDir()
	writeSFTPTestFile(t, filepath.Join(cache, "old.md"), "stale")
	fake := &fakeRemoteClient{files: map[string]string{
		"/var/www/docs/guide.md":                   "# Guide\nsftpmarkdownalpha\n",
		"/var/www/docs/site/index.html":            "<h1>Home</h1><p>sftphtmlbeta</p>",
		"/var/www/docs/site/private/hidden.md":     "# Hidden\nsftphiddengamma\n",
		"/var/www/docs/node_modules/pkg/readme.md": "# Ignored\nsftpignorednode\n",
		"/var/www/docs/image.png":                  "ignored",
	}}
	restore := setConnectRemoteForTest(func(context.Context, remoteTarget, Config) (remoteClient, error) {
		return fake, nil
	})
	defer restore()

	checkout, err := Sync(context.Background(), "sftp://alice@example.com:2222/var/www/docs", "source-sftp", `{
		"cache": "`+filepath.ToSlash(cache)+`",
		"url_prefix": "https://docs.example.com",
		"include": ["**/*.md", "**/*.html"],
		"exclude": ["**/private/**"]
	}`)
	if err != nil {
		t.Fatalf("Sync returned error: %v", err)
	}
	if checkout.Root != filepath.Clean(cache) || checkout.URL != "https://docs.example.com" {
		t.Fatalf("checkout = %+v, want cache root and configured URL", checkout)
	}
	assertSFTPTestFile(t, filepath.Join(cache, "guide.md"), "sftpmarkdownalpha")
	assertSFTPTestFile(t, filepath.Join(cache, "site", "index.html"), "sftphtmlbeta")
	assertSFTPTestMissing(t, filepath.Join(cache, "site", "private", "hidden.md"))
	assertSFTPTestMissing(t, filepath.Join(cache, "node_modules", "pkg", "readme.md"))
	assertSFTPTestMissing(t, filepath.Join(cache, "old.md"))
	if !fake.closed {
		t.Fatalf("fake client was not closed")
	}
}

func TestSyncReportsNoMatchingRemoteDocuments(t *testing.T) {
	cache := t.TempDir()
	restore := setConnectRemoteForTest(func(context.Context, remoteTarget, Config) (remoteClient, error) {
		return &fakeRemoteClient{files: map[string]string{"/srv/docs/image.png": "ignored"}}, nil
	})
	defer restore()

	_, err := Sync(context.Background(), "sftp://bob@example.com/srv/docs", "source-sftp", `{"cache":"`+filepath.ToSlash(cache)+`"}`)
	if err == nil || !strings.Contains(err.Error(), "no SFTP documents") {
		t.Fatalf("Sync error = %v, want no SFTP documents", err)
	}
}

func TestParseDSNDefaultsPortAndRequiresPath(t *testing.T) {
	remote, err := parseDSN("sftp://carol@example.com/opt/docs")
	if err != nil {
		t.Fatalf("parseDSN returned error: %v", err)
	}
	if remote.User != "carol" || remote.Host != "example.com" || remote.Port != "22" || remote.Path != "/opt/docs" {
		t.Fatalf("remote = %+v, want parsed user/host/default port/path", remote)
	}

	_, err = parseDSN("ssh://carol@example.com/opt/docs")
	if err == nil || !strings.Contains(err.Error(), "sftp://") {
		t.Fatalf("wrong scheme error = %v, want sftp scheme error", err)
	}
	_, err = parseDSN("sftp://carol@example.com")
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("missing path error = %v, want path error", err)
	}
}

func setConnectRemoteForTest(fn func(context.Context, remoteTarget, Config) (remoteClient, error)) func() {
	previous := connectRemote
	connectRemote = fn
	return func() { connectRemote = previous }
}

type fakeRemoteClient struct {
	files  map[string]string
	closed bool
}

func (c *fakeRemoteClient) Walk(root string, visit func(path string, isDir bool) error) error {
	paths := make([]string, 0, len(c.files)+1)
	paths = append(paths, path.Clean(root))
	seenDirs := map[string]bool{}
	for filePath := range c.files {
		clean := path.Clean(filePath)
		for dir := path.Dir(clean); strings.HasPrefix(dir, path.Clean(root)); dir = path.Dir(dir) {
			if !seenDirs[dir] {
				seenDirs[dir] = true
				paths = append(paths, dir)
			}
			if dir == path.Clean(root) || dir == "/" || dir == "." {
				break
			}
		}
		paths = append(paths, clean)
	}
	sort.Strings(paths)
	visited := map[string]bool{}
	for _, p := range paths {
		if visited[p] {
			continue
		}
		visited[p] = true
		_, isFile := c.files[p]
		if err := visit(p, !isFile); err != nil {
			return err
		}
	}
	return nil
}

func (c *fakeRemoteClient) ReadFile(path string) ([]byte, error) {
	return []byte(c.files[path]), nil
}

func (c *fakeRemoteClient) Close() error {
	c.closed = true
	return nil
}

func writeSFTPTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}

func assertSFTPTestFile(t *testing.T, path string, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) returned error: %v", path, err)
	}
	if !strings.Contains(string(content), want) {
		t.Fatalf("ReadFile(%s) = %q, want substring %q", path, content, want)
	}
}

func assertSFTPTestMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
		t.Fatalf("Stat(%s) error = %v, want not exist", path, err)
	}
}
