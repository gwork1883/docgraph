package webdocs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docgraph/docgraph/internal/ingest/htmldocs"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"golang.org/x/net/html"
)

type Config struct {
	MaxPages    int               `json:"max_pages"`
	MaxDepth    int               `json:"max_depth"`
	BearerToken string            `json:"bearer_token"`
	Cookie      string            `json:"cookie"`
	Headers     map[string]string `json:"headers"`
	HeadersJSON string            `json:"headers_json"`
	IsSPA       bool              `json:"is_spa"`
}

type rawLink struct {
	URL      string
	Fragment string
	Text     string
}

type crawledPage struct {
	URL     *url.URL
	Depth   int
	Content []byte
	Links   []rawLink
	Doc     htmldocs.Document
}

type Client struct {
	seed        *url.URL
	scopePath   string
	httpClient  *http.Client
	config      Config
	spaPageBase string // base path of the SPA page (e.g. "/app/home/") for hash routing
}

var httpClientFactory = struct {
	sync.RWMutex
	new func() *http.Client
}{
	new: func() *http.Client {
		return &http.Client{Timeout: 15 * time.Second}
	},
}

func Load(ctx context.Context, dsn string, configJSON string) ([]htmldocs.Document, error) {
	client, err := NewClient(dsn, configJSON)
	if err != nil {
		return nil, err
	}
	return client.Load(ctx)
}

func NewClient(dsn string, configJSON string) (*Client, error) {
	seed, err := parseSeedURL(dsn)
	if err != nil {
		return nil, err
	}
	cfg, err := parseConfig(configJSON)
	if err != nil {
		return nil, err
	}
	return &Client{
		seed:       seed,
		scopePath:  scopePath(seed.Path),
		httpClient: newHTTPClient(),
		config:     cfg,
	}, nil
}

func SetHTTPClientFactoryForTest(factory func() *http.Client) func() {
	httpClientFactory.Lock()
	previous := httpClientFactory.new
	httpClientFactory.new = factory
	httpClientFactory.Unlock()

	return func() {
		httpClientFactory.Lock()
		httpClientFactory.new = previous
		httpClientFactory.Unlock()
	}
}

func newHTTPClient() *http.Client {
	httpClientFactory.RLock()
	factory := httpClientFactory.new
	httpClientFactory.RUnlock()
	return factory()
}

func (c *Client) Load(ctx context.Context) ([]htmldocs.Document, error) {
	pages, err := c.crawl(ctx)
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("webdocs crawl found no HTML pages under %s", c.seed.String())
	}

	pathsByURL := make(map[string]string, len(pages))
	for _, page := range pages {
		pathsByURL[canonicalPageURL(page.URL).String()] = page.Doc.Path
	}

	docs := make([]htmldocs.Document, 0, len(pages))
	for _, page := range pages {
		doc := page.Doc
		for i := range doc.Sections {
			doc.Sections[i].Links = nil
		}
		if len(doc.Sections) > 0 {
			doc.Sections[0].Links = normalizedLinks(doc.Path, page.Links, pathsByURL)
		}
		docs = append(docs, doc)
	}
	sort.Slice(docs, func(i, j int) bool {
		return docs[i].Path < docs[j].Path
	})
	return docs, nil
}

