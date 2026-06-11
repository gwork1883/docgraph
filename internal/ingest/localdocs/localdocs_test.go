package localdocs

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScanDiscoversMarkdownAndSplitsSections(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "docs/product.md", "# Product\nIntro\n\n## Setup\nInstall\n\n### Details ###\nMore\n")
	writeFile(t, root, "docs/reference.markdown", "No headings\n")
	writeFile(t, root, "docs/notes.txt", "# Ignore\n")

	docs, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}

	if len(docs) != 2 {
		t.Fatalf("len(docs) = %d, want 2", len(docs))
	}

	product := docs[0]
	if product.Path != "docs/product.md" {
		t.Fatalf("first doc path = %q, want docs/product.md", product.Path)
	}
	if product.Title != "Product" {
		t.Fatalf("title = %q, want Product", product.Title)
	}
	if product.Hash != shaHex("# Product\nIntro\n\n## Setup\nInstall\n\n### Details ###\nMore\n") {
		t.Fatalf("doc hash = %q, want sha256 of content", product.Hash)
	}

	wantSections := []Section{
		{HeadingPath: []string{"Product"}, Title: "Product", Content: "Intro\n\n", Ordinal: 0, Hash: shaHex("Intro\n\n")},
		{HeadingPath: []string{"Product", "Setup"}, Title: "Setup", Content: "Install\n\n", Ordinal: 1, Hash: shaHex("Install\n\n")},
		{HeadingPath: []string{"Product", "Setup", "Details"}, Title: "Details", Content: "More\n", Ordinal: 2, Hash: shaHex("More\n")},
	}
	if !reflect.DeepEqual(product.Sections, wantSections) {
		t.Fatalf("sections mismatch\n got: %#v\nwant: %#v", product.Sections, wantSections)
	}

	reference := docs[1]
	if reference.Path != "docs/reference.markdown" {
		t.Fatalf("second doc path = %q, want docs/reference.markdown", reference.Path)
	}
	if reference.Title != "reference.markdown" {
		t.Fatalf("fallback title = %q, want reference.markdown", reference.Title)
	}
	if len(reference.Sections) != 1 {
		t.Fatalf("len(reference.Sections) = %d, want 1", len(reference.Sections))
	}
	if reference.Sections[0].Title != "reference.markdown" {
		t.Fatalf("fallback section title = %q, want reference.markdown", reference.Sections[0].Title)
	}
	if reference.Sections[0].Content != "No headings\n" {
		t.Fatalf("fallback section content = %q, want No headings newline", reference.Sections[0].Content)
	}
}

func TestScanSkipsHiddenAndBuildVendorDirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".hidden/secret.md", "# Secret\n")
	writeFile(t, root, "vendor/pkg/readme.md", "# Vendor\n")
	writeFile(t, root, "node_modules/pkg/readme.md", "# Node\n")
	writeFile(t, root, "build/out.md", "# Build\n")
	writeFile(t, root, "dist/out.md", "# Dist\n")
	writeFile(t, root, "target/out.md", "# Target\n")
	writeFile(t, root, "docs/visible.md", "# Visible\n")

	docs, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}

	if len(docs) != 1 {
		t.Fatalf("len(docs) = %d, want 1: %#v", len(docs), docs)
	}
	if docs[0].Path != "docs/visible.md" {
		t.Fatalf("doc path = %q, want docs/visible.md", docs[0].Path)
	}
}

func TestParseATXHeadingRules(t *testing.T) {
	tests := []struct {
		name string
		line string
		want heading
		ok   bool
	}{
		{name: "h1", line: "# Title\n", want: heading{Level: 1, Title: "Title"}, ok: true},
		{name: "indented up to three spaces", line: "   ## Child\r\n", want: heading{Level: 2, Title: "Child"}, ok: true},
		{name: "empty title", line: "###\n", want: heading{Level: 3, Title: ""}, ok: true},
		{name: "closing hashes", line: "#### Done ###\n", want: heading{Level: 4, Title: "Done"}, ok: true},
		{name: "too indented", line: "    # Code\n", ok: false},
		{name: "needs whitespace after marker", line: "#Not heading\n", ok: false},
		{name: "too many hashes", line: "####### Not heading\n", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseATXHeading(tt.line)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Fatalf("heading = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseMarkdownKeepsFallbackTitleForEmptyH1(t *testing.T) {
	doc := parseMarkdown("docs/empty.md", "#\nBody\n")

	if doc.Title != "empty.md" {
		t.Fatalf("title = %q, want empty.md", doc.Title)
	}
	if len(doc.Sections) != 1 {
		t.Fatalf("len(sections) = %d, want 1", len(doc.Sections))
	}
	if doc.Sections[0].Title != "" {
		t.Fatalf("section title = %q, want empty", doc.Sections[0].Title)
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func shaHex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
