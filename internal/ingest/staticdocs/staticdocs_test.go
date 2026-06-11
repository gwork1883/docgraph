package staticdocs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/ingest/htmldocs"
)

func TestScanMixedStaticDocsIncludeExcludeAndDefaults(t *testing.T) {
	root := t.TempDir()
	writeStaticTestFile(t, filepath.Join(root, "docs", "intro.md"), "# Intro\nstaticmarkdownalpha\n")
	writeStaticTestFile(t, filepath.Join(root, "docs", "index.html"), `<!doctype html><title>HTML Docs</title><h1 id="home">Home</h1><p>statichtmlbeta</p>`)
	writeStaticTestFile(t, filepath.Join(root, "docs", "notes.txt"), "statictextgamma")
	writeStaticTestFile(t, filepath.Join(root, "docs", "draft.tmp"), "ignored")
	writeStaticTestFile(t, filepath.Join(root, "node_modules", "pkg", "readme.md"), "# ignored staticnodemodules")

	docs, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("Scan returned %d docs, want 3: %+v", len(docs), docs)
	}
	if docs[0].Path != "docs/index.html" || docs[1].Path != "docs/intro.md" || docs[2].Path != "docs/notes.txt" {
		t.Fatalf("doc paths = %+v, want sorted html/md/txt docs", docPaths(docs))
	}
	if !sectionContentContains(docs, "staticmarkdownalpha") || !sectionContentContains(docs, "statichtmlbeta") || !sectionContentContains(docs, "statictextgamma") {
		t.Fatalf("docs = %+v, want md/html/txt content", docs)
	}
	if sectionContentContains(docs, "staticnodemodules") {
		t.Fatalf("docs = %+v, want node_modules skipped", docs)
	}
}

func TestScanHonorsIncludeExcludeConfig(t *testing.T) {
	root := t.TempDir()
	writeStaticTestFile(t, filepath.Join(root, "docs", "keep.md"), "# Keep\nstatickeepalpha\n")
	writeStaticTestFile(t, filepath.Join(root, "docs", "skip.md"), "# Skip\nstaticskipbeta\n")
	writeStaticTestFile(t, filepath.Join(root, "docs", "page.html"), "<h1>Page</h1><p>statichtmlignored</p>")

	docs, err := Scan(root, `{"include":"**/*.md","exclude":"**/skip.md"}`)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(docs) != 1 || docs[0].Path != "docs/keep.md" {
		t.Fatalf("docs = %+v, want only keep.md", docPaths(docs))
	}
	if sectionContentContains(docs, "staticskipbeta") || sectionContentContains(docs, "statichtmlignored") {
		t.Fatalf("docs = %+v, want excluded content absent", docs)
	}
}

func TestScanReportsClearEmptyAndMissingRootErrors(t *testing.T) {
	_, err := Scan(filepath.Join(t.TempDir(), "missing"), "")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing root error = %v, want does not exist", err)
	}

	root := t.TempDir()
	writeStaticTestFile(t, filepath.Join(root, "image.png"), "ignored")
	_, err = Scan(root, "")
	if err == nil || !strings.Contains(err.Error(), "no static documents") {
		t.Fatalf("empty docs error = %v, want no static documents", err)
	}
}

func writeStaticTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}

func docPaths(docs []htmldocs.Document) []string {
	paths := make([]string, 0, len(docs))
	for _, doc := range docs {
		paths = append(paths, doc.Path)
	}
	return paths
}

func sectionContentContains(docs []htmldocs.Document, want string) bool {
	for _, doc := range docs {
		for _, section := range doc.Sections {
			if strings.Contains(section.Content, want) {
				return true
			}
		}
	}
	return false
}