func (c *Client) crawl(ctx context.Context) ([]crawledPage, error) {
	type queueItem struct {
		url   *url.URL
		depth int
	}
	queue := []queueItem{{url: c.seed, depth: 0}}
	queued := map[string]bool{canonicalPageURL(c.seed).String(): true}
	visited := map[string]bool{}
	pages := make([]crawledPage, 0)

	// Determine whether we need browser rendering.
	// If is_spa is explicitly set, use browser mode directly.
	// Otherwise, auto-detect: fetch the seed page via HTTP, check SPA markers,
	// and switch to browser mode if detected.
	useBrowser := c.config.IsSPA
	var browser *rod.Browser
	var launcherInst *launcher.Launcher

	if useBrowser {
		b, l, err := launchBrowser(ctx)
		if err != nil {
			return nil, err
		}
		browser = b
		launcherInst = l
	}

	for len(queue) > 0 {
		if c.config.MaxPages > 0 && len(pages) >= c.config.MaxPages {
			break
		}
		item := queue[0]
		queue = queue[1:]
		key := canonicalPageURL(item.url).String()
		if visited[key] {
			continue
		}
		visited[key] = true

		var content []byte
		var err error

		// For hash-routed SPA pages, compute the real browser URL.
		// The target URL is a virtual path (e.g. /delivery-materials) but
		// the browser needs the actual SPA URL with hash fragment
		// (e.g. /app/home/#/delivery-materials).
		browserPath := ""
		if useBrowser && c.spaPageBase != "" && item.url.Path != c.spaPageBase {
			// This target was derived from a hash route — navigate the browser
			// to the SPA base path with the hash fragment appended.
			routePath := strings.TrimPrefix(item.url.Path, "/")
			browserPath = c.seed.Scheme + "://" + c.seed.Host + c.spaPageBase + "#/" + routePath
		}

		if useBrowser {
			content, err = c.fetchWithBrowser(ctx, browser, item.url, browserPath)
		} else {
			content, err = c.fetch(ctx, item.url)
		}
		if err != nil {
			if item.depth == 0 {
				if browser != nil {
					browser.MustClose()
					launcherInst.Cleanup()
				}
				return nil, err
			}
			continue
		}

		// After the first browser fetch, determine the SPA page base path
		// from the seed URL's path. This is where the SPA shell lives.
		if useBrowser && c.spaPageBase == "" && item.depth == 0 {
			c.spaPageBase = c.seed.Path
			if c.spaPageBase == "" || c.spaPageBase == "/" {
				// If the seed is at root, the SPA shell might be at a different path.
				// Try to detect from the fetched HTML: look for <base href> or
				// the common pattern where links start with a specific base path.
				c.spaPageBase = detectSPABasePath(content)
			}
		}

		// Auto-detect SPA: if is_spa was not explicitly set and the seed page
		// looks like a SPA, switch to browser mode and re-fetch.
		if !useBrowser && item.depth == 0 && isLikelySPA(content) {
			useBrowser = true
			b, l, err := launchBrowser(ctx)
			if err != nil {
				// SPA detected but no browser available — proceed with
				// the empty-shell HTML; the user will see zero sections.
				useBrowser = false
			} else {
				browser = b
				launcherInst = l
				content, err = c.fetchWithBrowser(ctx, browser, item.url, "")
				if err != nil {
					browser.MustClose()
					launcherInst.Cleanup()
					return nil, err
				}
			}
		}

		links := extractLinks(content, item.url, c.seed, c.scopePath)
		// In browser (SPA) mode, also discover hash-routed links from the rendered DOM.
		// Many SPAs use #/path routing where the fragment acts as a virtual path.
		if useBrowser {
			hashLinks := extractHashRouteLinks(content, item.url, c.seed, c.scopePath)
			links = mergeLinks(links, hashLinks)
		}
		docPath := docPathForURL(c.scopePath, item.url)
		doc := htmldocs.Parse(docPath, content)
		doc.URL = item.url.String()
		doc.Hash = hashBytes(content)
		pages = append(pages, crawledPage{
			URL:     canonicalPageURL(item.url),
			Depth:   item.depth,
			Content: content,
			Links:   links,
			Doc:     doc,
		})

		if c.config.MaxDepth > 0 && item.depth >= c.config.MaxDepth {
			continue
		}
		for _, link := range links {
			nextURL, err := url.Parse(link.URL)
			if err != nil {
				continue
			}
			// For hash-routed SPA links (#/path), convert the fragment to a
			// real URL so the crawler can fetch it as a distinct page.
			if strings.HasPrefix(nextURL.Fragment, "/") && useBrowser {
				hashURL := *item.url // base on current page host
				hashURL.Path = nextURL.Fragment
				hashURL.Fragment = ""
				hashURL.RawQuery = ""
				nextURL = canonicalPageURL(&hashURL)
			} else {
				nextURL = canonicalPageURL(nextURL)
			}
			nextKey := nextURL.String()
			if queued[nextKey] || visited[nextKey] {
				continue
			}
			queued[nextKey] = true
			queue = append(queue, queueItem{url: nextURL, depth: item.depth + 1})
		}
	}

	// Clean up browser instance
	if browser != nil {
		browser.MustClose()
		launcherInst.Cleanup()
	}

	return pages, nil
}

