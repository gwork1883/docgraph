package confluence

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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docgraph/docgraph/internal/ingest/htmldocs"
)

type Config struct {
	BaseURL         string   `json:"base_url"`
	PageID          string   `json:"page_id"`
	SpaceKey        string   `json:"space_key"`
	PageIDs         []string `json:"page_ids"`
	Token           string   `json:"token"`
	Username        string   `json:"username"`
	APIToken        string   `json:"api_token"`
	Cookie          string   `json:"cookie"`
	IncludeChildren bool     `json:"include_children"`
}

type Page struct {
	ID       string
	Title    string
	URL      string
	Version  string
	BodyHTML string
	ParentID string
}

type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	config     Config
}

var httpClientFactory = struct {
	sync.RWMutex
	new func() *http.Client
}{
	new: func() *http.Client {
		return &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	},
}

type pageResponse struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Type    string `json:"type"`
	Version struct {
		Number int `json:"number"`
	} `json:"version"`
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
	Body struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
	} `json:"body"`
}

type childrenResponse struct {
	Results []struct {
		ID string `json:"id"`
	} `json:"results"`
}

func Load(ctx context.Context, dsn string, configJSON string) ([]htmldocs.Document, error) {
	client, pageIDs, err := NewClient(dsn, configJSON)
	if err != nil {
		return nil, err
	}
	return client.LoadPages(ctx, pageIDs)
}

func Fetch(ctx context.Context, dsn string, configJSON string) ([]Page, error) {
	client, pageIDs, err := NewClient(dsn, configJSON)
	if err != nil {
		return nil, err
	}
	return client.FetchPages(ctx, pageIDs)
}

func NewClient(dsn string, configJSON string) (*Client, []string, error) {
	cfg, err := parseConfig(configJSON)
	if err != nil {
		return nil, nil, err
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = strings.TrimSpace(dsn)
	}
	if cfg.BaseURL == "" {
		return nil, nil, fmt.Errorf("confluence base_url is required")
	}
	baseURL, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, nil, fmt.Errorf("invalid confluence base_url %q", cfg.BaseURL)
	}
	pageIDs := configuredPageIDs(cfg)
	if len(pageIDs) == 0 {
		return nil, nil, fmt.Errorf("confluence page_id or page_ids is required")
	}
	if cfg.Username != "" && cfg.APIToken == "" {
		return nil, nil, fmt.Errorf("confluence api_token is required when username is set")
	}

	return &Client{
		baseURL:    baseURL,
		httpClient: newHTTPClient(),
		config:     cfg,
	}, pageIDs, nil
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

func (c *Client) FetchPages(ctx context.Context, pageIDs []string) ([]Page, error) {
	visited := make(map[string]bool)
	pagesByID := make(map[string]Page)
	for _, id := range pageIDs {
		if err := c.fetchPageTree(ctx, strings.TrimSpace(id), "", visited, pagesByID); err != nil {
			return nil, err
		}
	}
	ids := make([]string, 0, len(pagesByID))
	for id := range pagesByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	pages := make([]Page, 0, len(ids))
	for _, id := range ids {
		pages = append(pages, pagesByID[id])
	}
	return pages, nil
}

