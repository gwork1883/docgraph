package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/docgraph/docgraph/internal/config"
	"github.com/docgraph/docgraph/internal/ids"
	"github.com/docgraph/docgraph/internal/mcp"
	"github.com/docgraph/docgraph/internal/query"
	"github.com/docgraph/docgraph/internal/storage"
	syncsvc "github.com/docgraph/docgraph/internal/sync"
	"github.com/docgraph/docgraph/internal/web"
)

type Server struct {
	addr      string
	store     storage.Store
	logger    *slog.Logger
	auth      config.AuthConfig
	mcpServer *mcp.SSEServer
}

func New(addr string, store storage.Store, logger *slog.Logger) *Server {
	queryService := query.NewService(store)
	mcpHandler := mcp.NewHandler(queryService, store)
	return &Server{
		addr:      addr,
		store:     store,
		logger:    logger,
		mcpServer: mcp.NewSSEServer(mcpHandler, logger),
	}
}

func NewWithAuth(addr string, store storage.Store, logger *slog.Logger, auth config.AuthConfig) *Server {
	if auth.Mode == "" {
		auth.Mode = "none"
	}
	queryService := query.NewService(store)
	mcpHandler := mcp.NewHandler(queryService, store)
	return &Server{
		addr:      addr,
		store:     store,
		logger:    logger,
		auth:      auth,
		mcpServer: mcp.NewSSEServer(mcpHandler, logger),
	}
}