// launchBrowser finds a local Chrome/Chromium/Edge binary via rod's LookPath,
// launches a headless instance, and returns the connected browser.
// No auto-download is performed — if no browser is found, an error is returned.
func launchBrowser(ctx context.Context) (*rod.Browser, *launcher.Launcher, error) {
	bin, has := launcher.LookPath()
	if !has {
		return nil, nil, fmt.Errorf(
			"is_spa mode requires Chrome, Chromium, or Edge installed locally; " +
				"none found. Install a browser or set the ROD_BROWSER_BIN env variable")
	}
	l := launcher.New().Bin(bin).Headless(true).NoSandbox(true)
	l.Context(ctx)
	debugURL, err := l.Launch()
	if err != nil {
		return nil, nil, fmt.Errorf("launch headless browser: %w", err)
	}
	browser := rod.New().ControlURL(debugURL)
	if err := browser.Connect(); err != nil {
		l.Kill()
		return nil, nil, fmt.Errorf("connect to headless browser: %w", err)
	}
	return browser, l, nil
}

// fetchWithBrowser renders a page using the headless browser and returns the
// final HTML after JavaScript execution completes.
// If browserPath is non-empty, the browser navigates to that URL instead of target
// (used for hash-routed SPA pages where target is a virtual path but the browser
// needs the real SPA base URL with a hash fragment).
func (c *Client) fetchWithBrowser(ctx context.Context, browser *rod.Browser, target *url.URL, browserPath string) ([]byte, error) {
	page, err := browser.Context(ctx).Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, fmt.Errorf("browser create page for %s: %w", target.String(), err)
	}
	defer page.Close()

	// Set cookies before navigation so they are sent with the first request
	if strings.TrimSpace(c.config.Cookie) != "" {
		cookies := parseCookieHeader(target, strings.TrimSpace(c.config.Cookie))
		if err := page.SetCookies(cookies); err != nil {
			return nil, fmt.Errorf("browser set cookies for %s: %w", target.String(), err)
		}
	}

	// Set extra HTTP headers (bearer token, custom headers) before navigation
	extraHeaders := []string{}
	if strings.TrimSpace(c.config.BearerToken) != "" {
		extraHeaders = append(extraHeaders, "Authorization", "Bearer "+strings.TrimSpace(c.config.BearerToken))
	}
	for key, value := range c.config.Headers {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			extraHeaders = append(extraHeaders, key, value)
		}
	}
	if len(extraHeaders) > 0 {
		if _, err := page.SetExtraHeaders(extraHeaders); err != nil {
			return nil, fmt.Errorf("browser set extra headers for %s: %w", target.String(), err)
		}
	}

	// Navigate to the target URL (or override with browserPath for hash-routed SPA)
	navigateURL := target.String()
	if browserPath != "" {
		navigateURL = browserPath
	}
	if err := page.Navigate(navigateURL); err != nil {
		return nil, fmt.Errorf("browser navigate to %s: %w", navigateURL, err)
	}

	// Wait for the page to stabilize (DOM mutations settle).
	if err := page.WaitStable(2 * time.Second); err != nil {
		return nil, fmt.Errorf("browser wait stable %s: %w", target.String(), err)
	}

	htmlStr, err := page.HTML()
	if err != nil {
		return nil, fmt.Errorf("browser get HTML %s: %w", target.String(), err)
	}
	return []byte(htmlStr), nil
}

// parseCookieHeader converts a raw Cookie header string (e.g. "key1=val1; key2=val2")
// into rod network cookie params for the given target URL.
func parseCookieHeader(target *url.URL, cookieHeader string) []*proto.NetworkCookieParam {
	cookies := []*proto.NetworkCookieParam{}
	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			name = part
			value = ""
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		cookies = append(cookies, &proto.NetworkCookieParam{
			Name:   name,
			Value:  value,
			Domain: target.Hostname(),
			URL:    target.String(),
		})
	}
	return cookies
}

