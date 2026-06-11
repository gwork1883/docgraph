package webdocs

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/ingest/htmldocs"
)

func TestLoadCrawlsSeedAndDescendantLinks(t *testing.T) {
	restore := SetHTTPClientFactoryForTest(func() *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/docs":
				return htmlResponse(`<!doctype html>
<html>
  <head><title>Docs Home</title></head>
  <body>
    <h1 id="home">Home</h1>
    <p>Root webdocsrootalpha content.</p>
    <p><a href="/docs/guide#start">Guide</a> <a href="/docs/missing/">Missing</a> <a href="/other">Other</a> <a href="https://external.example/docs">External</a></p>
  </body>
</html>`), nil
			case "/docs/guide":
				return htmlResponse(`<!doctype html>
<html>
  <head><title>Guide</title></head>
  <body><h1 id="start">Guide</h1><p>Child webdocschildbeta content.</p></body>
</html>`), nil
			case "/docs/missing/":
				return statusResponse(http.StatusNotFound, "not found"), nil
			default:
				t.Fatalf("crawler requested unexpected path %s", r.URL.Path)
				return htmlResponse("not found"), nil
			}
		})}
	})
	t.Cleanup(restore)

	docs, err := Load(context.Background(), "https://docs.example/docs", `{"max_pages":"10","max_depth":"3"}`)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("Load returned %d docs, want 2: %+v", len(docs), docs)
	}
	if docs[0].Path != "guide/index.html" || docs[0].URL != "https://docs.example/docs/guide" {
		t.Fatalf("first doc = %+v, want guide doc with real URL", docs[0])
	}
	if docs[1].Path != "index.html" || docs[1].URL != "https://docs.example/docs" {
		t.Fatalf("second doc = %+v, want root doc with real URL", docs[1])
	}
	if !sectionsContain(docs, "webdocsrootalpha") || !sectionsContain(docs, "webdocschildbeta") {
		t.Fatalf("docs = %+v, want root and child content", docs)
	}
	if len(docs[1].Sections) == 0 || len(docs[1].Sections[0].Links) != 1 || docs[1].Sections[0].Links[0].Href != "guide/index.html#start" {
		t.Fatalf("root links = %+v, want normalized descendant link", docs[1].Sections)
	}
}

func TestLoadSendsConfiguredAuthHeaders(t *testing.T) {
	restore := SetHTTPClientFactoryForTest(func() *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
				t.Fatalf("Authorization = %q, want bearer token", got)
			}
			if got := r.Header.Get("Cookie"); got != "SESSION=abc" {
				t.Fatalf("Cookie = %q, want configured cookie", got)
			}
			if got := r.Header.Get("X-Docs-Auth"); got != "docs-secret" {
				t.Fatalf("X-Docs-Auth = %q, want custom header", got)
			}
			return htmlResponse(`<html><head><title>Private</title></head><body><h1>Private</h1><p>privateauthtoken</p></body></html>`), nil
		})}
	})
	t.Cleanup(restore)

	docs, err := Load(context.Background(), "https://docs.example/private", `{"bearer_token":"secret-token","cookie":"SESSION=abc","headers_json":"{\"X-Docs-Auth\":\"docs-secret\"}"}`)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(docs) != 1 || !sectionsContain(docs, "privateauthtoken") {
		t.Fatalf("docs = %+v, want private content", docs)
	}
}

func TestNewClientDefaultsToUnlimitedCrawl(t *testing.T) {
	client, err := NewClient("https://docs.example", "{}")
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	if client.config.MaxPages != 0 || client.config.MaxDepth != 0 {
		t.Fatalf("config = %+v, want zero max pages/depth for unlimited crawl", client.config)
	}
}