func (s *Server) Run(ctx context.Context) error {
	mux, err := s.routes()
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              s.addr,
		Handler:           s.logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		s.logger.Info("server started", "addr", s.addr)
		errc <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		s.logger.Info("server stopped")
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) routes() (http.Handler, error) {
	mux := http.NewServeMux()

	staticFS, err := fs.Sub(web.Static, "static")
	if err != nil {
		return nil, err
	}

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/search", s.handleSearch)
	mux.HandleFunc("POST /api/context", s.handleContext)
	mux.HandleFunc("GET /api/sources", s.handleListSources)
	mux.HandleFunc("POST /api/sources", s.handleCreateSource)
	mux.HandleFunc("PUT /api/sources/{id}", s.handleUpdateSource)
	mux.HandleFunc("DELETE /api/sources/{id}", s.handleDeleteSource)
	mux.HandleFunc("POST /api/sources/{id}/sync", s.handleSyncSource)
	mux.HandleFunc("GET /api/sources/{id}/jobs", s.handleListSourceJobs)
	mux.HandleFunc("DELETE /api/sources/{id}/jobs/{jobID}", s.handleDeleteSourceJob)
	mux.HandleFunc("GET /api/sources/{id}/artifacts", s.handleListSourceArtifacts)
	mux.HandleFunc("GET /api/documents/{id}/profile", s.handleGetDocumentProfile)
	mux.HandleFunc("PUT /api/documents/{id}/profile", s.handleUpdateDocumentProfile)
	mux.HandleFunc("GET /api/nodes", s.handleSearchNodes)
	mux.HandleFunc("GET /api/nodes/{id}", s.handleGetNode)
	mux.HandleFunc("GET /api/nodes/{id}/related", s.handleRelatedNodes)
	mux.HandleFunc("POST /api/impact", s.handleImpact)
	mux.HandleFunc("POST /api/feedback", s.handleCreateFeedback)
	mux.HandleFunc("GET /api/feedback", s.handleListFeedback)
	mux.HandleFunc("DELETE /api/feedback/{id}", s.handleDeleteFeedback)
	s.mcpServer.RegisterRoutes(mux)
	mux.Handle("/", http.FileServerFS(staticFS))

	return s.requireAuth(mux), nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.store.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	sources, err := s.store.ListSources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

type createSourceRequest struct {
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	DSN             string `json:"dsn"`
	ConfigJSON      string `json:"config_json"`
	ProductHint     string `json:"product_hint"`
	ModuleHint      string `json:"module_hint"`
	SyncSchedule    string `json:"sync_schedule"`
	Branch          string `json:"branch"`
	Path            string `json:"path"`
	Cache           string `json:"cache"`
	Include         string `json:"include"`
	Exclude         string `json:"exclude"`
	URLPrefix       string `json:"url_prefix"`
	IdentityFile    string `json:"identity_file"`
	Password        string `json:"password"`
	Passphrase      string `json:"passphrase"`
	KnownHosts      string `json:"known_hosts"`
	StrictHostKey   bool   `json:"strict_host_key"`
	BaseURL         string `json:"base_url"`
	PageID          string `json:"page_id"`
	SpaceKey        string `json:"space_key"`
	Token           string `json:"token"`
	Username        string `json:"username"`
	APIToken        string `json:"api_token"`
	IncludeChildren *bool  `json:"include_children"`
	MaxPages        string `json:"max_pages"`
	MaxDepth        string `json:"max_depth"`
	BearerToken     string `json:"bearer_token"`
	Cookie          string `json:"cookie"`
	HeadersJSON     string `json:"headers_json"`
	IsSPA           *bool  `json:"is_spa"`
}

type updateSourceRequest struct {
	Kind            *string `json:"kind"`
	Name            *string `json:"name"`
	DSN             *string `json:"dsn"`
	ConfigJSON      *string `json:"config_json"`
	ProductHint     *string `json:"product_hint"`
	ModuleHint      *string `json:"module_hint"`
	SyncSchedule    *string `json:"sync_schedule"`
	Branch          *string `json:"branch"`
	Path            *string `json:"path"`
	Cache           *string `json:"cache"`
	Include         *string `json:"include"`
	Exclude         *string `json:"exclude"`
	URLPrefix       *string `json:"url_prefix"`
	IdentityFile    *string `json:"identity_file"`
	Password        *string `json:"password"`
	Passphrase      *string `json:"passphrase"`
	KnownHosts      *string `json:"known_hosts"`
	StrictHostKey   *bool   `json:"strict_host_key"`
	BaseURL         *string `json:"base_url"`
	PageID          *string `json:"page_id"`
	SpaceKey        *string `json:"space_key"`
	Token           *string `json:"token"`
	Username        *string `json:"username"`
	APIToken        *string `json:"api_token"`
	IncludeChildren *bool   `json:"include_children"`
	MaxPages        *string `json:"max_pages"`
	MaxDepth        *string `json:"max_depth"`
	BearerToken     *string `json:"bearer_token"`
	Cookie          *string `json:"cookie"`
	HeadersJSON     *string `json:"headers_json"`
	IsSPA           *bool   `json:"is_spa"`
}

type feedbackRequest struct {
	TargetKind   string          `json:"target_kind"`
	TargetID     string          `json:"target_id"`
	FeedbackKind string          `json:"feedback_kind"`
	Payload      json.RawMessage `json:"payload"`
	PayloadJSON  string          `json:"payload_json"`
	Actor        string          `json:"actor"`
}

func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	var req createSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	req.Kind = strings.TrimSpace(req.Kind)
	req.Name = strings.TrimSpace(req.Name)
	req.DSN = strings.TrimSpace(req.DSN)
	if req.Kind == "" {
		req.Kind = "local"
	}
	if !supportedSourceKind(req.Kind) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported source kind %q", req.Kind))
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source name is required"))
		return
	}
	if req.DSN == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source dsn is required"))
		return
	}

	bools := map[string]bool{}
	if req.IncludeChildren != nil {
		bools["include_children"] = *req.IncludeChildren
	}
	if req.StrictHostKey {
		bools["strict_host_key"] = req.StrictHostKey
	}
	if req.IsSPA != nil {
		bools["is_spa"] = *req.IsSPA
	}

	source := storage.Source{
		ID:   ids.Random("src", 12),
		Kind: req.Kind,
		Name: req.Name,
		DSN:  req.DSN,
		ConfigJSON: sourceConfigJSON(req.ConfigJSON, map[string]string{
			"branch":        req.Branch,
			"path":          req.Path,
			"cache":         req.Cache,
			"include":       req.Include,
			"exclude":       req.Exclude,
			"url_prefix":    req.URLPrefix,
			"identity_file": req.IdentityFile,
			"password":      req.Password,
			"passphrase":    req.Passphrase,
			"known_hosts":   req.KnownHosts,
			"base_url":      req.BaseURL,
			"page_id":       req.PageID,
			"space_key":     req.SpaceKey,
			"token":         req.Token,
			"username":      req.Username,
			"api_token":     req.APIToken,
			"max_pages":     req.MaxPages,
			"max_depth":     req.MaxDepth,
			"bearer_token":  req.BearerToken,
			"cookie":        req.Cookie,
			"headers_json":  req.HeadersJSON,
		}, bools),
		ProductHint:  strings.TrimSpace(req.ProductHint),
		ModuleHint:   strings.TrimSpace(req.ModuleHint),
		SyncSchedule: strings.TrimSpace(req.SyncSchedule),
	}
	created, err := s.store.CreateSource(r.Context(), source)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
		return
	}
	var req updateSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}

	source, err := s.store.GetSource(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("source %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if req.Kind != nil {
		source.Kind = strings.TrimSpace(*req.Kind)
	}
	if req.Name != nil {
		source.Name = strings.TrimSpace(*req.Name)
	}
	if req.DSN != nil {
		source.DSN = strings.TrimSpace(*req.DSN)
	}
	if req.ConfigJSON != nil {
		source.ConfigJSON = strings.TrimSpace(*req.ConfigJSON)
	}
	if req.ProductHint != nil {
		source.ProductHint = strings.TrimSpace(*req.ProductHint)
	}
	if req.ModuleHint != nil {
		source.ModuleHint = strings.TrimSpace(*req.ModuleHint)
	}
	if req.SyncSchedule != nil {
		source.SyncSchedule = strings.TrimSpace(*req.SyncSchedule)
	}
	if req.Branch != nil || req.Path != nil || req.Cache != nil || req.Include != nil || req.Exclude != nil || req.URLPrefix != nil || req.IdentityFile != nil || req.Password != nil || req.Passphrase != nil || req.KnownHosts != nil || req.StrictHostKey != nil {
		bools := map[string]bool{}
		if req.StrictHostKey != nil {
			bools["strict_host_key"] = *req.StrictHostKey
		}
		stringsMap := map[string]string{}
		addOptionalString(stringsMap, "branch", req.Branch)
		addOptionalString(stringsMap, "path", req.Path)
		addOptionalString(stringsMap, "cache", req.Cache)
		addOptionalString(stringsMap, "include", req.Include)
		addOptionalString(stringsMap, "exclude", req.Exclude)
		addOptionalString(stringsMap, "url_prefix", req.URLPrefix)
		addOptionalString(stringsMap, "identity_file", req.IdentityFile)
		addOptionalString(stringsMap, "password", req.Password)
		addOptionalString(stringsMap, "passphrase", req.Passphrase)
		addOptionalString(stringsMap, "known_hosts", req.KnownHosts)
		source.ConfigJSON = sourceConfigJSONClearingEmpty(source.ConfigJSON, stringsMap, bools)
	}
	if req.BaseURL != nil || req.PageID != nil || req.SpaceKey != nil || req.Token != nil || req.Username != nil || req.APIToken != nil || req.IncludeChildren != nil || req.MaxPages != nil || req.MaxDepth != nil || req.BearerToken != nil || req.Cookie != nil || req.HeadersJSON != nil || req.IsSPA != nil {
		bools := map[string]bool{}
		if req.IncludeChildren != nil {
			bools["include_children"] = *req.IncludeChildren
		}
		if req.IsSPA != nil {
			bools["is_spa"] = *req.IsSPA
		}
		stringsMap := map[string]string{}
		addOptionalString(stringsMap, "base_url", req.BaseURL)
		addOptionalString(stringsMap, "page_id", req.PageID)
		addOptionalString(stringsMap, "space_key", req.SpaceKey)
		addOptionalString(stringsMap, "token", req.Token)
		addOptionalString(stringsMap, "username", req.Username)
		addOptionalString(stringsMap, "api_token", req.APIToken)
		addOptionalString(stringsMap, "max_pages", req.MaxPages)
		addOptionalString(stringsMap, "max_depth", req.MaxDepth)
		addOptionalString(stringsMap, "bearer_token", req.BearerToken)
		addOptionalString(stringsMap, "cookie", req.Cookie)
		addOptionalString(stringsMap, "headers_json", req.HeadersJSON)
		source.ConfigJSON = sourceConfigJSONClearingEmpty(source.ConfigJSON, stringsMap, bools)
	}
	if !supportedSourceKind(source.Kind) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported source kind %q", source.Kind))
		return
	}
	if source.Name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source name is required"))
		return
	}
	if source.DSN == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source dsn is required"))
		return
	}

	updated, err := s.store.UpdateSource(r.Context(), source)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func supportedSourceKind(kind string) bool {
	switch kind {
	case "local", "static", "sftp", "openapi", "git", "html", "confluence", "webdocs":
		return true
	default:
		return false
	}
}