// isLikelySPA examines raw HTML content for common SPA indicators:
//   - A near-empty body with a root mount point (<div id="app">, <div id="root">)
//   - A <noscript> fallback message mentioning JavaScript
//   - Large JS bundle references (vue, react, angular) in script tags
//   - Very low visible text-to-HTML ratio
func isLikelySPA(content []byte) bool {
	root, err := html.Parse(strings.NewReader(string(content)))
	if err != nil {
		return false
	}

	var hasSPARootDiv bool    // <div id="app"> or <div id="root">
	var hasNoscriptJSMsg bool // <noscript> mentions "javascript" or "JavaScript enabled"
	var hasLargeJSBundle bool // script src contains vue/react/angular/webpack
	var visibleTextLen int
	var totalHTMLLen = len(content)

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script":
				src := strings.ToLower(attr(n, "src"))
				if strings.Contains(src, "vue") ||
					strings.Contains(src, "react") ||
					strings.Contains(src, "angular") ||
					strings.Contains(src, "webpack") ||
					strings.Contains(src, "chunk-vendors") ||
					strings.Contains(src, "app.") &&
						(strings.Contains(src, ".js") || strings.Contains(src, ".mjs")) {
					hasLargeJSBundle = true
				}
				// Don't count script text content as visible text
				return
			case "noscript":
				text := strings.ToLower(textContent(n))
				if strings.Contains(text, "javascript") || strings.Contains(text, "js") {
					hasNoscriptJSMsg = true
				}
				return
			case "style":
				// Don't count style text content as visible text
				return
			case "div":
				id := strings.ToLower(attr(n, "id"))
				if id == "app" || id == "root" || id == "__next" || id == "__nuxt" {
					hasSPARootDiv = true
				}
				// Fall through to count visible text inside the div
			case "p", "li", "h1", "h2", "h3", "h4", "h5", "h6", "span", "a", "td", "th":
				text := strings.TrimSpace(textContent(n))
				visibleTextLen += len(text)
			}
		}
		if n.Type == html.TextNode {
			// Only count text not inside script/style (those are skipped above)
			text := strings.TrimSpace(n.Data)
			visibleTextLen += len(text)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)

	// SPA detection: any of these strong indicators is sufficient
	if hasSPARootDiv && hasNoscriptJSMsg {
		return true
	}
	if hasSPARootDiv && hasLargeJSBundle {
		return true
	}
	if hasNoscriptJSMsg && hasLargeJSBundle {
		return true
	}

	// Weak indicator: root div exists and visible text ratio is very low
	if hasSPARootDiv && totalHTMLLen > 500 && visibleTextLen < 50 {
		return true
	}

	return false
}

// detectSPABasePath analyzes the HTML to find the SPA's base path.
// It looks for common patterns like:
// - <base href="/app/home/"> tags
// - Hash-routed links starting with a common prefix (e.g. /app/home/#/)
// Returns "/" if no specific base path is found.
func detectSPABasePath(content []byte) string {
	root, err := html.Parse(strings.NewReader(string(content)))
	if err != nil {
		return "/"
	}

	// Check for <base href="/path/"> tag
	var baseHref string
	var findBase func(*html.Node)
	findBase = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "base" {
			href := strings.TrimSpace(attr(n, "href"))
			if href != "" {
				baseHref = href
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			findBase(child)
		}
	}
	findBase(root)

	if baseHref != "" {
		return cleanURLPath(baseHref)
	}

	// Check hash-routed links to find a common path prefix
	// e.g. if all links are /app/home/#/xxx, the base is /app/home/
	linkPaths := []string{}
	var findLinks func(*html.Node)
	findLinks = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := strings.TrimSpace(attr(n, "href"))
			parsed, err := url.Parse(href)
			if err == nil && strings.HasPrefix(parsed.Fragment, "/") && parsed.Path != "" && parsed.Path != "/" {
				linkPaths = append(linkPaths, cleanURLPath(parsed.Path))
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			findLinks(child)
		}
	}
	findLinks(root)

	if len(linkPaths) >= 2 {
		// Find the longest common prefix among all link paths
		prefix := linkPaths[0]
		for _, p := range linkPaths[1:] {
			for !strings.HasPrefix(p, prefix) || (prefix != "/" && !strings.HasPrefix(p+"/", prefix+"/") && p != prefix) {
				prefix = prefix[:strings.LastIndex(prefix, "/")]
				if prefix == "" {
					prefix = "/"
					break
				}
			}
		}
		if prefix != "/" {
			return prefix
		}
	}

	return "/"
}

func (c *Client) fetch(ctx context.Context, target *url.URL) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", "DocGraph-WebDocs/1.0")
	for key, value := range c.config.Headers {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			req.Header.Set(key, value)
		}
	}
	if strings.TrimSpace(c.config.BearerToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.config.BearerToken))
	}
	if strings.TrimSpace(c.config.Cookie) != "" {
		req.Header.Set("Cookie", strings.TrimSpace(c.config.Cookie))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch webdocs page %s: %w", target.String(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch webdocs page %s failed: HTTP %d", target.String(), resp.StatusCode)
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "text/html") && !strings.Contains(contentType, "application/xhtml+xml") {
		return nil, fmt.Errorf("fetch webdocs page %s returned non-HTML content type %q", target.String(), contentType)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read webdocs page %s: %w", target.String(), err)
	}
	return body, nil
}