func TestLoadHonorsExplicitMaxPages(t *testing.T) {
	restore := SetHTTPClientFactoryForTest(func() *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/docs":
				return htmlResponse(`<html><head><title>Root</title></head><body><h1>Root</h1><a href="/docs/child">Child</a></body></html>`), nil
			case "/docs/child":
				return htmlResponse(`<html><head><title>Child</title></head><body><h1>Child</h1><p>shouldnotappear</p></body></html>`), nil
			default:
				t.Fatalf("crawler requested unexpected path %s", r.URL.Path)
				return htmlResponse("not found"), nil
			}
		})}
	})
	t.Cleanup(restore)

	docs, err := Load(context.Background(), "https://docs.example/docs", `{"max_pages":"1"}`)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(docs) != 1 || sectionsContain(docs, "shouldnotappear") {
		t.Fatalf("docs = %+v, want only the seed page", docs)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func htmlResponse(body string) *http.Response {
	return statusResponse(http.StatusOK, body)
}

func statusResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func sectionsContain(docs []htmldocs.Document, want string) bool {
	for _, doc := range docs {
		for _, section := range doc.Sections {
			if strings.Contains(section.Content, want) {
				return true
			}
		}
	}
	return false
}

func TestIsLikelySPA(t *testing.T) {
	tests := []struct {
		name  string
		html  string
		isSPA bool
	}{
		{
			name:  "Vue SPA with noscript and app div",
			html:  `<!DOCTYPE html><html><head><meta charset="utf-8"></head><body><div id="app"></div><noscript><strong>We're sorry but dayu-home doesn't work properly without JavaScript enabled.</strong></noscript><script src="/js/app.abc123.js"></script></body></html>`,
			isSPA: true,
		},
		{
			name:  "React SPA with root div and bundle",
			html:  `<!DOCTYPE html><html><head></head><body><div id="root"></div><script src="/static/js/react-main.js"></script></body></html>`,
			isSPA: true,
		},
		{
			name:  "Next.js SPA",
			html:  `<!DOCTYPE html><html><head></head><body><div id="__next"></div><noscript>You need to enable JavaScript to run this app.</noscript></body></html>`,
			isSPA: true,
		},
		{
			name:  "SPA with empty app div and low text ratio",
			html:  `<!DOCTYPE html><html><head><meta charset="utf-8"><title>App</title></head><body><div id="app"></div><script src="/chunk-vendors.js"></script></body></html>`,
			isSPA: true,
		},
		{
			name:  "normal static HTML page",
			html:  `<!DOCTYPE html><html><head><title>Docs</title></head><body><h1>Documentation</h1><p>This is a normal static page with real content.</p><ul><li>Item 1</li><li>Item 2</li></ul></body></html>`,
			isSPA: false,
		},
		{
			name:  "empty body but no SPA markers",
			html:  `<!DOCTYPE html><html><head><title>Blank</title></head><body></body></html>`,
			isSPA: false,
		},
		{
			name:  "page with script but real content",
			html:  `<!DOCTYPE html><html><head><title>Site</title></head><body><h1>Welcome</h1><p>Real content here.</p><script src="/analytics.js"></script></body></html>`,
			isSPA: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLikelySPA([]byte(tt.html))
			if got != tt.isSPA {
				t.Errorf("isLikelySPA(%q) = %v, want %v", tt.name, got, tt.isSPA)
			}
		})
	}
}