func (c *Client) fetchPageTree(ctx context.Context, id string, parentID string, visited map[string]bool, pages map[string]Page) error {
	if id == "" || visited[id] {
		return nil
	}
	visited[id] = true
	page, err := c.fetchPage(ctx, id)
	if err != nil {
		return err
	}
	pages[id] = responseToPage(c.baseURL, page, parentID)

	if !c.config.IncludeChildren {
		return nil
	}
	childIDs, err := c.fetchChildPageIDs(ctx, id)
	if err != nil {
		return err
	}
	for _, childID := range childIDs {
		if err := c.fetchPageTree(ctx, childID, id, visited, pages); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) LoadPages(ctx context.Context, pageIDs []string) ([]htmldocs.Document, error) {
	visited := make(map[string]bool)
	docsByID := make(map[string]htmldocs.Document)
	for _, id := range pageIDs {
		if err := c.loadPageTree(ctx, strings.TrimSpace(id), visited, docsByID); err != nil {
			return nil, err
		}
	}
	ids := make([]string, 0, len(docsByID))
	for id := range docsByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	docs := make([]htmldocs.Document, 0, len(ids))
	for _, id := range ids {
		docs = append(docs, docsByID[id])
	}
	return docs, nil
}

func (c *Client) loadPageTree(ctx context.Context, id string, visited map[string]bool, docs map[string]htmldocs.Document) error {
	if id == "" || visited[id] {
		return nil
	}
	visited[id] = true
	page, err := c.fetchPage(ctx, id)
	if err != nil {
		return err
	}
	docs[id] = pageToDocument(c.baseURL, page)

	if !c.config.IncludeChildren {
		return nil
	}
	childIDs, err := c.fetchChildPageIDs(ctx, id)
	if err != nil {
		return err
	}
	for _, childID := range childIDs {
		if err := c.loadPageTree(ctx, childID, visited, docs); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) fetchPage(ctx context.Context, id string) (pageResponse, error) {
	var page pageResponse
	endpoint := c.endpoint("/rest/api/content/" + url.PathEscape(id))
	query := endpoint.Query()
	query.Set("expand", "body.storage,version")
	endpoint.RawQuery = query.Encode()
	if err := c.getJSON(ctx, endpoint.String(), &page); err != nil {
		return pageResponse{}, err
	}
	if page.ID == "" {
		page.ID = id
	}
	if page.Title == "" {
		page.Title = id
	}
	return page, nil
}

func (c *Client) fetchChildPageIDs(ctx context.Context, id string) ([]string, error) {
	endpoint := c.endpoint("/rest/api/content/" + url.PathEscape(id) + "/child/page")
	query := endpoint.Query()
	query.Set("limit", "100")
	endpoint.RawQuery = query.Encode()
	var body childrenResponse
	if err := c.getJSON(ctx, endpoint.String(), &body); err != nil {
		return nil, err
	}
	childIDs := make([]string, 0, len(body.Results))
	for _, child := range body.Results {
		if strings.TrimSpace(child.ID) != "" {
			childIDs = append(childIDs, strings.TrimSpace(child.ID))
		}
	}
	return childIDs, nil
}

func (c *Client) getJSON(ctx context.Context, rawURL string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	if c.config.Token != "" {
		if strings.HasPrefix(c.config.Token, "Token ") || strings.HasPrefix(c.config.Token, "Bearer ") {
			req.Header.Set("Authorization", c.config.Token)
		} else {
			req.Header.Set("Authorization", "Bearer "+c.config.Token)
		}
	}
	if c.config.Username != "" && c.config.APIToken != "" {
		req.SetBasicAuth(c.config.Username, c.config.APIToken)
	}
	if c.config.Cookie != "" {
		req.Header.Set("Cookie", c.config.Cookie)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("confluence auth failed: HTTP %d (check your token — for Confluence Cloud use a Personal Access Token; for Data Center use a PAT from your user profile)%s", resp.StatusCode, errorDetail(body))
	}
	if isRedirect(resp.StatusCode) {
		location := resp.Header.Get("Location")
		return fmt.Errorf("confluence requires SSO/OIDC login: redirected to %s (HTTP %d) — the base URL sits behind an identity proxy (e.g. Zero Trust gateway) that blocks API access. You can use a browser session cookie to authenticate: 1) open Chrome DevTools on the wiki page, 2) go to Application > Cookies, 3) copy all cookies in the format \"name1=value1; name2=value2\", 4) paste them into the \"cookie\" field in source config", location, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("confluence page not found: HTTP 404")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("confluence request failed: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "application/json") && !strings.HasPrefix(contentType, "application/vnd.atlassian") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("confluence returned non-JSON response (Content-Type: %q) — likely an SSO/OIDC login page. If your Confluence sits behind a Zero Trust / identity proxy, you can use a browser session cookie: copy cookies from your logged-in browser session and paste them into the \"cookie\" config field.\n\nResponse preview: %s", contentType, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode confluence response: %w", err)
	}
	return nil
}

func isRedirect(code int) bool {
	return code == http.StatusFound || code == http.StatusMovedPermanently ||
		code == http.StatusTemporaryRedirect || code == http.StatusPermanentRedirect
}

func errorDetail(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	return fmt.Sprintf("; response: %s", strings.TrimSpace(string(body)))
}

func (c *Client) endpoint(suffix string) *url.URL {
	u := *c.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + suffix
	return &u
}

func pageToDocument(baseURL *url.URL, page pageResponse) htmldocs.Document {
	relPath := page.ID + ".html"
	storageHTML := page.Body.Storage.Value
	content := []byte("<!doctype html><html><head><title>" + page.Title + "</title></head><body>" + storageHTML + "</body></html>")
	doc := htmldocs.Parse(relPath, content)
	doc.Title = page.Title
	doc.Path = relPath
	doc.Hash = hashString(page.ID + "\n" + page.Title + "\n" + fmt.Sprint(page.Version.Number) + "\n" + storageHTML)
	if page.Links.WebUI != "" {
		webURL := *baseURL
		webURL.Path = path.Join(webURL.Path, page.Links.WebUI)
		if strings.HasPrefix(page.Links.WebUI, "/") {
			webURL.Path = strings.TrimRight(baseURL.Path, "/") + page.Links.WebUI
		}
		doc.Path = relPath + "|" + webURL.String()
	}
	return doc
}

func parseConfig(configJSON string) (Config, error) {
	configJSON = strings.TrimSpace(configJSON)
	if configJSON == "" {
		configJSON = "{}"
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(configJSON), &raw); err != nil {
		return Config{}, fmt.Errorf("parse confluence config_json: %w", err)
	}
	cfg := Config{
		BaseURL:         stringValue(raw["base_url"]),
		PageID:          stringValue(raw["page_id"]),
		SpaceKey:        stringValue(raw["space_key"]),
		Token:           stringValue(raw["token"]),
		Username:        stringValue(raw["username"]),
		APIToken:        stringValue(raw["api_token"]),
		Cookie:          stringValue(raw["cookie"]),
		IncludeChildren: boolValue(raw["include_children"]),
	}
	cfg.PageIDs = stringSliceValue(raw["page_ids"])
	if cfg.PageID == "" && len(cfg.PageIDs) == 0 {
		cfg.PageID = stringValue(raw["root_page_id"])
	}
	return cfg, nil
}

func configuredPageIDs(cfg Config) []string {
	values := make([]string, 0, len(cfg.PageIDs)+1)
	values = append(values, cfg.PageIDs...)
	if cfg.PageID != "" {
		values = append(values, cfg.PageID)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part != "" && !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	return out
}

func stringValue(value any) string {
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func boolValue(value any) bool {
	if b, ok := value.(bool); ok {
		return b
	}
	return false
}

func stringSliceValue(value any) []string {
	switch typed := value.(type) {
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if s := stringValue(item); s != "" {
				values = append(values, s)
			}
		}
		return values
	case string:
		return []string{typed}
	default:
		return nil
	}
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func responseToPage(baseURL *url.URL, page pageResponse, parentID string) Page {
	return Page{
		ID:       page.ID,
		Title:    page.Title,
		URL:      pageURL(baseURL, page),
		Version:  fmt.Sprint(page.Version.Number),
		BodyHTML: page.Body.Storage.Value,
		ParentID: parentID,
	}
}

func pageURL(baseURL *url.URL, page pageResponse) string {
	if page.Links.WebUI == "" {
		endpoint := *baseURL
		endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/rest/api/content/" + url.PathEscape(page.ID)
		return endpoint.String()
	}
	webURL := *baseURL
	if strings.HasPrefix(page.Links.WebUI, "/") {
		webURL.Path = strings.TrimRight(baseURL.Path, "/") + page.Links.WebUI
	} else {
		webURL.Path = path.Join(webURL.Path, page.Links.WebUI)
	}
	return webURL.String()
}