func extractLinks(content []byte, pageURL *url.URL, seed *url.URL, scope string) []rawLink {
	root, err := html.Parse(strings.NewReader(string(content)))
	if err != nil {
		return nil
	}
	seen := map[string]rawLink{}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "a" {
			href := strings.TrimSpace(attr(node, "href"))
			if target, fragment, ok := resolveLink(pageURL, seed, scope, href); ok {
				key := target + "#" + fragment
				seen[key] = rawLink{URL: target, Fragment: fragment, Text: strings.TrimSpace(textContent(node))}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)

	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	links := make([]rawLink, 0, len(keys))
	for _, key := range keys {
		links = append(links, seen[key])
	}
	return links
}

func resolveLink(pageURL *url.URL, seed *url.URL, scope string, href string) (string, string, bool) {
	href = strings.TrimSpace(href)
	if href == "" {
		return "", "", false
	}
	lower := strings.ToLower(href)
	if strings.HasPrefix(lower, "mailto:") || strings.HasPrefix(lower, "tel:") || strings.HasPrefix(lower, "javascript:") {
		return "", "", false
	}
	target, err := pageURL.Parse(href)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return "", "", false
	}
	if !strings.EqualFold(target.Host, seed.Host) || !sameScopePath(target.Path, scope) {
		return "", "", false
	}
	fragment := target.Fragment
	target.Fragment = ""
	// For hash-routed SPA links where fragment starts with "/" (e.g. #/delivery-materials),
	// convert the fragment into a virtual path so it becomes a distinct URL.
	// This is handled separately by extractHashRouteLinks, so we skip these here
	// to avoid duplicate link entries.
	if strings.HasPrefix(fragment, "/") {
		return "", "", false
	}
	return canonicalPageURL(target).String(), fragment, true
}

// extractHashRouteLinks finds <a href="#/path"> style links in SPA pages.
// These are hash-routed navigation links where the fragment starting with "/"
// represents a virtual path within the SPA. We convert them into real URLs
// so the crawler can discover and fetch them as distinct pages.
func extractHashRouteLinks(content []byte, pageURL *url.URL, seed *url.URL, scope string) []rawLink {
	root, err := html.Parse(strings.NewReader(string(content)))
	if err != nil {
		return nil
	}
	seen := map[string]rawLink{}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "a" {
			href := strings.TrimSpace(attr(node, "href"))
			// Match hash-routed links. Two patterns:
			// 1. Relative: #/path (e.g. #/guide)
			// 2. Absolute with hash: /some/base#/path (e.g. /app/home/#/delivery-materials)
			parsed, err := pageURL.Parse(href)
			if err == nil && strings.HasPrefix(parsed.Fragment, "/") {
				// This is a hash-routed SPA link: the fragment starting with "/"
				// is a virtual route path. Convert it to a real URL path.
				virtualPath := parsed.Fragment // e.g. "/delivery-materials"
				virtualURL := *seed            // base on seed for host/scheme
				virtualURL.Path = virtualPath
				virtualURL.Fragment = ""
				virtualURL.RawQuery = ""
				if !strings.EqualFold(virtualURL.Host, seed.Host) || !sameScopePath(virtualURL.Path, scope) {
					return
				}
				key := canonicalPageURL(&virtualURL).String()
				seen[key] = rawLink{
					URL:      key,
					Fragment: virtualPath,
					Text:     strings.TrimSpace(textContent(node)),
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)

	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	links := make([]rawLink, 0, len(keys))
	for _, key := range keys {
		links = append(links, seen[key])
	}
	return links
}

// mergeLinks combines two link lists, deduplicating by URL+Fragment.
func mergeLinks(a, b []rawLink) []rawLink {
	seen := map[string]bool{}
	out := make([]rawLink, 0, len(a)+len(b))
	for _, link := range a {
		key := link.URL + "#" + link.Fragment
		if !seen[key] {
			seen[key] = true
			out = append(out, link)
		}
	}
	for _, link := range b {
		key := link.URL + "#" + link.Fragment
		if !seen[key] {
			seen[key] = true
			out = append(out, link)
		}
	}
	return out
}

func normalizedLinks(sourceDocPath string, links []rawLink, pathsByURL map[string]string) []htmldocs.Link {
	seen := map[string]htmldocs.Link{}
	sourceDir := filepath.Dir(filepath.FromSlash(sourceDocPath))
	if sourceDir == "." {
		sourceDir = ""
	}
	for _, link := range links {
		targetPath := pathsByURL[link.URL]
		if targetPath == "" {
			continue
		}
		rel, err := filepath.Rel(sourceDir, filepath.FromSlash(targetPath))
		if err != nil {
			rel = filepath.FromSlash(targetPath)
		}
		href := filepath.ToSlash(rel)
		if link.Fragment != "" {
			href += "#" + link.Fragment
		}
		seen[href] = htmldocs.Link{Href: href, Text: link.Text}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]htmldocs.Link, 0, len(keys))
	for _, key := range keys {
		out = append(out, seen[key])
	}
	return out
}

func parseSeedURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("webdocs dsn is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid webdocs URL %q", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported webdocs URL scheme %q", parsed.Scheme)
	}
	parsed.Fragment = ""
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return canonicalPageURL(parsed), nil
}