func sourceConfigJSON(existing string, stringsMap map[string]string, boolsMap map[string]bool) string {
	return sourceConfigJSONWithOptions(existing, stringsMap, boolsMap, false)
}

func sourceConfigJSONClearingEmpty(existing string, stringsMap map[string]string, boolsMap map[string]bool) string {
	return sourceConfigJSONWithOptions(existing, stringsMap, boolsMap, true)
}

func sourceConfigJSONWithOptions(existing string, stringsMap map[string]string, boolsMap map[string]bool, clearEmpty bool) string {
	existing = strings.TrimSpace(existing)
	if existing == "" {
		existing = "{}"
	}
	value := map[string]any{}
	if existing != "{}" {
		_ = json.Unmarshal([]byte(existing), &value)
	}
	for key, raw := range stringsMap {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			value[key] = raw
		} else if clearEmpty {
			delete(value, key)
		}
	}
	for key, raw := range boolsMap {
		value[key] = raw
	}
	if len(value) == 0 {
		return "{}"
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func addOptionalString(values map[string]string, key string, value *string) {
	if value != nil {
		values[key] = *value
	}
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
		return
	}
	if err := s.store.DeleteSource(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("source %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (s *Server) handleSyncSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	result, err := syncsvc.NewService(s.store).SyncSource(r.Context(), id)
	if err != nil {
		if errors.Is(err, storage.ErrSyncInProgress) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleListSourceJobs(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
		return
	}
	limit := 20
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		value, err := strconv.Atoi(rawLimit)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("limit must be an integer"))
			return
		}
		limit = value
	}
	jobs, err := s.store.ListSyncJobs(r.Context(), id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) handleDeleteSourceJob(w http.ResponseWriter, r *http.Request) {
	sourceID := strings.TrimSpace(r.PathValue("id"))
	jobID := strings.TrimSpace(r.PathValue("jobID"))
	if sourceID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
		return
	}
	if jobID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("job id is required"))
		return
	}
	if err := s.store.DeleteSyncJob(r.Context(), sourceID, jobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("sync job %q not found", jobID))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": jobID})
}

