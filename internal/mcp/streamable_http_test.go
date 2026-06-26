package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamableHTTPInitializeNegotiatesCurrentProtocol(t *testing.T) {
	server := NewStreamableHTTPServer(NewHandler(newTestQueryService(t), nil), nil)
	rr := postStreamableMCP(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"dev"}}}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}

	var resp rpcResponse
	decodeStreamableResponse(t, rr, &resp)
	requireNoRPCError(t, resp)
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	unmarshalResult(t, resp, &result)
	if result.ProtocolVersion != "2025-11-25" {
		t.Fatalf("protocolVersion = %q, want 2025-11-25", result.ProtocolVersion)
	}
}

func TestStreamableHTTPToolsList(t *testing.T) {
	server := NewStreamableHTTPServer(NewHandler(newTestQueryService(t), nil), nil)
	rr := postStreamableMCP(t, server, `{"jsonrpc":"2.0","id":"tools","method":"tools/list"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp rpcResponse
	decodeStreamableResponse(t, rr, &resp)
	requireNoRPCError(t, resp)
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	unmarshalResult(t, resp, &result)
	if len(result.Tools) == 0 {
		t.Fatalf("tools/list returned no tools")
	}
}

func TestStreamableHTTPNotificationReturnsAccepted(t *testing.T) {
	server := NewStreamableHTTPServer(NewHandler(newTestQueryService(t), nil), nil)
	rr := postStreamableMCP(t, server, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusAccepted, rr.Body.String())
	}
	if strings.TrimSpace(rr.Body.String()) != "" {
		t.Fatalf("body = %q, want empty", rr.Body.String())
	}
}

func TestStreamableHTTPRejectsInvalidHeaders(t *testing.T) {
	server := NewStreamableHTTPServer(NewHandler(newTestQueryService(t), nil), nil)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotAcceptable {
		t.Fatalf("missing Accept status = %d, want %d; body: %s", rr.Code, http.StatusNotAcceptable, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Origin", "https://attacker.example")
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("invalid Origin status = %d, want %d; body: %s", rr.Code, http.StatusForbidden, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want %d; body: %s", rr.Code, http.StatusMethodNotAllowed, rr.Body.String())
	}
}

func postStreamableMCP(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func decodeStreamableResponse(t *testing.T, rr *httptest.ResponseRecorder, dst any) {
	t.Helper()

	if err := json.NewDecoder(rr.Body).Decode(dst); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
}
