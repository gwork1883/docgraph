package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/docgraph/docgraph/internal/syncschedule"
	"github.com/docgraph/docgraph/internal/web"
)

type Server struct {
	addr      string
	store     storage.Store
	logger    *slog.Logger
	auth      config.AuthConfig
	webPrefix string
	mcpServer *mcp.SSEServer
	mcpHTTP   *mcp.StreamableHTTPServer
	jobs      JobController
}

type JobController interface {
	CancelJob(ctx context.Context, id string, reason string) (storage.Job, error)
}

func New(addr string, store storage.Store, logger *slog.Logger) *Server {
	return NewWithAuthAndPrefix(addr, store, logger, config.AuthConfig{Mode: "none"}, "")
}

func NewWithAuth(addr string, store storage.Store, logger *slog.Logger, auth config.AuthConfig) *Server {
	return NewWithAuthAndPrefix(addr, store, logger, auth, "")
}

func NewWithAuthAndPrefix(addr string, store storage.Store, logger *slog.Logger, auth config.AuthConfig, webPrefix string) *Server {
	return NewWithAuthAndPrefixAndJobs(addr, store, logger, auth, webPrefix, nil)
}

func NewWithAuthAndPrefixAndJobs(addr string, store storage.Store, logger *slog.Logger, auth config.AuthConfig, webPrefix string, jobs JobController) *Server {
	if auth.Mode == "" {
		auth.Mode = "none"
	}
	webPrefix = cleanWebPrefix(webPrefix)
	queryService := query.NewService(store)
	mcpHandler := mcp.NewHandler(queryService, store)
	return &Server{
		addr:      addr,
		store:     store,
		logger:    logger,
		auth:      auth,
		webPrefix: webPrefix,
		mcpServer: mcp.NewSSEServerWithBasePath(mcpHandler, logger, webPrefix),
		mcpHTTP:   mcp.NewStreamableHTTPServer(mcpHandler, logger),
		jobs:      jobs,
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

	mux.HandleFunc(s.pattern("GET", "/api/health"), s.handleHealth)
	mux.HandleFunc(s.pattern("GET", "/api/status"), s.handleStatus)
	mux.HandleFunc(s.pattern("POST", "/api/search"), s.handleSearch)
	mux.HandleFunc(s.pattern("POST", "/api/context"), s.handleContext)
	mux.HandleFunc(s.pattern("GET", "/api/jobs"), s.handleListJobs)
	mux.HandleFunc(s.pattern("GET", "/api/jobs/{id}"), s.handleGetJob)
	mux.HandleFunc(s.pattern("POST", "/api/jobs/{id}/cancel"), s.handleCancelJob)
	mux.HandleFunc(s.pattern("POST", "/api/sync-tasks"), s.handleCreateSyncTask)
	mux.HandleFunc(s.pattern("GET", "/api/sync-schedules"), s.handleListSyncSchedules)
	mux.HandleFunc(s.pattern("POST", "/api/sync-schedules"), s.handleUpsertSyncSchedule)
	mux.HandleFunc(s.pattern("PUT", "/api/sync-schedules/{sourceID}"), s.handleUpsertSyncSchedule)
	mux.HandleFunc(s.pattern("DELETE", "/api/sync-schedules/{sourceID}"), s.handleDeleteSyncSchedule)
	mux.HandleFunc(s.pattern("POST", "/api/maintenance/profile-rebuild"), s.handleEnqueueMaintenanceJob("maintenance_profile_rebuild"))
	mux.HandleFunc(s.pattern("POST", "/api/maintenance/graph-repair"), s.handleEnqueueMaintenanceJob("maintenance_graph_repair"))
	mux.HandleFunc(s.pattern("GET", "/api/sources"), s.handleListSources)
	mux.HandleFunc(s.pattern("POST", "/api/sources"), s.handleCreateSource)
	mux.HandleFunc(s.pattern("GET", "/api/confluence-cookie-credentials"), s.handleListConfluenceCookieCredentials)
	mux.HandleFunc(s.pattern("POST", "/api/confluence-cookie-credentials"), s.handleCreateConfluenceCookieCredential)
	mux.HandleFunc(s.pattern("GET", "/api/confluence-cookie-credentials/{id}"), s.handleGetConfluenceCookieCredential)
	mux.HandleFunc(s.pattern("PUT", "/api/confluence-cookie-credentials/{id}"), s.handleUpdateConfluenceCookieCredential)
	mux.HandleFunc(s.pattern("DELETE", "/api/confluence-cookie-credentials/{id}"), s.handleDeleteConfluenceCookieCredential)
	mux.HandleFunc(s.pattern("GET", "/api/confluence-cookie-credentials/{id}/sources"), s.handleListConfluenceCookieCredentialSources)
	mux.HandleFunc(s.pattern("POST", "/api/confluence-cookie-credentials/{id}/attach-sources"), s.handleAttachConfluenceCookieCredentialSources)
	mux.HandleFunc(s.pattern("POST", "/api/confluence-cookie-credentials/{id}/resume-sources"), s.handleResumeConfluenceCookieCredentialSources)
	mux.HandleFunc(s.pattern("PUT", "/api/sources/{id}"), s.handleUpdateSource)
	mux.HandleFunc(s.pattern("DELETE", "/api/sources/{id}"), s.handleDeleteSource)
	mux.HandleFunc(s.pattern("POST", "/api/sources/{id}/sync"), s.handleSyncSource)
	mux.HandleFunc(s.pattern("GET", "/api/sources/{id}/jobs"), s.handleListSourceJobs)
	mux.HandleFunc(s.pattern("POST", "/api/sources/{id}/jobs/{jobID}/cancel"), s.handleCancelSourceJob)
	mux.HandleFunc(s.pattern("DELETE", "/api/sources/{id}/jobs/{jobID}"), s.handleDeleteSourceJob)
	mux.HandleFunc(s.pattern("GET", "/api/sources/{id}/artifacts"), s.handleListSourceArtifacts)
	mux.HandleFunc(s.pattern("GET", "/api/sources/{id}/artifacts/counts"), s.handleGetSourceArtifactCounts)
	mux.HandleFunc(s.pattern("GET", "/api/sources/{id}/health"), s.handleGetSourceHealth)
	mux.HandleFunc(s.pattern("GET", "/api/sources/{id}/documents"), s.handleListSourceDocuments)
	mux.HandleFunc(s.pattern("GET", "/api/sources/{id}/sections"), s.handleListSourceSections)
	mux.HandleFunc(s.pattern("GET", "/api/sources/{id}/nodes"), s.handleListSourceNodes)
	mux.HandleFunc(s.pattern("GET", "/api/sources/{id}/edges"), s.handleListSourceEdges)
	mux.HandleFunc(s.pattern("GET", "/api/documents/{id}"), s.handleGetDocument)
	mux.HandleFunc(s.pattern("GET", "/api/documents/{id}/profile"), s.handleGetDocumentProfile)
	mux.HandleFunc(s.pattern("PUT", "/api/documents/{id}/profile"), s.handleUpdateDocumentProfile)
	mux.HandleFunc(s.pattern("POST", "/api/knowledge-relation-proposals"), s.handleCreateKnowledgeRelationProposal)
	mux.HandleFunc(s.pattern("GET", "/api/knowledge-relation-proposals"), s.handleListKnowledgeRelationProposals)
	mux.HandleFunc(s.pattern("GET", "/api/knowledge-relation-proposals/{id}"), s.handleGetKnowledgeRelationProposal)
	mux.HandleFunc(s.pattern("POST", "/api/knowledge-relation-proposals/{id}/approve"), s.handleApproveKnowledgeRelationProposal)
	mux.HandleFunc(s.pattern("POST", "/api/knowledge-relation-proposals/{id}/reject"), s.handleRejectKnowledgeRelationProposal)
	mux.HandleFunc(s.pattern("POST", "/api/knowledge-relation-proposals/{id}/cancel"), s.handleCancelKnowledgeRelationProposal)
	mux.HandleFunc(s.pattern("GET", "/api/knowledge-relations"), s.handleListKnowledgeRelations)
	mux.HandleFunc(s.pattern("POST", "/api/knowledge-relations/{id}/disable"), s.handleDisableKnowledgeRelation)
	mux.HandleFunc(s.pattern("POST", "/api/knowledge-relations/{id}/reenable"), s.handleReenableKnowledgeRelation)
	mux.HandleFunc(s.pattern("GET", "/api/nodes"), s.handleSearchNodes)
	mux.HandleFunc(s.pattern("GET", "/api/nodes/{id}"), s.handleGetNode)
	mux.HandleFunc(s.pattern("GET", "/api/nodes/{id}/related"), s.handleRelatedNodes)
	mux.HandleFunc(s.pattern("POST", "/api/impact"), s.handleImpact)
	mux.HandleFunc(s.pattern("POST", "/api/feedback"), s.handleCreateFeedback)
	mux.HandleFunc(s.pattern("GET", "/api/feedback"), s.handleListFeedback)
	mux.HandleFunc(s.pattern("DELETE", "/api/feedback/{id}"), s.handleDeleteFeedback)
	s.mcpHTTP.RegisterRoutesWithPrefix(mux, s.webPrefix)
	s.mcpServer.RegisterRoutesWithPrefix(mux, s.webPrefix)
	staticHandler := http.FileServerFS(staticFS)
	if s.webPrefix == "" {
		mux.Handle("/", staticHandler)
	} else {
		mux.HandleFunc("GET "+s.webPrefix, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, s.webPrefix+"/", http.StatusMovedPermanently)
		})
		mux.Handle("GET "+s.webPrefix+"/", http.StripPrefix(s.webPrefix, staticHandler))
	}

	return s.requireAuth(mux), nil
}