func (s *Server) handleListSourceArtifacts(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
		return
	}
	limit, err := optionalPositiveInt(r.URL.Query().Get("limit"), 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	offset, err := optionalNonNegativeInt(r.URL.Query().Get("offset"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	artifacts, err := s.store.ListSourceArtifacts(r.Context(), id, limit, offset)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("source %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, artifacts)
}

type searchRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type documentProfileRequest struct {
	Desc string `json:"desc"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("query is required"))
		return
	}

	result, err := s.store.SearchSectionsWithOptions(r.Context(), storage.SearchOptions{
		Query:                  req.Query,
		Limit:                  req.Limit,
		MaxSearches:            5,
		MaxSectionsPerDocument: 5,
		ProfileDetail:          "compact",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleGetDocumentProfile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("document id is required"))
		return
	}
	profile, err := s.store.GetDocumentProfile(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("document %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (s *Server) handleUpdateDocumentProfile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("document id is required"))
		return
	}
	var req documentProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	profile, err := s.store.UpdateDocumentProfileDesc(r.Context(), storage.DocumentProfileInput{
		DocumentID: id,
		Desc:       req.Desc,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("document %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	var req query.ContextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}

	pack, err := query.NewService(s.store).Context(r.Context(), req)
	if err != nil {
		if strings.Contains(err.Error(), "required") {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, pack)
}

func (s *Server) handleSearchNodes(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]any{"nodes": []storage.Node{}})
		return
	}
	limit, err := optionalPositiveInt(r.URL.Query().Get("limit"), 20)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	nodes, err := s.store.SearchNodes(r.Context(), query, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("node id is required"))
		return
	}

	node, err := s.store.GetNode(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("node %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, node)
}

func (s *Server) handleRelatedNodes(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("node id is required"))
		return
	}

	opts, err := relatedOptionsFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	related, err := s.store.RelatedNodes(r.Context(), id, opts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"related": related})
}

func relatedOptionsFromQuery(r *http.Request) (storage.RelatedOptions, error) {
	values := r.URL.Query()
	opts := storage.RelatedOptions{
		Direction: strings.TrimSpace(values.Get("direction")),
		Kind:      strings.TrimSpace(values.Get("kind")),
	}
	if rawLimit := strings.TrimSpace(values.Get("limit")); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil {
			return storage.RelatedOptions{}, fmt.Errorf("limit must be an integer")
		}
		opts.Limit = limit
	}
	return opts, nil
}

type impactRequest struct {
	ID        string `json:"id"`
	Direction string `json:"direction"`
	Kind      string `json:"kind"`
	MaxDepth  int    `json:"max_depth"`
	Limit     int    `json:"limit"`
}

func (s *Server) handleImpact(w http.ResponseWriter, r *http.Request) {
	var req impactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("node id is required"))
		return
	}

	result, err := s.store.Impact(r.Context(), req.ID, storage.ImpactOptions{
		Direction: strings.TrimSpace(req.Direction),
		Kind:      strings.TrimSpace(req.Kind),
		MaxDepth:  req.MaxDepth,
		Limit:     req.Limit,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("node %q not found", req.ID))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleCreateFeedback(w http.ResponseWriter, r *http.Request) {
	var req feedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}

	payloadJSON := strings.TrimSpace(req.PayloadJSON)
	if payloadJSON == "" && len(req.Payload) > 0 {
		payloadJSON = string(req.Payload)
	}
	event, err := s.store.CreateFeedbackEvent(r.Context(), storage.FeedbackEventInput{
		ID:           ids.Random("fb", 12),
		TargetKind:   strings.TrimSpace(req.TargetKind),
		TargetID:     strings.TrimSpace(req.TargetID),
		FeedbackKind: strings.TrimSpace(req.FeedbackKind),
		PayloadJSON:  payloadJSON,
		Actor:        strings.TrimSpace(req.Actor),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, event)
}

func (s *Server) handleListFeedback(w http.ResponseWriter, r *http.Request) {
	limit, err := optionalPositiveInt(r.URL.Query().Get("limit"), 20)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	events, err := s.store.ListFeedbackEvents(r.Context(), storage.FeedbackListOptions{
		TargetKind:   strings.TrimSpace(r.URL.Query().Get("target_kind")),
		TargetID:     strings.TrimSpace(r.URL.Query().Get("target_id")),
		FeedbackKind: strings.TrimSpace(r.URL.Query().Get("feedback_kind")),
		Limit:        limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"feedback": events})
}

func (s *Server) handleDeleteFeedback(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteFeedbackEvent(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) handleNotImplemented(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": map[string]string{
				"code":    "not_implemented",
				"message": fmt.Sprintf("%s is not implemented yet", name),
			},
		})
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start).String())
	})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	if s.auth.Mode != "token" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		if !tokenMatches(r, s.auth.Token) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]string{
					"code":    http.StatusText(http.StatusUnauthorized),
					"message": "authentication required",
				},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func tokenMatches(r *http.Request, want string) bool {
	got := strings.TrimSpace(r.Header.Get("X-DocGraph-Token"))
	if got == "" {
		got = strings.TrimSpace(r.Header.Get("X-ProductGraph-Token"))
	}
	if got == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			got = strings.TrimSpace(auth[len("bearer "):])
		}
	}
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func optionalPositiveInt(raw string, fallback int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid positive integer %q", raw)
	}
	if value == 0 {
		return fallback, nil
	}
	return value, nil
}

func optionalNonNegativeInt(raw string, fallback int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid non-negative integer %q", raw)
	}
	return value, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    http.StatusText(status),
			"message": err.Error(),
		},
	})
}
