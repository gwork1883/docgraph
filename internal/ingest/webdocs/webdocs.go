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
	MaxPages           int               `json:"max_pages"`
	MaxDepth           int               `json:"max_depth"`
	BearerToken        string            `json:"bearer_token"`
	Cookie             string            `json:"cookie"`
	Headers            map[string]string `json:"headers"`
	HeadersJSON        string            `json:"headers_json"`
	CrawlMode          string            `json:"crawl_mode"`
	IsSPA              bool              `json:"is_spa"`
	PageTimeoutSeconds int               `json:"page_timeout_seconds"`
}

type Progress struct {
	Phase   string
	URL     string
	Visited int
	Queued  int
	Depth   int
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

type inPageCandidate struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

type contentSnapshot struct {
	Title string `json:"title"`
	HTML  string `json:"html"`
	Text  string `json:"text"`
}

type Client struct {
	seed        *url.URL
	scopePath   string
	httpClient  *http.Client
	config      Config
	spaPageBase string // base path of the SPA page (e.g. "/app/home/") for hash routing
	progress    func(Progress)
}

var httpClientFactory = struct {
	sync.RWMutex
	new func() *http.Client
}{
	new: func() *http.Client {
		return &http.Client{Timeout: 15 * time.Second}
	},
}

const (
	maxInPageCandidates       = 160
	maxInPageNoProgressClicks = 32
	minInPageContentTextRunes = 40
	inPageClickStableWait     = 500 * time.Millisecond
)

type inPageTraversalQueue struct {
	seen       map[string]bool
	queued     map[string]bool
	queue      []inPageCandidate
	processed  int
	noProgress int
}

func newInPageTraversalQueue() *inPageTraversalQueue {
	return &inPageTraversalQueue{
		seen:   map[string]bool{},
		queued: map[string]bool{},
		queue:  make([]inPageCandidate, 0),
	}
}

func (q *inPageTraversalQueue) enqueue(candidates []inPageCandidate) int {
	added := 0
	for _, candidate := range candidates {
		if candidate.Key == "" || q.seen[candidate.Key] || q.queued[candidate.Key] {
			continue
		}
		q.queued[candidate.Key] = true
		q.queue = append(q.queue, candidate)
		added++
		if q.processed+len(q.queue) >= maxInPageCandidates {
			break
		}
	}
	return added
}

func (q *inPageTraversalQueue) next() (inPageCandidate, bool) {
	for len(q.queue) > 0 {
		next := q.queue[0]
		q.queue = q.queue[1:]
		delete(q.queued, next.Key)
		if q.seen[next.Key] {
			continue
		}
		q.seen[next.Key] = true
		q.processed++
		return next, true
	}
	return inPageCandidate{}, false
}

func (q *inPageTraversalQueue) recordProgress(progress bool) {
	if progress {
		q.noProgress = 0
		return
	}
	q.noProgress++
}

func (q *inPageTraversalQueue) canContinue(docCount int, limit int) bool {
	return len(q.queue) > 0 &&
		docCount < limit &&
		q.processed < maxInPageCandidates &&
		q.noProgress < maxInPageNoProgressClicks
}

func (q *inPageTraversalQueue) queuedLen() int {
	return len(q.queue)
}

func Load(ctx context.Context, dsn string, configJSON string) ([]htmldocs.Document, error) {
	client, err := NewClient(dsn, configJSON)
	if err != nil {
		return nil, err
	}
	return client.Load(ctx)
}

func LoadWithProgress(ctx context.Context, dsn string, configJSON string, progress func(Progress)) ([]htmldocs.Document, error) {
	client, err := NewClient(dsn, configJSON)
	if err != nil {
		return nil, err
	}
	client.progress = progress
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
	// Existing sources without crawl_mode stay in auto mode; legacy is_spa=true
	// still forces browser rendering.
	useBrowser := c.config.IsSPA || c.config.CrawlMode == "browser" || c.config.CrawlMode == "in_page"
	var browser *rod.Browser
	var launcherInst *launcher.Launcher
	cleanupBrowser := func() {
		if browser != nil {
			browser.MustClose()
			browser = nil
		}
		if launcherInst != nil {
			launcherInst.Cleanup()
			launcherInst = nil
		}
	}
	defer cleanupBrowser()

	if useBrowser {
		b, l, err := launchBrowser(ctx)
		if err != nil {
			return nil, err
		}
		browser = b
		launcherInst = l
	}

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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

		c.report(Progress{Phase: "fetching", URL: item.url.String(), Visited: len(pages), Queued: len(queue) + 1, Depth: item.depth})
		if useBrowser {
			content, err = c.fetchWithBrowser(ctx, browser, item.url, browserPath)
		} else {
			content, err = c.fetch(ctx, item.url)
		}
		if err != nil {
			if ctx.Err() != nil {
				cleanupBrowser()
				return nil, ctx.Err()
			}
			if item.depth == 0 {
				cleanupBrowser()
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

		// Auto-detect SPA: if browser mode was not explicitly set and the seed page
		// looks like a SPA, switch to browser mode and re-fetch.
		if !useBrowser && c.config.CrawlMode != "static" && item.depth == 0 && isLikelySPA(content) {
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
					cleanupBrowser()
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
		c.report(Progress{Phase: "parsed", URL: item.url.String(), Visited: len(pages), Queued: len(queue), Depth: item.depth})

		if useBrowser && item.depth == 0 && c.inPageNavigationEnabled() && len(links) <= 2 {
			inPageDocs, err := c.discoverInPageDocuments(ctx, browser, item.url, browserPath, content)
			if len(inPageDocs) > 0 {
				pages = pages[:len(pages)-1]
				pages = append(pages, inPageDocs...)
				c.report(Progress{Phase: "in_page", URL: item.url.String(), Visited: len(pages), Queued: len(queue), Depth: item.depth})
				continue
			}
			if err != nil && c.config.CrawlMode == "in_page" {
				cleanupBrowser()
				return nil, err
			}
		}

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

	return pages, nil
}

func (c *Client) report(progress Progress) {
	if c.progress != nil {
		c.progress(progress)
	}
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
	pageCtx, cancel := context.WithTimeout(ctx, c.pageTimeout())
	defer cancel()
	page, err := browser.Context(pageCtx).Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, fmt.Errorf("browser create page for %s: %w", target.String(), err)
	}
	defer page.Close()

	if err := c.prepareBrowserPage(page, target); err != nil {
		return nil, err
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

func htmlDocument(title string, innerHTML string) []byte {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Document"
	}
	return []byte("<!doctype html><html><head><meta charset=\"utf-8\"><title>" + htmlEscape(title) + "</title></head><body><main><h1>" + htmlEscape(title) + "</h1>" + innerHTML + "</main></body></html>")
}

func htmlEscape(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	value = strings.ReplaceAll(value, `"`, "&quot;")
	return value
}

func normalizedContentText(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func virtualDocPath(basePath string, title string, key string) string {
	slug := slugify(title)
	if slug == "" {
		slug = slugify(key)
	}
	if slug == "" {
		slug = hashString(key)[:12]
	}
	base := strings.TrimSuffix(basePath, ".html")
	base = strings.TrimSuffix(base, "/index")
	base = strings.Trim(base, "/")
	if base == "" {
		base = "index"
	}
	return path.Join(base, slug+".html")
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r >= 0x4e00 && r <= 0x9fff:
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func (c *Client) inPageNavigationEnabled() bool {
	return c.config.CrawlMode == "" || c.config.CrawlMode == "auto" || c.config.CrawlMode == "browser" || c.config.CrawlMode == "in_page" || c.config.IsSPA
}

func (c *Client) discoverInPageDocuments(ctx context.Context, browser *rod.Browser, target *url.URL, browserPath string, seedContent []byte) ([]crawledPage, error) {
	if browser == nil {
		return nil, nil
	}
	page, err := browser.Context(ctx).Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, fmt.Errorf("browser create in-page discovery page for %s: %w", target.String(), err)
	}
	defer page.Close()

	if err := c.prepareBrowserPage(page, target); err != nil {
		return nil, err
	}
	navigateURL := target.String()
	if browserPath != "" {
		navigateURL = browserPath
	}
	navPage := page.Timeout(c.pageTimeout())
	if err := navPage.Navigate(navigateURL); err != nil {
		navPage.CancelTimeout()
		return nil, fmt.Errorf("browser navigate to %s for in-page discovery: %w", navigateURL, err)
	}
	if err := navPage.WaitStable(2 * time.Second); err != nil {
		navPage.CancelTimeout()
		return nil, fmt.Errorf("browser wait stable %s for in-page discovery: %w", target.String(), err)
	}
	navPage.CancelTimeout()

	baseSnapshot, err := c.pageContentSnapshot(page)
	if err != nil {
		return nil, err
	}
	seenContent := map[string]bool{}
	if normalizedContentText(baseSnapshot.Text) != "" {
		seenContent[hashString(normalizedContentText(baseSnapshot.Text))] = true
	}
	if len(seedContent) > 0 {
		seedDoc := htmldocs.Parse(docPathForURL(c.scopePath, target), seedContent)
		for _, section := range seedDoc.Sections {
			if text := normalizedContentText(section.Content); text != "" {
				seenContent[hashString(text)] = true
			}
		}
	}

	limit := c.inPageDocumentLimit()
	pages := make([]crawledPage, 0)
	if baseText := normalizedContentText(baseSnapshot.Text); len([]rune(baseText)) >= minInPageContentTextRunes {
		title := strings.TrimSpace(baseSnapshot.Title)
		if title == "" {
			title = "Overview"
		}
		content := htmlDocument(title, baseSnapshot.HTML)
		virtualURL := *target
		virtualURL.Fragment = "initial"
		docPath := virtualDocPath(docPathForURL(c.scopePath, target), title, "initial")
		doc := htmldocs.Parse(docPath, content)
		doc.URL = virtualURL.String()
		doc.Hash = hashBytes(content)
		pages = append(pages, crawledPage{
			URL:     &virtualURL,
			Depth:   1,
			Content: content,
			Doc:     doc,
		})
	}

	traversal := newInPageTraversalQueue()
	candidates, err := c.inPageCandidates(page)
	if err != nil {
		if len(pages) > 0 {
			return pages, err
		}
		return nil, err
	}
	traversal.enqueue(candidates)

	for traversal.canContinue(len(pages), limit) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		next, ok := traversal.next()
		if !ok {
			break
		}

		beforeSnapshot, err := c.pageContentSnapshot(page)
		if err != nil {
			if len(pages) > 0 {
				return pages, err
			}
			return nil, err
		}

		c.report(Progress{Phase: "in_page_click", URL: target.String() + "#" + next.Key, Visited: len(pages), Queued: traversal.queuedLen(), Depth: 1})
		clicked, err := c.clickInPageCandidate(page, next.Key)
		if err != nil {
			if len(pages) > 0 {
				return pages, err
			}
			return nil, err
		}
		if !clicked {
			traversal.recordProgress(false)
			continue
		}
		snapshot, err := c.pageContentSnapshot(page)
		if err != nil {
			if len(pages) > 0 {
				return pages, err
			}
			return nil, err
		}
		afterCandidates, err := c.inPageCandidates(page)
		if err != nil {
			if len(pages) > 0 {
				return pages, err
			}
			return nil, err
		}
		addedCandidates := traversal.enqueue(afterCandidates)

		progress := addedCandidates > 0
		text := normalizedContentText(snapshot.Text)
		beforeText := normalizedContentText(beforeSnapshot.Text)
		contentChanged := hashString(text) != hashString(beforeText)
		if contentChanged && len([]rune(text)) >= minInPageContentTextRunes {
			textHash := hashString(text)
			if !seenContent[textHash] {
				seenContent[textHash] = true

				title := strings.TrimSpace(snapshot.Title)
				if title == "" {
					title = strings.TrimSpace(next.Text)
				}
				content := htmlDocument(title, snapshot.HTML)
				virtualURL := *target
				virtualURL.Fragment = next.Key
				docPath := virtualDocPath(docPathForURL(c.scopePath, target), title, next.Key)
				doc := htmldocs.Parse(docPath, content)
				doc.URL = virtualURL.String()
				doc.Hash = hashBytes(content)
				pages = append(pages, crawledPage{
					URL:     &virtualURL,
					Depth:   1,
					Content: content,
					Doc:     doc,
				})
				progress = true
			}
		}
		traversal.recordProgress(progress)
	}

	if len(pages) < 2 && c.config.CrawlMode != "in_page" {
		return nil, nil
	}
	return pages, nil
}

func (c *Client) prepareBrowserPage(page *rod.Page, target *url.URL) error {
	if strings.TrimSpace(c.config.Cookie) != "" {
		cookies := parseCookieHeader(target, strings.TrimSpace(c.config.Cookie))
		if err := page.SetCookies(cookies); err != nil {
			return fmt.Errorf("browser set cookies for %s: %w", target.String(), err)
		}
	}

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
			return fmt.Errorf("browser set extra headers for %s: %w", target.String(), err)
		}
	}
	return nil
}

func (c *Client) inPageCandidates(page *rod.Page) ([]inPageCandidate, error) {
	res, err := page.Eval(`() => {
  const selectors = [
    'nav a', 'nav button', 'aside a', 'aside button',
    '[role="menuitem"]', '[role="treeitem"]',
    '[aria-expanded]', 'summary',
    '.el-submenu__title', '.el-tree-node__content',
    '.docs-sidebar .el-menu-item', '.docs-sidebar li',
    '.sidebar a', '.sidebar button', '.sidebar li',
    '.el-menu-item', '.menu-item', '.submenu', '.tree-node',
    '[class*="sidebar"] a', '[class*="sidebar"] button', '[class*="sidebar"] li',
    '[class*="menu"] a', '[class*="menu"] button', '[class*="menu"] li', '[class*="menu"][aria-expanded]',
    '[class*="tree"] a', '[class*="tree"] button', '[class*="tree"] li', '[class*="tree"][aria-expanded]'
  ];
  const blocked = new Set(['返回', '返回系统', '登录', '退出', '注销', '搜索', '上传文件']);
  const visible = (el) => {
    const style = getComputedStyle(el);
    if (style.display === 'none' || style.visibility === 'hidden' || style.pointerEvents === 'none') return false;
    const rect = el.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
  };
  const normalize = (text) => text.replace(/\s+/g, ' ').trim();
  const directText = (el) => {
    const aria = normalize(el.getAttribute('aria-label') || el.getAttribute('title') || '');
    if (aria) return aria;
    const clone = el.cloneNode(true);
    clone.querySelectorAll('ul,ol,nav,aside,[role="menu"],[role="tree"],[role="group"],[class*="submenu"],[class*="children"]').forEach(n => n.remove());
    const text = normalize(clone.innerText || clone.textContent || '');
    return text || normalize(el.innerText || el.textContent || '');
  };
  const shortClass = (el) => normalize(String(el.className || '')).split(' ').filter(Boolean).slice(0, 3).join('.');
  const navPath = (el) => {
    const parts = [];
    let cur = el;
    while (cur && cur !== document.body && parts.length < 5) {
      if (cur !== el && /^(li|details)$/i.test(cur.tagName || '')) {
        const text = directText(cur);
        if (text && text.length <= 80) parts.unshift(text);
      }
      cur = cur.parentElement;
    }
    return parts.join('/');
  };
  const elementIndex = (el) => {
    if (!el.parentElement) return '0';
    return Array.from(el.parentElement.children).indexOf(el).toString();
  };
  const stableKey = (el, text) => {
    const parts = [
      navPath(el),
      text,
      el.getAttribute('role') || '',
      el.getAttribute('aria-controls') || '',
      el.hasAttribute('aria-expanded') ? 'expandable' : '',
      el.getAttribute('data-index') || el.getAttribute('index') || '',
      shortClass(el),
      elementIndex(el)
    ].filter(Boolean);
    return 'nav-' + parts.join('|').toLowerCase().replace(/\s+/g, ' ').slice(0, 180);
  };
  const seen = new Set();
  const out = [];
  for (const el of document.querySelectorAll(selectors.join(','))) {
    if (!visible(el)) continue;
    const text = directText(el);
    if (text.length < 2 || text.length > 80 || blocked.has(text)) continue;
    let key = stableKey(el, text);
    let uniqueKey = key;
    let suffix = 2;
    while (seen.has(uniqueKey)) {
      uniqueKey = key + '|' + suffix++;
    }
    seen.add(uniqueKey);
    el.setAttribute('data-docgraph-nav-key', uniqueKey);
    out.push({ key: uniqueKey, text });
  }
  return out.slice(0, 160);
}`)
	if err != nil {
		return nil, fmt.Errorf("browser collect in-page candidates: %w", err)
	}
	var candidates []inPageCandidate
	if err := res.Value.Unmarshal(&candidates); err != nil {
		return nil, fmt.Errorf("decode in-page candidates: %w", err)
	}
	return candidates, nil
}

func (c *Client) clickInPageCandidate(page *rod.Page, key string) (bool, error) {
	res, err := page.Eval(`(key) => {
  const el = Array.from(document.querySelectorAll('[data-docgraph-nav-key]')).find((node) => node.getAttribute('data-docgraph-nav-key') === key);
  if (!el) return false;
  el.scrollIntoView({ block: 'center', inline: 'center' });
  el.click();
  return true;
}`, key)
	if err != nil {
		return false, fmt.Errorf("browser click in-page candidate %s: %w", key, err)
	}
	var ok bool
	if err := res.Value.Unmarshal(&ok); err != nil || !ok {
		return false, nil
	}
	_ = page.WaitStable(inPageClickStableWait)
	return true, nil
}

func (c *Client) pageContentSnapshot(page *rod.Page) (contentSnapshot, error) {
	res, err := page.Eval(`() => {
  const visible = (el) => {
    const style = getComputedStyle(el);
    if (style.display === 'none' || style.visibility === 'hidden') return false;
    const rect = el.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
  };
  const isChrome = (el) => {
    const raw = ((el.tagName || '') + ' ' + (el.id || '') + ' ' + (el.className || '') + ' ' + (el.getAttribute('role') || '')).toLowerCase();
    return /(^|\s)(nav|navigation|menu|sidebar|header|footer|toolbar)(\s|$)/.test(raw) ||
      raw.includes('docs-sidebar') || raw.includes('el-menu') || raw.includes('tree');
  };
  const cleanClone = (el) => {
    const clone = el.cloneNode(true);
    clone.querySelectorAll('script,style,noscript,nav,aside,header,footer,[role="navigation"],[role="menu"],[role="tree"],[role="group"],[class*="sidebar"],[class*="menu"],.el-menu').forEach(n => n.remove());
    return clone;
  };
  const candidates = Array.from(document.querySelectorAll('main,article,[role="main"],.docs-content,.doc-content,.markdown,.markdown-body,.content,.container'));
  let best = null;
  let bestScore = -1;
  for (const el of candidates) {
    if (!visible(el) || isChrome(el)) continue;
    const clone = cleanClone(el);
    const text = clone.innerText || clone.textContent || '';
    const score = text.trim().length - clone.querySelectorAll('a,button,input,select,textarea').length * 12;
    if (score > bestScore) {
      best = clone;
      bestScore = score;
    }
  }
  if (!best) best = cleanClone(document.body);
  const titleEl = best.querySelector('h1,h2,h3') || document.querySelector('h1,h2,h3');
  return {
    title: (titleEl ? titleEl.textContent : document.title || '').replace(/\s+/g, ' ').trim(),
    html: best.innerHTML || '',
    text: (best.innerText || best.textContent || '').replace(/\s+/g, ' ').trim()
  };
}`)
	if err != nil {
		return contentSnapshot{}, fmt.Errorf("browser snapshot page content: %w", err)
	}
	var snapshot contentSnapshot
	if err := res.Value.Unmarshal(&snapshot); err != nil {
		return contentSnapshot{}, fmt.Errorf("decode page content snapshot: %w", err)
	}
	return snapshot, nil
}

func (c *Client) inPageDocumentLimit() int {
	if c.config.MaxPages > 0 {
		return c.config.MaxPages
	}
	return 80
}

func (c *Client) pageTimeout() time.Duration {
	seconds := c.config.PageTimeoutSeconds
	if seconds <= 0 {
		seconds = 30
	}
	if seconds < 5 {
		seconds = 5
	}
	return time.Duration(seconds) * time.Second
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
		MaxPages:           intSetting(raw["max_pages"]),
		MaxDepth:           intSetting(raw["max_depth"]),
		BearerToken:        stringSetting(raw["bearer_token"]),
		Cookie:             stringSetting(raw["cookie"]),
		Headers:            headers,
		HeadersJSON:        stringSetting(raw["headers_json"]),
		CrawlMode:          crawlModeSetting(raw["crawl_mode"]),
		IsSPA:              boolSetting(raw["is_spa"]),
		PageTimeoutSeconds: intSetting(raw["page_timeout_seconds"]),
	}, nil
}

func crawlModeSetting(value any) string {
	mode := strings.ToLower(strings.TrimSpace(stringSetting(value)))
	switch mode {
	case "", "auto":
		return "auto"
	case "static", "browser", "in_page":
		return mode
	case "spa", "js", "rendered":
		return "browser"
	case "off":
		return "static"
	default:
		return "auto"
	}
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
