package mcp

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const (
	mcpEndpoint           = "/mcp"
	headerProtocolVersion = "MCP-Protocol-Version"
)

// StreamableHTTPServer serves MCP over the current Streamable HTTP transport.
type StreamableHTTPServer struct {
	handler *Handler
	logger  *slog.Logger
}

func NewStreamableHTTPServer(handler *Handler, logger *slog.Logger) *StreamableHTTPServer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &StreamableHTTPServer{handler: handler, logger: logger}
}

func (s *StreamableHTTPServer) RegisterRoutes(mux *http.ServeMux) {
	s.RegisterRoutesWithPrefix(mux, "")
}

func (s *StreamableHTTPServer) RegisterRoutesWithPrefix(mux *http.ServeMux, prefix string) {
	endpoint := strings.TrimRight(prefix, "/") + mcpEndpoint
	mux.HandleFunc("POST "+endpoint, s.handlePost)
	mux.HandleFunc("GET "+endpoint, s.handleGet)
	mux.HandleFunc("DELETE "+endpoint, s.handleDelete)
}

func (s *StreamableHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == mcpEndpoint:
		s.handlePost(w, r)
	case r.Method == http.MethodGet && r.URL.Path == mcpEndpoint:
		s.handleGet(w, r)
	case r.Method == http.MethodDelete && r.URL.Path == mcpEndpoint:
		s.handleDelete(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *StreamableHTTPServer) handlePost(w http.ResponseWriter, r *http.Request) {
	if !validOrigin(r) {
		writeJSON(w, http.StatusForbidden, rpcHTTPError("forbidden origin"))
		return
	}
	if !accepts(r, "application/json") || !accepts(r, "text/event-stream") {
		writeJSON(w, http.StatusNotAcceptable, rpcHTTPError("Accept must include application/json and text/event-stream"))
		return
	}
	if !hasContentType(r, "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, rpcHTTPError("Content-Type must be application/json"))
		return
	}
	if err := validateProtocolVersionHeader(r); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcHTTPError(err.Error()))
		return
	}

	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcHTTPError("invalid JSON body"))
		return
	}
	if len(req.ID) == 0 || strings.TrimSpace(req.Method) == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := s.handler.Handle(r.Context(), req)
	writeJSON(w, http.StatusOK, resp)
}

func (s *StreamableHTTPServer) handleGet(w http.ResponseWriter, r *http.Request) {
	if !validOrigin(r) {
		writeJSON(w, http.StatusForbidden, rpcHTTPError("forbidden origin"))
		return
	}
	if !accepts(r, "text/event-stream") {
		writeJSON(w, http.StatusNotAcceptable, rpcHTTPError("Accept must include text/event-stream"))
		return
	}
	if err := validateProtocolVersionHeader(r); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcHTTPError(err.Error()))
		return
	}
	w.Header().Set("Allow", http.MethodPost)
	writeJSON(w, http.StatusMethodNotAllowed, rpcHTTPError("server-initiated SSE stream is not supported"))
}

func (s *StreamableHTTPServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !validOrigin(r) {
		writeJSON(w, http.StatusForbidden, rpcHTTPError("forbidden origin"))
		return
	}
	if err := validateProtocolVersionHeader(r); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcHTTPError(err.Error()))
		return
	}
	w.Header().Set("Allow", http.MethodPost)
	writeJSON(w, http.StatusMethodNotAllowed, rpcHTTPError("explicit session termination is not supported"))
}

func validateProtocolVersionHeader(r *http.Request) error {
	version := strings.TrimSpace(r.Header.Get(headerProtocolVersion))
	if version == "" {
		return nil
	}
	if supportedProtocolVersions[version] {
		return nil
	}
	return errUnsupportedProtocolVersion(version)
}

type unsupportedProtocolVersionError string

func errUnsupportedProtocolVersion(version string) error {
	return unsupportedProtocolVersionError(version)
}

func (e unsupportedProtocolVersionError) Error() string {
	return "unsupported MCP protocol version " + string(e)
}

func accepts(r *http.Request, contentType string) bool {
	accept := strings.TrimSpace(r.Header.Get("Accept"))
	if accept == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if mediaType == contentType || mediaType == "*/*" {
			return true
		}
	}
	return false
}

func hasContentType(r *http.Request, contentType string) bool {
	got := strings.TrimSpace(r.Header.Get("Content-Type"))
	if got == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(strings.SplitN(got, ";", 2)[0]), contentType)
}

func validOrigin(r *http.Request) bool {
	rawOrigin := strings.TrimSpace(r.Header.Get("Origin"))
	if rawOrigin == "" {
		return true
	}
	origin, err := url.Parse(rawOrigin)
	if err != nil || origin.Host == "" {
		return false
	}
	if strings.EqualFold(origin.Host, r.Host) {
		return true
	}
	return isLoopbackHost(origin.Host) && isLoopbackHost(r.Host)
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func rpcHTTPError(message string) Response {
	return Response{
		JSONRPC: "2.0",
		Error:   &ResponseError{Code: -32600, Message: message},
	}
}
