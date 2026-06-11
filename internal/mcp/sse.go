package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/docgraph/docgraph/internal/ids"
)

// SSEServer serves MCP over Server-Sent Events (SSE) via HTTP.
type SSEServer struct {
	handler  *Handler
	logger   *slog.Logger
	sessions map[string]*sseSession
	mu       sync.Mutex
}

// NewSSEServer creates an SSE MCP server wrapping the given handler.
func NewSSEServer(handler *Handler, logger *slog.Logger) *SSEServer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &SSEServer{
		handler:  handler,
		logger:   logger,
		sessions: make(map[string]*sseSession),
	}
}

type sseSession struct {
	id       string
	messages chan Response
	ctx      context.Context
	cancel   context.CancelFunc
}

const sessionTimeout = 5 * time.Minute

// RegisterRoutes adds the MCP SSE routes to an http.ServeMux.
func (s *SSEServer) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /mcp/sse", s.handleSSE)
	mux.HandleFunc("POST /mcp/sse/messages", s.handleMessages)
}

func (s *SSEServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	sessionID := ids.Random("sse", 12)
	ctx, cancel := context.WithTimeout(r.Context(), sessionTimeout)

	sess := &sseSession{
		id:       sessionID,
		messages: make(chan Response, 32),
		ctx:      ctx,
		cancel:   cancel,
	}

	s.mu.Lock()
	s.sessions[sessionID] = sess
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.sessions, sessionID)
		s.mu.Unlock()
		cancel()
	}()

	s.logger.Info("mcp sse session started", "session", sessionID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Send endpoint event so client knows where to POST messages.
	endpointURL := fmt.Sprintf("/mcp/sse/messages?sessionId=%s", sessionID)
	writeSSEEvent(w, flusher, "endpoint", endpointURL)
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("mcp sse session ended", "session", sessionID)
			return
		case msg := <-sess.messages:
			data, err := json.Marshal(msg)
			if err != nil {
				s.logger.Error("mcp sse marshal error", "err", err)
				continue
			}
			writeSSEEvent(w, flusher, "message", string(data))
		}
	}
}

func (s *SSEServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		http.Error(w, "sessionId query parameter is required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	sess, ok := s.sessions[sessionID]
	s.mu.Unlock()

	if !ok {
		http.Error(w, "session not found or expired", http.StatusNotFound)
		return
	}

	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	if len(req.ID) == 0 {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "notification ignored"})
		return
	}

	resp := s.handler.Handle(sess.ctx, req)

	select {
	case sess.messages <- resp:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ok"})
	case <-sess.ctx.Done():
		writeJSON(w, http.StatusGone, map[string]string{"error": "session closed"})
	case <-r.Context().Done():
		writeJSON(w, http.StatusRequestTimeout, map[string]string{"error": "request cancelled"})
	}
}

func (s *SSEServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/mcp/sse":
		s.handleSSE(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/mcp/sse/messages":
		s.handleMessages(w, r)
	default:
		http.NotFound(w, r)
	}
}

func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event, data string) {
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
