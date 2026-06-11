package confluence

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestFetchUsesBasicAuthAndConfiguredPageIDs(t *testing.T) {
	client := newConfluenceTestClient(t, func(r *http.Request) *http.Response {
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("user@example.com:api-secret")) {
			return textConfluenceResponse(http.StatusUnauthorized, "unauthorized")
		}
		switch r.URL.Path {
		case "/wiki/rest/api/content/100":
			return confluencePageResponse(t, "100", "Member Confluence Root", 1, `<h1>Overview</h1><p>Connector confluencebasicroot token.</p>`)
		case "/wiki/rest/api/content/200":
			return confluencePageResponse(t, "200", "Member Confluence Configured", 2, `<h1>Configured</h1><p>Connector confluencebasicchild token.</p>`)
		default:
			return textConfluenceResponse(http.StatusNotFound, "not found")
		}
	}, Config{
		Username: "user@example.com",
		APIToken: "api-secret",
	})

	pages, err := client.FetchPages(context.Background(), []string{"100", "200"})
	if err != nil {
		t.Fatalf("FetchPages returned error: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("FetchPages returned %d pages, want 2: %+v", len(pages), pages)
	}
	if pages[0].ID != "100" || pages[0].Title != "Member Confluence Root" || !strings.Contains(pages[0].BodyHTML, "confluencebasicroot") {
		t.Fatalf("first page = %+v, want root page body", pages[0])
	}
	if pages[1].ID != "200" || pages[1].Version != "2" || !strings.Contains(pages[1].URL, "/spaces/PROD/pages/200") {
		t.Fatalf("second page = %+v, want configured page URL/version", pages[1])
	}
}

func TestFetchReturnsClearAuthAndNotFoundErrors(t *testing.T) {
	client := newConfluenceTestClient(t, func(r *http.Request) *http.Response {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			return textConfluenceResponse(http.StatusUnauthorized, "unauthorized")
		}
		return textConfluenceResponse(http.StatusNotFound, "not found")
	}, Config{Token: "bad-token"})

	_, err := client.FetchPages(context.Background(), []string{"100"})
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("FetchPages auth error = %v, want auth error", err)
	}

	client.config.Token = "good-token"
	_, err = client.FetchPages(context.Background(), []string{"missing"})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("FetchPages missing page error = %v, want 404 error", err)
	}
}

func TestFetchReturnsClearRedirectError(t *testing.T) {
	client := newConfluenceTestClient(t, func(r *http.Request) *http.Response {
		return redirectConfluenceResponse("https://zerotrust.example/portal.php")
	}, Config{Token: "pat-secret"})

	_, err := client.FetchPages(context.Background(), []string{"100"})
	if err == nil || !strings.Contains(err.Error(), "redirected to https://zerotrust.example/portal.php") || !strings.Contains(err.Error(), "SSO/OIDC") {
		t.Fatalf("FetchPages redirect error = %v, want clear SSO redirect error", err)
	}
}

func newConfluenceTestClient(t *testing.T, handle func(*http.Request) *http.Response, config Config) *Client {
	t.Helper()

	baseURL, err := url.Parse("https://confluence.example/wiki")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return responseViaRecorder(handle(r)), nil
			}),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		config: config,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func confluencePageResponse(t *testing.T, id string, title string, version int, body string) *http.Response {
	t.Helper()

	var data bytes.Buffer
	if err := json.NewEncoder(&data).Encode(map[string]any{
		"id":    id,
		"type":  "page",
		"title": title,
		"version": map[string]any{
			"number": version,
		},
		"_links": map[string]any{
			"webui": "/spaces/PROD/pages/" + id,
		},
		"body": map[string]any{
			"storage": map[string]any{
				"value":          body,
				"representation": "storage",
			},
		},
	}); err != nil {
		t.Fatalf("encode confluence page: %v", err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(&data),
	}
}

func textConfluenceResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func redirectConfluenceResponse(location string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": []string{location}},
		Body:       io.NopCloser(strings.NewReader("redirect")),
	}
}

func responseViaRecorder(resp *http.Response) *http.Response {
	rr := httptest.NewRecorder()
	for key, values := range resp.Header {
		for _, value := range values {
			rr.Header().Add(key, value)
		}
	}
	rr.WriteHeader(resp.StatusCode)
	if resp.Body != nil {
		_, _ = io.Copy(rr, resp.Body)
	}
	return rr.Result()
}