func parseConfig(configJSON string) (Config, error) {
	configJSON = strings.TrimSpace(configJSON)
	if configJSON == "" {
		return Config{}, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(configJSON), &raw); err != nil {
		return Config{}, fmt.Errorf("parse webdocs config_json: %w", err)
	}
	headers, err := headersSetting(raw["headers"], stringSetting(raw["headers_json"]))
	if err != nil {
		return Config{}, err
	}
	return Config{
		MaxPages:    intSetting(raw["max_pages"]),
		MaxDepth:    intSetting(raw["max_depth"]),
		BearerToken: stringSetting(raw["bearer_token"]),
		Cookie:      stringSetting(raw["cookie"]),
		Headers:     headers,
		HeadersJSON: stringSetting(raw["headers_json"]),
		IsSPA:       boolSetting(raw["is_spa"]),
	}, nil
}

func intSetting(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(typed))
		return parsed
	default:
		return 0
	}
}

func stringSetting(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return ""
	}
}

func boolSetting(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.TrimSpace(strings.ToLower(typed)) == "true"
	case float64:
		return typed != 0
	default:
		return false
	}
}

func headersSetting(value any, headersJSON string) (map[string]string, error) {
	headers := map[string]string{}
	if raw, ok := value.(map[string]any); ok {
		for key, value := range raw {
			if text, ok := value.(string); ok && strings.TrimSpace(key) != "" && strings.TrimSpace(text) != "" {
				headers[strings.TrimSpace(key)] = strings.TrimSpace(text)
			}
		}
	}
	if strings.TrimSpace(headersJSON) != "" {
		var raw map[string]string
		if err := json.Unmarshal([]byte(headersJSON), &raw); err != nil {
			return nil, fmt.Errorf("parse webdocs headers_json: %w", err)
		}
		for key, value := range raw {
			if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
				headers[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
	}
	return headers, nil
}

func canonicalPageURL(value *url.URL) *url.URL {
	clone := *value
	clone.Fragment = ""
	clone.RawQuery = ""
	if clone.Path == "" {
		clone.Path = "/"
	}
	clone.Path = cleanURLPath(clone.Path)
	return &clone
}

func cleanURLPath(value string) string {
	if value == "" {
		return "/"
	}
	cleaned := path.Clean("/" + strings.TrimLeft(value, "/"))
	if strings.HasSuffix(value, "/") && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned
}

func scopePath(value string) string {
	value = cleanURLPath(value)
	if value == "/" {
		return "/"
	}
	return strings.TrimRight(value, "/")
}

func sameScopePath(value string, scope string) bool {
	value = cleanURLPath(value)
	if scope == "/" {
		return true
	}
	return value == scope || strings.HasPrefix(value, strings.TrimRight(scope, "/")+"/")
}

func docPathForURL(scope string, value *url.URL) string {
	cleaned := strings.TrimPrefix(cleanURLPath(value.Path), strings.TrimRight(scope, "/"))
	cleaned = strings.Trim(cleaned, "/")
	if cleaned == "" {
		return "index.html"
	}
	if strings.Contains(path.Base(cleaned), ".") {
		return cleaned
	}
	return strings.TrimRight(cleaned, "/") + "/index.html"
}

func attr(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func textContent(node *html.Node) string {
	var parts []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				parts = append(parts, text)
			}
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(parts, " ")
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