func (s *Server) pattern(method, path string) string {
	return method + " " + s.routePath(path)
}

func (s *Server) routePath(path string) string {
	if s.webPrefix == "" {
		return path
	}
	if path == "/" {
		return s.webPrefix + "/"
	}
	return s.webPrefix + path
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

func (s *Server) handleListConfluenceCookieCredentials(w http.ResponseWriter, r *http.Request) {
	credentials, err := s.store.ListConfluenceCookieCredentials(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sources, err := s.store.ListSources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	responses := make([]confluenceCookieCredentialResponse, 0, len(credentials))
	for _, credential := range credentials {
		responses = append(responses, confluenceCookieCredentialResponseFor(credential, countSourcesForConfluenceCookieCredential(sources, credential.ID)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": responses})
}

func (s *Server) handleCreateConfluenceCookieCredential(w http.ResponseWriter, r *http.Request) {
	var req confluenceCookieCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	cookie := ""
	if req.Cookie != nil {
		cookie = strings.TrimSpace(*req.Cookie)
	}
	credential, err := s.store.CreateConfluenceCookieCredential(r.Context(), storage.ConfluenceCookieCredential{
		ID:      ids.Random("cred", 12),
		Name:    strings.TrimSpace(req.Name),
		BaseURL: strings.TrimSpace(req.BaseURL),
		Cookie:  cookie,
		Notes:   strings.TrimSpace(req.Notes),
		Status:  "unknown",
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, confluenceCookieCredentialResponseFor(credential, 0))
}

func (s *Server) handleGetConfluenceCookieCredential(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	credential, err := s.store.GetConfluenceCookieCredential(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("credential %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sources, err := s.store.ListSources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, confluenceCookieCredentialResponseFor(credential, countSourcesForConfluenceCookieCredential(sources, credential.ID)))
}

func (s *Server) handleUpdateConfluenceCookieCredential(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	var req confluenceCookieCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	existing, err := s.store.GetConfluenceCookieCredential(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("credential %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cookie := existing.Cookie
	if req.Cookie != nil && strings.TrimSpace(*req.Cookie) != "" {
		cookie = strings.TrimSpace(*req.Cookie)
	}
	updated, err := s.store.UpdateConfluenceCookieCredential(r.Context(), storage.ConfluenceCookieCredential{
		ID:        id,
		Name:      firstNonEmpty(req.Name, existing.Name),
		BaseURL:   firstNonEmpty(req.BaseURL, existing.BaseURL),
		Cookie:    cookie,
		Notes:     strings.TrimSpace(req.Notes),
		Status:    "unknown",
		LastError: "",
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sources, err := s.store.ListSources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, confluenceCookieCredentialResponseFor(updated, countSourcesForConfluenceCookieCredential(sources, updated.ID)))
}

func (s *Server) handleDeleteConfluenceCookieCredential(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	sources, err := s.sourcesForConfluenceCookieCredential(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(sources) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]string{
				"code":    http.StatusText(http.StatusConflict),
				"message": "credential is still referenced by confluence sources",
			},
			"sources": sources,
		})
		return
	}
	if err := s.store.DeleteConfluenceCookieCredential(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("credential %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (s *Server) handleListConfluenceCookieCredentialSources(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if _, err := s.store.GetConfluenceCookieCredential(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("credential %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sources, err := s.sourcesForConfluenceCookieCredential(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

func (s *Server) handleAttachConfluenceCookieCredentialSources(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	credential, err := s.store.GetConfluenceCookieCredential(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("credential %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var req attachConfluenceCookieCredentialSourcesRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	sources, err := s.store.ListSources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	selected := map[string]bool{}
	for _, sourceID := range req.SourceIDs {
		if sourceID = strings.TrimSpace(sourceID); sourceID != "" {
			selected[sourceID] = true
		}
	}
	attached := []string{}
	for _, source := range sources {
		if source.Kind != "confluence" {
			continue
		}
		if len(selected) > 0 {
			if !selected[source.ID] {
				continue
			}
		} else if !confluenceSourceMatchesBaseURL(source, credential.BaseURL) {
			continue
		}
		updated, changed, err := sourceWithConfluenceCookieCredential(source, credential.ID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if !changed {
			continue
		}
		if _, err := s.store.UpdateSource(r.Context(), updated); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		attached = append(attached, source.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"attached": attached})
}

func (s *Server) handleResumeConfluenceCookieCredentialSources(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if _, err := s.store.GetConfluenceCookieCredential(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("credential %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sources, err := s.sourcesForConfluenceCookieCredential(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	resumed := []string{}
	for _, source := range sources {
		if source.SyncStatus == "paused" && source.SyncStatusReason == "credential_required" {
			if err := s.store.UpdateSourceSyncState(r.Context(), source.ID, "active", ""); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			resumed = append(resumed, source.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"resumed": resumed})
}

type createSourceRequest struct {
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	DSN                string `json:"dsn"`
	ConfigJSON         string `json:"config_json"`
	ProductHint        string `json:"product_hint"`
	ModuleHint         string `json:"module_hint"`
	SyncSchedule       string `json:"sync_schedule"`
	Branch             string `json:"branch"`
	Path               string `json:"path"`
	Cache              string `json:"cache"`
	Include            string `json:"include"`
	Exclude            string `json:"exclude"`
	URLPrefix          string `json:"url_prefix"`
	IdentityFile       string `json:"identity_file"`
	Password           string `json:"password"`
	Passphrase         string `json:"passphrase"`
	KnownHosts         string `json:"known_hosts"`
	StrictHostKey      bool   `json:"strict_host_key"`
	BaseURL            string `json:"base_url"`
	PageID             string `json:"page_id"`
	SpaceKey           string `json:"space_key"`
	Token              string `json:"token"`
	Username           string `json:"username"`
	APIToken           string `json:"api_token"`
	CookieCredentialID string `json:"cookie_credential_id"`
	IncludeChildren    *bool  `json:"include_children"`
	MaxPages           string `json:"max_pages"`
	MaxDepth           string `json:"max_depth"`
	BearerToken        string `json:"bearer_token"`
	Cookie             string `json:"cookie"`
	HeadersJSON        string `json:"headers_json"`
	CrawlMode          string `json:"crawl_mode"`
	IsSPA              *bool  `json:"is_spa"`
}

type updateSourceRequest struct {
	Kind               *string `json:"kind"`
	Name               *string `json:"name"`
	DSN                *string `json:"dsn"`
	ConfigJSON         *string `json:"config_json"`
	ProductHint        *string `json:"product_hint"`
	ModuleHint         *string `json:"module_hint"`
	SyncSchedule       *string `json:"sync_schedule"`
	Branch             *string `json:"branch"`
	Path               *string `json:"path"`
	Cache              *string `json:"cache"`
	Include            *string `json:"include"`
	Exclude            *string `json:"exclude"`
	URLPrefix          *string `json:"url_prefix"`
	IdentityFile       *string `json:"identity_file"`
	Password           *string `json:"password"`
	Passphrase         *string `json:"passphrase"`
	KnownHosts         *string `json:"known_hosts"`
	StrictHostKey      *bool   `json:"strict_host_key"`
	BaseURL            *string `json:"base_url"`
	PageID             *string `json:"page_id"`
	SpaceKey           *string `json:"space_key"`
	Token              *string `json:"token"`
	Username           *string `json:"username"`
	APIToken           *string `json:"api_token"`
	CookieCredentialID *string `json:"cookie_credential_id"`
	IncludeChildren    *bool   `json:"include_children"`
	MaxPages           *string `json:"max_pages"`
	MaxDepth           *string `json:"max_depth"`
	BearerToken        *string `json:"bearer_token"`
	Cookie             *string `json:"cookie"`
	HeadersJSON        *string `json:"headers_json"`
	CrawlMode          *string `json:"crawl_mode"`
	IsSPA              *bool   `json:"is_spa"`
}

type confluenceCookieCredentialRequest struct {
	Name    string  `json:"name"`
	BaseURL string  `json:"base_url"`
	Cookie  *string `json:"cookie"`
	Notes   string  `json:"notes"`
}

type attachConfluenceCookieCredentialSourcesRequest struct {
	SourceIDs []string `json:"source_ids"`
}

type confluenceCookieCredentialResponse struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	BaseURL         string `json:"base_url"`
	Notes           string `json:"notes"`
	Status          string `json:"status"`
	LastValidatedAt string `json:"last_validated_at"`
	LastError       string `json:"last_error"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	HasCookie       bool   `json:"has_cookie"`
	CookiePreview   string `json:"cookie_preview,omitempty"`
	SourceCount     int    `json:"source_count"`
}

type feedbackRequest struct {
	TargetKind   string          `json:"target_kind"`
	TargetID     string          `json:"target_id"`
	FeedbackKind string          `json:"feedback_kind"`
	Payload      json.RawMessage `json:"payload"`
	PayloadJSON  string          `json:"payload_json"`
	Actor        string          `json:"actor"`
}

type syncTaskRequest struct {
	SourceID           string `json:"source_id"`
	CookieCredentialID string `json:"cookie_credential_id"`
}

type syncScheduleRequest struct {
	SourceID           string `json:"source_id"`
	SyncSchedule       string `json:"sync_schedule"`
	CookieCredentialID string `json:"cookie_credential_id"`
}

type syncScheduleResponse struct {
	SourceID           string           `json:"source_id"`
	SourceName         string           `json:"source_name"`
	SourceKind         string           `json:"source_kind"`
	SyncSchedule       string           `json:"sync_schedule"`
	Enabled            bool             `json:"enabled"`
	Paused             bool             `json:"paused"`
	PausedReason       string           `json:"paused_reason,omitempty"`
	LastJob            *storage.SyncJob `json:"last_job,omitempty"`
	LastRunAt          string           `json:"last_run_at,omitempty"`
	NextRunAt          string           `json:"next_run_at,omitempty"`
	InProgress         bool             `json:"in_progress"`
	LastError          string           `json:"last_error,omitempty"`
	CookieCredentialID string           `json:"cookie_credential_id,omitempty"`
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
	if err := validateSyncSchedule(req.SyncSchedule); err != nil {
		writeError(w, http.StatusBadRequest, err)
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
			"branch":               req.Branch,
			"path":                 req.Path,
			"cache":                req.Cache,
			"include":              req.Include,
			"exclude":              req.Exclude,
			"url_prefix":           req.URLPrefix,
			"identity_file":        req.IdentityFile,
			"password":             req.Password,
			"passphrase":           req.Passphrase,
			"known_hosts":          req.KnownHosts,
			"base_url":             req.BaseURL,
			"page_id":              req.PageID,
			"space_key":            req.SpaceKey,
			"token":                req.Token,
			"username":             req.Username,
			"api_token":            req.APIToken,
			"cookie_credential_id": req.CookieCredentialID,
			"max_pages":            req.MaxPages,
			"max_depth":            req.MaxDepth,
			"bearer_token":         req.BearerToken,
			"cookie":               req.Cookie,
			"headers_json":         req.HeadersJSON,
			"crawl_mode":           req.CrawlMode,
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
	if req.BaseURL != nil || req.PageID != nil || req.SpaceKey != nil || req.Token != nil || req.Username != nil || req.APIToken != nil || req.CookieCredentialID != nil || req.IncludeChildren != nil || req.MaxPages != nil || req.MaxDepth != nil || req.BearerToken != nil || req.Cookie != nil || req.HeadersJSON != nil || req.CrawlMode != nil || req.IsSPA != nil {
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
		addOptionalString(stringsMap, "cookie_credential_id", req.CookieCredentialID)
		addOptionalString(stringsMap, "max_pages", req.MaxPages)
		addOptionalString(stringsMap, "max_depth", req.MaxDepth)
		addOptionalString(stringsMap, "bearer_token", req.BearerToken)
		addOptionalString(stringsMap, "cookie", req.Cookie)
		addOptionalString(stringsMap, "headers_json", req.HeadersJSON)
		addOptionalString(stringsMap, "crawl_mode", req.CrawlMode)
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
	if err := validateSyncSchedule(source.SyncSchedule); err != nil {
		writeError(w, http.StatusBadRequest, err)
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

func validateSyncSchedule(raw string) error {
	_, _, err := syncschedule.Parse(raw)
	return err
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

func firstNonEmpty(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return strings.TrimSpace(fallback)
	}
	return value
}

func confluenceCookieCredentialResponseFor(credential storage.ConfluenceCookieCredential, sourceCount int) confluenceCookieCredentialResponse {
	return confluenceCookieCredentialResponse{
		ID:              credential.ID,
		Name:            credential.Name,
		BaseURL:         credential.BaseURL,
		Notes:           credential.Notes,
		Status:          credential.Status,
		LastValidatedAt: credential.LastValidatedAt,
		LastError:       credential.LastError,
		CreatedAt:       credential.CreatedAt,
		UpdatedAt:       credential.UpdatedAt,
		HasCookie:       strings.TrimSpace(credential.Cookie) != "",
		CookiePreview:   cookiePreview(credential.Cookie),
		SourceCount:     sourceCount,
	}
}

func cookiePreview(cookie string) string {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return ""
	}
	parts := strings.Split(cookie, ";")
	first := strings.TrimSpace(parts[0])
	if first == "" {
		return "set"
	}
	if len(first) > 18 {
		first = first[:18]
	}
	return first + "..."
}

func countSourcesForConfluenceCookieCredential(sources []storage.Source, credentialID string) int {
	count := 0
	for _, source := range sources {
		if sourceUsesConfluenceCookieCredential(source, credentialID) {
			count++
		}
	}
	return count
}

func (s *Server) sourcesForConfluenceCookieCredential(ctx context.Context, credentialID string) ([]storage.Source, error) {
	sources, err := s.store.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]storage.Source, 0)
	for _, source := range sources {
		if sourceUsesConfluenceCookieCredential(source, credentialID) {
			out = append(out, source)
		}
	}
	return out, nil
}

func sourceUsesConfluenceCookieCredential(source storage.Source, credentialID string) bool {
	if source.Kind != "confluence" || strings.TrimSpace(credentialID) == "" {
		return false
	}
	config := map[string]any{}
	if err := json.Unmarshal([]byte(defaultJSON(source.ConfigJSON)), &config); err != nil {
		return false
	}
	return stringFromMap(config, "cookie_credential_id") == credentialID
}

func sourceWithConfluenceCookieCredential(source storage.Source, credentialID string) (storage.Source, bool, error) {
	config := map[string]any{}
	if err := json.Unmarshal([]byte(defaultJSON(source.ConfigJSON)), &config); err != nil {
		return storage.Source{}, false, fmt.Errorf("parse source config_json: %w", err)
	}
	if stringFromMap(config, "cookie_credential_id") == credentialID && stringFromMap(config, "cookie") == "" {
		return source, false, nil
	}
	config["cookie_credential_id"] = credentialID
	delete(config, "cookie")
	data, err := json.Marshal(config)
	if err != nil {
		return storage.Source{}, false, err
	}
	source.ConfigJSON = string(data)
	return source, true, nil
}

func confluenceSourceMatchesBaseURL(source storage.Source, baseURL string) bool {
	want := normalizedURLForMatch(baseURL)
	if want == "" {
		return false
	}
	config := map[string]any{}
	_ = json.Unmarshal([]byte(defaultJSON(source.ConfigJSON)), &config)
	for _, candidate := range []string{
		stringFromMap(config, "base_url"),
		source.DSN,
	} {
		if normalizedURLForMatch(candidate) == want {
			return true
		}
	}
	return false
}

func normalizedURLForMatch(value string) string {
	value = strings.TrimSpace(strings.TrimRight(value, "/"))
	if value == "" {
		return ""
	}
	return strings.ToLower(value)
}

func stringFromMap(value map[string]any, key string) string {
	if raw, ok := value[key].(string); ok {
		return strings.TrimSpace(raw)
	}
	return ""
}

func defaultJSON(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "{}"
	}
	return value
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
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
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
	job, err := s.store.CreateSyncJobIfIdle(r.Context(), source.ID)
	if err != nil {
		if errors.Is(err, storage.ErrSyncInProgress) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	limit, err := optionalPositiveInt(r.URL.Query().Get("limit"), 20)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if limit > 100 {
		limit = 100
	}
	offset, err := optionalNonNegativeInt(r.URL.Query().Get("offset"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	opts := storage.JobListOptions{
		SourceID:   r.URL.Query().Get("source_id"),
		Kind:       r.URL.Query().Get("kind"),
		Status:     r.URL.Query().Get("status"),
		TargetKind: r.URL.Query().Get("target_kind"),
		TargetID:   r.URL.Query().Get("target_id"),
		Limit:      limit,
		Offset:     offset,
	}
	jobs, err := s.store.ListJobs(r.Context(), opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	total, err := s.store.CountJobs(r.Context(), opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":     jobs,
		"limit":    limit,
		"offset":   offset,
		"total":    total,
		"has_more": offset+len(jobs) < total,
	})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("job id is required"))
		return
	}
	job, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("job %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("job id is required"))
		return
	}
	job, err := s.cancelJob(r.Context(), id, "canceled by user")
	if err != nil {
		writeCancelJobError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

func (s *Server) handleEnqueueMaintenanceJob(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, err := s.store.CreateJob(r.Context(), storage.JobInput{Kind: kind})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
	}
}

func (s *Server) handleCreateSyncTask(w http.ResponseWriter, r *http.Request) {
	var req syncTaskRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
			return
		}
	}
	sourceID := strings.TrimSpace(req.SourceID)
	if sourceID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
		return
	}
	source, err := s.sourceWithOptionalCredential(r.Context(), sourceID, req.CookieCredentialID)
	if err != nil {
		writeSyncTaskSourceError(w, sourceID, err)
		return
	}
	job, err := s.store.CreateSyncJobIfIdle(r.Context(), source.ID)
	if err != nil {
		if errors.Is(err, storage.ErrSyncInProgress) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) handleListSyncSchedules(w http.ResponseWriter, r *http.Request) {
	sources, err := s.store.ListSources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	schedules := make([]syncScheduleResponse, 0, len(sources))
	now := time.Now()
	for _, source := range sources {
		response, err := s.syncScheduleResponse(r.Context(), source, now)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		schedules = append(schedules, response)
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": schedules})
}

func (s *Server) handleUpsertSyncSchedule(w http.ResponseWriter, r *http.Request) {
	var req syncScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	pathSourceID := strings.TrimSpace(r.PathValue("sourceID"))
	if pathSourceID != "" {
		req.SourceID = pathSourceID
	}
	sourceID := strings.TrimSpace(req.SourceID)
	if sourceID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
		return
	}
	schedule := strings.TrimSpace(req.SyncSchedule)
	if err := validateSyncSchedule(schedule); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	source, err := s.sourceWithOptionalCredential(r.Context(), sourceID, req.CookieCredentialID)
	if err != nil {
		writeSyncTaskSourceError(w, sourceID, err)
		return
	}
	source.SyncSchedule = schedule
	updated, err := s.store.UpdateSource(r.Context(), source)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	response, err := s.syncScheduleResponse(r.Context(), updated, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedule": response})
}

func (s *Server) handleDeleteSyncSchedule(w http.ResponseWriter, r *http.Request) {
	sourceID := strings.TrimSpace(r.PathValue("sourceID"))
	if sourceID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
		return
	}
	source, err := s.store.GetSource(r.Context(), sourceID)
	if err != nil {
		writeSyncTaskSourceError(w, sourceID, err)
		return
	}
	source.SyncSchedule = ""
	updated, err := s.store.UpdateSource(r.Context(), source)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	response, err := s.syncScheduleResponse(r.Context(), updated, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedule": response})
}

func (s *Server) sourceWithOptionalCredential(ctx context.Context, sourceID string, credentialID string) (storage.Source, error) {
	source, err := s.store.GetSource(ctx, strings.TrimSpace(sourceID))
	if err != nil {
		return storage.Source{}, err
	}
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return source, nil
	}
	if source.Kind != "confluence" {
		return storage.Source{}, fmt.Errorf("cookie credentials can only be used with confluence sources")
	}
	if _, err := s.store.GetConfluenceCookieCredential(ctx, credentialID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storage.Source{}, fmt.Errorf("credential %q not found", credentialID)
		}
		return storage.Source{}, err
	}
	updated, changed, err := sourceWithConfluenceCookieCredential(source, credentialID)
	if err != nil {
		return storage.Source{}, err
	}
	if changed {
		source, err = s.store.UpdateSource(ctx, updated)
		if err != nil {
			return storage.Source{}, err
		}
	}
	return source, nil
}

func (s *Server) syncScheduleResponse(ctx context.Context, source storage.Source, now time.Time) (syncScheduleResponse, error) {
	interval, enabled, err := syncschedule.Parse(source.SyncSchedule)
	if err != nil {
		return syncScheduleResponse{}, err
	}
	jobs, err := s.store.ListSyncJobs(ctx, source.ID, 1)
	if err != nil {
		return syncScheduleResponse{}, err
	}
	var lastJob *storage.SyncJob
	var lastRunAt string
	var nextRunAt string
	var lastError string
	inProgress := false
	if len(jobs) > 0 {
		job := jobs[0]
		lastJob = &job
		inProgress = job.Status == "queued" || job.Status == "running"
		lastError = strings.TrimSpace(job.LastError)
		lastRunAt = firstJobTime(job.UpdatedAt, job.CreatedAt)
		if enabled && !inProgress {
			if parsed, ok := parseAPITime(lastRunAt); ok {
				nextRunAt = parsed.Add(interval).Format(time.RFC3339)
			}
		}
	} else if enabled {
		nextRunAt = now.Format(time.RFC3339)
	}
	if enabled && nextRunAt == "" && !inProgress {
		nextRunAt = now.Format(time.RFC3339)
	}
	if source.SyncStatus == "paused" {
		nextRunAt = ""
	}
	return syncScheduleResponse{
		SourceID:           source.ID,
		SourceName:         source.Name,
		SourceKind:         source.Kind,
		SyncSchedule:       strings.TrimSpace(source.SyncSchedule),
		Enabled:            enabled,
		Paused:             source.SyncStatus == "paused",
		PausedReason:       source.SyncStatusReason,
		LastJob:            lastJob,
		LastRunAt:          lastRunAt,
		NextRunAt:          nextRunAt,
		InProgress:         inProgress,
		LastError:          lastError,
		CookieCredentialID: sourceCookieCredentialID(source),
	}, nil
}

func sourceCookieCredentialID(source storage.Source) string {
	if source.Kind != "confluence" {
		return ""
	}
	config := map[string]any{}
	if err := json.Unmarshal([]byte(defaultJSON(source.ConfigJSON)), &config); err != nil {
		return ""
	}
	return stringFromMap(config, "cookie_credential_id")
}

func firstJobTime(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			if parsed, ok := parseAPITime(value); ok {
				return parsed.Format(time.RFC3339)
			}
			return value
		}
	}
	return ""
}

func parseAPITime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func writeSyncTaskSourceError(w http.ResponseWriter, sourceID string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Errorf("source %q not found", sourceID))
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func (s *Server) handleListSourceJobs(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
		return
	}
	limit, err := optionalPositiveInt(r.URL.Query().Get("limit"), 20)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if limit > 100 {
		limit = 100
	}
	offset, err := optionalNonNegativeInt(r.URL.Query().Get("offset"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	opts := storage.JobListOptions{SourceID: id, Kind: "sync_source", Limit: limit, Offset: offset}
	jobs, err := s.store.ListJobs(r.Context(), opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	total, err := s.store.CountJobs(r.Context(), opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":     jobs,
		"limit":    limit,
		"offset":   offset,
		"total":    total,
		"has_more": offset+len(jobs) < total,
	})
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
		if errors.Is(err, storage.ErrJobNotCancelable) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": jobID})
}

func (s *Server) handleCancelSourceJob(w http.ResponseWriter, r *http.Request) {
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
	job, err := s.store.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("job %q not found", jobID))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	jobSourceID := job.SourceID
	if jobSourceID == "" {
		jobSourceID = sourceIDFromPayload(job.PayloadJSON)
	}
	if job.Kind != "sync_source" || jobSourceID != sourceID {
		writeError(w, http.StatusNotFound, fmt.Errorf("sync job %q not found for source %q", jobID, sourceID))
		return
	}
	job, err = s.cancelJob(r.Context(), jobID, "canceled by user")
	if err != nil {
		writeCancelJobError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) cancelJob(ctx context.Context, id string, reason string) (storage.Job, error) {
	if s.jobs != nil {
		return s.jobs.CancelJob(ctx, id, reason)
	}
	return s.store.CancelJob(ctx, id, reason)
}

func writeCancelJobError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if errors.Is(err, storage.ErrJobNotCancelable) {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

func sourceIDFromPayload(payload string) string {
	var value struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value.SourceID)
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

func (s *Server) handleGetSourceArtifactCounts(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
		return
	}
	counts, err := s.store.GetSourceArtifactCounts(r.Context(), id)
	if err != nil {
		writeSourceArtifactError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source_id": id, "counts": counts})
}

func (s *Server) handleGetSourceHealth(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
		return
	}
	health, err := s.store.GetSourceHealth(r.Context(), id)
	if err != nil {
		writeSourceArtifactError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, health)
}

func (s *Server) handleListSourceDocuments(w http.ResponseWriter, r *http.Request) {
	id, limit, offset, ok := sourceArtifactPageFromRequest(w, r)
	if !ok {
		return
	}
	documents, err := s.store.ListSourceDocuments(r.Context(), id, limit, offset)
	if err != nil {
		writeSourceArtifactError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source_id": id, "documents": documents})
}

func (s *Server) handleListSourceSections(w http.ResponseWriter, r *http.Request) {
	id, limit, offset, ok := sourceArtifactPageFromRequest(w, r)
	if !ok {
		return
	}
	sections, err := s.store.ListSourceSections(r.Context(), id, limit, offset)
	if err != nil {
		writeSourceArtifactError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source_id": id, "sections": sections})
}

func (s *Server) handleListSourceNodes(w http.ResponseWriter, r *http.Request) {
	id, limit, offset, ok := sourceArtifactPageFromRequest(w, r)
	if !ok {
		return
	}
	nodes, err := s.store.ListSourceNodes(r.Context(), id, limit, offset)
	if err != nil {
		writeSourceArtifactError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source_id": id, "nodes": nodes})
}

func (s *Server) handleListSourceEdges(w http.ResponseWriter, r *http.Request) {
	id, limit, offset, ok := sourceArtifactPageFromRequest(w, r)
	if !ok {
		return
	}
	edges, err := s.store.ListSourceEdges(r.Context(), id, limit, offset)
	if err != nil {
		writeSourceArtifactError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source_id": id, "edges": edges})
}

func sourceArtifactPageFromRequest(w http.ResponseWriter, r *http.Request) (string, int, int, bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("source id is required"))
		return "", 0, 0, false
	}
	limit, err := optionalPositiveInt(r.URL.Query().Get("limit"), 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return "", 0, 0, false
	}
	offset, err := optionalNonNegativeInt(r.URL.Query().Get("offset"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return "", 0, 0, false
	}
	return id, limit, offset, true
}

func writeSourceArtifactError(w http.ResponseWriter, sourceID string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Errorf("source %q not found", sourceID))
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

type searchRequest struct {
	Query                string   `json:"query"`
	Limit                int      `json:"limit"`
	UseRelationExpansion *bool    `json:"use_relation_expansion"`
	RelationTypes        []string `json:"relation_types"`
}

type documentProfileRequest struct {
	Desc string `json:"desc"`
}

type knowledgeRelationProposalRequest struct {
	RelationType   string          `json:"relation_type"`
	FromDocumentID string          `json:"from_document_id"`
	FromAnchor     string          `json:"from_anchor"`
	ToDocumentID   string          `json:"to_document_id"`
	ToAnchor       string          `json:"to_anchor"`
	Direction      string          `json:"direction"`
	Reason         string          `json:"reason"`
	Evidence       json.RawMessage `json:"evidence"`
	EvidenceJSON   string          `json:"evidence_json"`
	ProposedEffect string          `json:"proposed_effect"`
	Confidence     float64         `json:"confidence"`
	CreatedByType  string          `json:"created_by_type"`
	CreatedByRef   string          `json:"created_by_ref"`
}

type knowledgeRelationReviewRequest struct {
	ReviewedBy string `json:"reviewed_by"`
	ReviewNote string `json:"review_note"`
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

	start := time.Now()
	useRelationExpansion := true
	if req.UseRelationExpansion != nil {
		useRelationExpansion = *req.UseRelationExpansion
	}
	result, err := s.store.SearchSectionsWithOptions(r.Context(), storage.SearchOptions{
		Query:                  req.Query,
		Limit:                  req.Limit,
		MaxSearches:            5,
		MaxSectionsPerDocument: 5,
		ProfileDetail:          "compact",
		UseRelationExpansion:   useRelationExpansion,
		RelationDepth:          1,
		RelationTypes:          req.RelationTypes,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.recordSearchObservation(r.Context(), req.Query, result, time.Since(start))
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) recordSearchObservation(ctx context.Context, queryText string, result storage.SearchResult, elapsed time.Duration) {
	results := make([]storage.SearchResultObservationInput, 0, len(result.Hits))
	for i, hit := range result.Hits {
		results = append(results, storage.SearchResultObservationInput{
			DocumentID: hit.DocumentID,
			SectionID:  hit.SectionID,
			Rank:       i + 1,
			Score:      hit.Rank,
		})
	}
	if err := s.store.RecordQueryObservation(ctx, storage.QueryObservationInput{
		QueryText:       queryText,
		NormalizedQuery: strings.ToLower(strings.TrimSpace(queryText)),
		Source:          "web",
		ResultCount:     len(result.Hits),
		LatencyMS:       elapsed.Milliseconds(),
		Results:         results,
	}); err != nil && s.logger != nil {
		s.logger.Warn("record search observation failed", "err", err)
	}
}

func (s *Server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("document id is required"))
		return
	}
	if truthyQuery(r, "summary") {
		detail, err := s.documentSummary(r.Context(), id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, fmt.Errorf("document %q not found", id))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, detail)
		return
	}
	detail, err := s.documentDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("document %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func truthyQuery(r *http.Request, key string) bool {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(key)))
	return value == "1" || value == "true" || value == "yes"
}

func (s *Server) documentSummary(ctx context.Context, id string) (storage.DocumentDetail, error) {
	doc, err := s.store.GetDocument(ctx, id)
	if err != nil {
		return storage.DocumentDetail{}, err
	}
	source, err := s.store.GetSource(ctx, doc.SourceID)
	if err != nil {
		return storage.DocumentDetail{}, err
	}
	return storage.DocumentDetail{
		Document: doc,
		Source:   source,
	}, nil
}

func (s *Server) documentDetail(ctx context.Context, id string) (storage.DocumentDetail, error) {
	doc, err := s.store.GetDocument(ctx, id)
	if err != nil {
		return storage.DocumentDetail{}, err
	}
	source, err := s.store.GetSource(ctx, doc.SourceID)
	if err != nil {
		return storage.DocumentDetail{}, err
	}
	profile, err := s.store.GetDocumentProfile(ctx, doc.ID)
	if err != nil {
		return storage.DocumentDetail{}, err
	}
	sections, err := s.store.ListDocumentSections(ctx, doc.ID, 200, 0)
	if err != nil {
		return storage.DocumentDetail{}, err
	}
	entities, err := s.store.ListDocumentEntities(ctx, doc.ID)
	if err != nil {
		return storage.DocumentDetail{}, err
	}
	feedback, err := s.store.ListFeedbackEvents(ctx, storage.FeedbackListOptions{
		TargetKind: "document",
		TargetID:   doc.ID,
		Limit:      100,
	})
	if err != nil {
		return storage.DocumentDetail{}, err
	}
	related, err := s.store.RelatedNodes(ctx, doc.NodeID, storage.RelatedOptions{
		Direction: "both",
		Limit:     50,
	})
	if err != nil {
		return storage.DocumentDetail{}, err
	}
	health, err := s.store.GetSourceHealth(ctx, doc.SourceID)
	if err != nil {
		return storage.DocumentDetail{}, err
	}
	diagnostics, brokenLinks := documentDiagnostics(doc, sections, entities, feedback, health)
	return storage.DocumentDetail{
		Document:    doc,
		Source:      source,
		Profile:     profile,
		Sections:    sections,
		Entities:    entities,
		Feedback:    feedback,
		Related:     related,
		Diagnostics: diagnostics,
		LatestJob:   health.LatestJob,
		BrokenLinks: brokenLinks,
	}, nil
}

func documentDiagnostics(doc storage.DocumentSummary, sections []storage.SectionSummary, entities []storage.SectionEntity, feedback []storage.FeedbackEvent, health storage.SourceHealth) ([]storage.SourceHealthWarning, []storage.BrokenLink) {
	diagnostics := make([]storage.SourceHealthWarning, 0)
	if doc.SectionCount == 0 || len(sections) == 0 {
		diagnostics = append(diagnostics, storage.SourceHealthWarning{
			Kind:       "zero_sections",
			Severity:   "warn",
			Message:    "Document has no parsed sections.",
			DocumentID: doc.ID,
		})
	}
	if len(sections) > 0 && len(entities) == 0 {
		diagnostics = append(diagnostics, storage.SourceHealthWarning{
			Kind:       "no_section_entities",
			Severity:   "info",
			Message:    "Document has parsed sections but no extracted technical entities.",
			DocumentID: doc.ID,
		})
	}
	lowCount := 0
	for _, section := range sections {
		if len(strings.TrimSpace(section.ContentSnippet)) < 80 {
			lowCount++
			if lowCount <= 5 {
				diagnostics = append(diagnostics, storage.SourceHealthWarning{
					Kind:       "low_content_section",
					Severity:   "warn",
					Message:    "Section contains very little parsed text.",
					DocumentID: doc.ID,
					SectionID:  section.ID,
				})
			}
		}
	}
	if lowCount > 5 {
		diagnostics = append(diagnostics, storage.SourceHealthWarning{
			Kind:       "low_content_sections",
			Severity:   "warn",
			Message:    "Additional sections contain very little parsed text.",
			DocumentID: doc.ID,
			Count:      lowCount - 5,
		})
	}
	for _, event := range feedback {
		if event.FeedbackKind == "document_stale" {
			diagnostics = append(diagnostics, storage.SourceHealthWarning{
				Kind:       "document_stale",
				Severity:   "info",
				Message:    "Document is manually marked stale and excluded from search.",
				DocumentID: doc.ID,
			})
		}
	}
	brokenLinks := matchingDocumentBrokenLinks(doc, health.BrokenLinks)
	if len(brokenLinks) > 0 {
		diagnostics = append(diagnostics, storage.SourceHealthWarning{
			Kind:       "document_broken_links",
			Severity:   "warn",
			Message:    "Latest sync reported broken links from this document.",
			DocumentID: doc.ID,
			Count:      len(brokenLinks),
		})
	}
	return diagnostics, brokenLinks
}

func matchingDocumentBrokenLinks(doc storage.DocumentSummary, links []storage.BrokenLink) []storage.BrokenLink {
	matches := make([]storage.BrokenLink, 0)
	keys := []string{doc.ID, doc.ExternalID, doc.Title, doc.URL}
	for _, link := range links {
		source := strings.TrimSpace(link.SourceDocument)
		if source == "" {
			continue
		}
		for _, key := range keys {
			key = strings.TrimSpace(key)
			if key != "" && (source == key || strings.Contains(source, key) || strings.Contains(key, source)) {
				matches = append(matches, link)
				break
			}
		}
	}
	return matches
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

func (s *Server) handleCreateKnowledgeRelationProposal(w http.ResponseWriter, r *http.Request) {
	var req knowledgeRelationProposalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	evidenceJSON := strings.TrimSpace(req.EvidenceJSON)
	if evidenceJSON == "" && len(req.Evidence) > 0 {
		evidenceJSON = string(req.Evidence)
	}
	proposal, err := s.store.CreateKnowledgeRelationProposal(r.Context(), storage.KnowledgeRelationProposalInput{
		ID:             ids.Random("krp", 12),
		RelationType:   strings.TrimSpace(req.RelationType),
		FromDocumentID: strings.TrimSpace(req.FromDocumentID),
		FromAnchor:     strings.TrimSpace(req.FromAnchor),
		ToDocumentID:   strings.TrimSpace(req.ToDocumentID),
		ToAnchor:       strings.TrimSpace(req.ToAnchor),
		Direction:      strings.TrimSpace(req.Direction),
		Reason:         strings.TrimSpace(req.Reason),
		EvidenceJSON:   evidenceJSON,
		ProposedEffect: strings.TrimSpace(req.ProposedEffect),
		Confidence:     req.Confidence,
		CreatedByType:  firstNonEmpty(strings.TrimSpace(req.CreatedByType), "user"),
		CreatedByRef:   strings.TrimSpace(req.CreatedByRef),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, proposal)
}

func (s *Server) handleListKnowledgeRelationProposals(w http.ResponseWriter, r *http.Request) {
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
	proposals, err := s.store.ListKnowledgeRelationProposals(r.Context(), storage.KnowledgeRelationProposalListOptions{
		Status:     strings.TrimSpace(r.URL.Query().Get("status")),
		DocumentID: strings.TrimSpace(r.URL.Query().Get("document_id")),
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": proposals})
}

func (s *Server) handleGetKnowledgeRelationProposal(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	proposal, err := s.store.GetKnowledgeRelationProposal(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("proposal %q not found", id))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (s *Server) handleApproveKnowledgeRelationProposal(w http.ResponseWriter, r *http.Request) {
	req, ok := relationReviewRequestFromHTTP(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	relation, err := s.store.ApproveKnowledgeRelationProposal(r.Context(), id, req.ReviewedBy, req.ReviewNote)
	if err != nil {
		writeKnowledgeRelationReviewError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, relation)
}

func (s *Server) handleRejectKnowledgeRelationProposal(w http.ResponseWriter, r *http.Request) {
	req, ok := relationReviewRequestFromHTTP(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	proposal, err := s.store.RejectKnowledgeRelationProposal(r.Context(), id, req.ReviewedBy, req.ReviewNote)
	if err != nil {
		writeKnowledgeRelationReviewError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (s *Server) handleCancelKnowledgeRelationProposal(w http.ResponseWriter, r *http.Request) {
	req, ok := relationReviewRequestFromHTTP(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	proposal, err := s.store.CancelKnowledgeRelationProposal(r.Context(), id, req.ReviewedBy, req.ReviewNote)
	if err != nil {
		writeKnowledgeRelationReviewError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func relationReviewRequestFromHTTP(w http.ResponseWriter, r *http.Request) (knowledgeRelationReviewRequest, bool) {
	var req knowledgeRelationReviewRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
			return req, false
		}
	}
	req.ReviewedBy = firstNonEmpty(strings.TrimSpace(req.ReviewedBy), "web")
	req.ReviewNote = strings.TrimSpace(req.ReviewNote)
	return req, true
}

func writeKnowledgeRelationReviewError(w http.ResponseWriter, id string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Errorf("proposal %q not found", id))
		return
	}
	if strings.Contains(err.Error(), " is ") {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func (s *Server) handleListKnowledgeRelations(w http.ResponseWriter, r *http.Request) {
	limit, err := optionalPositiveInt(r.URL.Query().Get("limit"), 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	relations, err := s.store.ListKnowledgeRelations(r.Context(), storage.KnowledgeRelationListOptions{
		DocumentID:      strings.TrimSpace(r.URL.Query().Get("document_id")),
		RelationTypes:   relationTypesFromQuery(r.URL.Query().Get("relation_types")),
		IncludeDisabled: strings.TrimSpace(r.URL.Query().Get("include_disabled")) == "true",
		Limit:           limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"relations": relations})
}

func (s *Server) handleDisableKnowledgeRelation(w http.ResponseWriter, r *http.Request) {
	req, ok := relationReviewRequestFromHTTP(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	relation, err := s.store.DisableKnowledgeRelation(r.Context(), id, req.ReviewedBy, req.ReviewNote)
	if err != nil {
		writeKnowledgeRelationError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, relation)
}

func (s *Server) handleReenableKnowledgeRelation(w http.ResponseWriter, r *http.Request) {
	req, ok := relationReviewRequestFromHTTP(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	relation, err := s.store.ReenableKnowledgeRelation(r.Context(), id, req.ReviewedBy, req.ReviewNote)
	if err != nil {
		writeKnowledgeRelationError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, relation)
}

func writeKnowledgeRelationError(w http.ResponseWriter, id string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Errorf("relation %q not found", id))
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func relationTypesFromQuery(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
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
		if !s.requiresToken(r.URL.Path) {
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

func requiresToken(path string) bool {
	return requiresTokenPath(path)
}

func (s *Server) requiresToken(path string) bool {
	if s.webPrefix != "" {
		if path == s.webPrefix || path == s.webPrefix+"/" {
			return false
		}
		trimmed, ok := strings.CutPrefix(path, s.webPrefix)
		if !ok {
			return false
		}
		if trimmed == "" {
			trimmed = "/"
		}
		path = trimmed
	}
	return requiresTokenPath(path)
}

func requiresTokenPath(path string) bool {
	if path == "/api/health" {
		return false
	}
	return strings.HasPrefix(path, "/api/")
}

func cleanWebPrefix(value string) string {
	prefix, err := config.NormalizeWebPrefix(value)
	if err != nil {
		return ""
	}
	return prefix
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
