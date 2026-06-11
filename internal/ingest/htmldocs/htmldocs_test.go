package htmldocs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseExtractsHTMLTextAndInternalLinks(t *testing.T) {
	doc := Parse("guides/index.html", []byte(`<!doctype html>
<html>
  <head><title>Member HTML Docs</title><script>ignoredToken()</script></head>
  <body>
    <nav>navigation noise token</nav>
    <h1 id="overview">Member Portal</h1>
    <p>HTML overview htmloverviewtoken for product teams.</p>
    <h2 id="api">API Usage</h2>
    <p>Call <code>GET /member/html</code> for htmlapitoken.</p>
    <main id="main-content"><p>Read <a href="details.html#quota">quota details</a>, <a href="../guide">guide</a>, and <a href="https://example.com/out">external docs</a>.</p></main>
    <code>curl /member/html</code>
  </body>
</html>`))

	if doc.Title != "Member HTML Docs" {
		t.Fatalf("Title = %q, want Member HTML Docs", doc.Title)
	}
	if len(doc.Sections) != 2 {
		t.Fatalf("Sections len = %d, want 2: %+v", len(doc.Sections), doc.Sections)
	}
	if doc.Sections[0].Title != "Member Portal" || doc.Sections[0].Anchor != "overview" || doc.Sections[0].Content != "HTML overview htmloverviewtoken for product teams." {
		t.Fatalf("overview section = %+v, want h1 content", doc.Sections[0])
	}
	if doc.Sections[1].Title != "API Usage" || doc.Sections[1].Anchor != "api" {
		t.Fatalf("api section heading = %+v, want h2 api", doc.Sections[1])
	}
	if !contains(doc.Sections[1].Content, "GET /member/html") || !contains(doc.Sections[1].Content, "curl /member/html") || !contains(doc.Sections[1].Content, "htmlapitoken") {
		t.Fatalf("api section content = %q, want paragraph and code text", doc.Sections[1].Content)
	}
	if contains(doc.Sections[1].Content, "ignoredToken") || contains(doc.Sections[1].Content, "navigation noise token") {
		t.Fatalf("api section content = %q, want extracted text without script/nav noise", doc.Sections[1].Content)
	}
	if !containsString(doc.Anchors, "main-content") {
		t.Fatalf("anchors = %+v, want main-content", doc.Anchors)
	}
	if len(doc.Sections[1].Links) != 2 || doc.Sections[1].Links[0].Href != "details.html#quota" || doc.Sections[1].Links[1].Href != "../guide" {
		t.Fatalf("api section links = %+v, want internal HTML and extensionless links", doc.Sections[1].Links)
	}
}

func TestScanLoadsOnlyHTMLFiles(t *testing.T) {
	root := t.TempDir()
	writeHTMLTestFile(t, filepath.Join(root, "index.html"), `<h1>Home</h1><p>htmlscantoken</p>`)
	writeHTMLTestFile(t, filepath.Join(root, "details.htm"), `<h1>Details</h1><p>details token</p>`)
	writeHTMLTestFile(t, filepath.Join(root, "notes.md"), `# ignored`)

	docs, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("Scan returned %d docs, want 2: %+v", len(docs), docs)
	}
}

func contains(value string, want string) bool {
	return strings.Contains(value, want)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func writeHTMLTestFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}