func TestBoolSetting(t *testing.T) {
	tests := []struct {
		input any
		want  bool
	}{
		{true, true},
		{false, false},
		{"true", true},
		{"True", true},
		{"false", false},
		{"1", false},
		{float64(1), true},
		{float64(0), false},
		{nil, false},
	}
	for _, tt := range tests {
		got := boolSetting(tt.input)
		if got != tt.want {
			t.Errorf("boolSetting(%v) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseConfigIsSPA(t *testing.T) {
	cfg, err := parseConfig(`{"is_spa": true, "max_pages": "50"}`)
	if err != nil {
		t.Fatalf("parseConfig error: %v", err)
	}
	if !cfg.IsSPA {
		t.Errorf("IsSPA = %v, want true", cfg.IsSPA)
	}
	if cfg.MaxPages != 50 {
		t.Errorf("MaxPages = %d, want 50", cfg.MaxPages)
	}

	cfg2, err := parseConfig(`{"max_pages": "10"}`)
	if err != nil {
		t.Fatalf("parseConfig error: %v", err)
	}
	if cfg2.IsSPA {
		t.Errorf("IsSPA = %v, want false (default)", cfg2.IsSPA)
	}
}

func TestExtractHashRouteLinks(t *testing.T) {
	// Test with a SPA that uses /base/#/path hash routing.
	seed, _ := url.Parse("https://spa.example/")
	pageURL, _ := url.Parse("https://spa.example/app/home/")
	scope := scopePath(seed.Path) // scope is "/" so all paths are allowed

	spaHTML := `<!DOCTYPE html><html><head></head><body>
		<div id="app">
			<nav>
				<a href="/app/home/#/delivery-materials">产品文档</a>
				<a href="/app/home/#/release-notes">版本发布说明</a>
				<a href="/app/home/#/dev-docs">设计文档</a>
				<a href="#/guide">Guide (relative hash)</a>
				<a href="#contact">Contact (non-route hash)</a>
			</nav>
			<h1>Welcome</h1>
		</div>
	</body></html>`

	links := extractHashRouteLinks([]byte(spaHTML), pageURL, seed, scope)

	// Should find 4 hash-routed links (fragment starts with /), not #contact
	if len(links) != 4 {
		t.Fatalf("extractHashRouteLinks returned %d links, want 4: %+v", len(links), links)
	}

	// Check that the URLs are converted to real paths (sorted alphabetically)
	expectedPaths := []string{
		"https://spa.example/delivery-materials",
		"https://spa.example/dev-docs",
		"https://spa.example/guide",
		"https://spa.example/release-notes",
	}
	for i, link := range links {
		if link.URL != expectedPaths[i] {
			t.Errorf("link[%d].URL = %q, want %q", i, link.URL, expectedPaths[i])
		}
	}
}

func TestMergeLinks(t *testing.T) {
	a := []rawLink{rawLink{URL: "http://a.com", Fragment: "sec1", Text: "A"}}
	b := []rawLink{rawLink{URL: "http://b.com", Fragment: "", Text: "B"}, rawLink{URL: "http://a.com", Fragment: "sec1", Text: "A duplicate"}}
	result := mergeLinks(a, b)
	if len(result) != 2 {
		t.Errorf("mergeLinks returned %d links, want 2 (deduped)", len(result))
	}
}

func TestDetectSPABasePath(t *testing.T) {
	// SPA with /app/home/ base path.
	appHTML := `<!DOCTYPE html><html><head></head><body>
		<div id="app">
			<nav>
				<a href="/app/home/#/delivery-materials">产品文档</a>
				<a href="/app/home/#/release-notes">版本说明</a>
				<a href="/app/home/#/dev-docs">设计文档</a>
			</nav>
		</div>
	</body></html>`
	base := detectSPABasePath([]byte(appHTML))
	if base != "/app/home/" {
		t.Errorf("detectSPABasePath(app) = %q, want /app/home/", base)
	}

	// SPA with <base> tag
	baseTagHTML := `<!DOCTYPE html><html><head><base href="/docs/"></head><body><div id="app"></div></body></html>`
	base2 := detectSPABasePath([]byte(baseTagHTML))
	if base2 != "/docs/" {
		t.Errorf("detectSPABasePath(baseTag) = %q, want /docs/", base2)
	}

	// No base path detected → returns "/"
	simpleHTML := `<!DOCTYPE html><html><head></head><body><h1>Hello</h1><p>World</p></body></html>`
	base3 := detectSPABasePath([]byte(simpleHTML))
	if base3 != "/" {
		t.Errorf("detectSPABasePath(simple) = %q, want /", base3)
	}
}
