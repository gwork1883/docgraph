package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/docgraph/docgraph/internal/blobstore"
	"github.com/docgraph/docgraph/internal/storage"
)

type BlobStore interface {
	Open(digest string) (*os.File, blobstore.Blob, error)
}

func (s *Server) SetBlobStore(blobs BlobStore) {
	if s != nil {
		s.blobs = blobs
	}
}

func (s *Server) handleGetSection(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("section id is required"))
		return
	}
	section, err := s.store.GetSection(r.Context(), id)
	if err != nil {
		writeSectionLookupError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, section)
}

func (s *Server) handleGetSectionContext(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("section id is required"))
		return
	}
	childLimit, err := optionalPositiveInt(r.URL.Query().Get("child_limit"), 100)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if childLimit > 200 {
		childLimit = 200
	}
	relationLimit, err := optionalPositiveInt(r.URL.Query().Get("relation_limit"), 20)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if relationLimit > 100 {
		relationLimit = 100
	}
	subtreeDepth, err := optionalNonNegativeInt(r.URL.Query().Get("subtree_depth"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if subtreeDepth > 10 {
		subtreeDepth = 10
	}
	subtreeLimit, err := optionalPositiveInt(r.URL.Query().Get("subtree_limit"), 200)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if subtreeLimit > 500 {
		subtreeLimit = 500
	}
	contextStore, ok := s.store.(storage.SectionContextStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("section context is not supported by this storage backend"))
		return
	}
	sectionContext, err := contextStore.GetSectionContext(r.Context(), id, childLimit)
	if err != nil {
		writeSectionLookupError(w, id, err)
		return
	}
	document, err := s.store.GetDocument(r.Context(), sectionContext.Section.DocumentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	source, err := s.store.GetSource(r.Context(), document.SourceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	featureInventory := []storage.FeatureInventoryEntry{}
	if mediaStore, ok := s.store.(storage.MediaStore); ok {
		if inventory, inventoryErr := mediaStore.ListSourceFeatureInventory(r.Context(), source.ID); inventoryErr == nil {
			featureInventory = inventory
		}
	}
	authoredRelations := sectionContext.AuthoredRelations
	if len(authoredRelations) > relationLimit {
		authoredRelations = authoredRelations[:relationLimit]
	}
	subtree, subtreeTruncated, err := loadSectionSubtree(r.Context(), contextStore, sectionContext.Section.DocumentID, id, subtreeDepth, subtreeLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"section":            sectionContext.Section,
		"structure":          sectionContext.Structure,
		"ancestors":          sectionContext.Ancestors,
		"children":           sectionContext.Children,
		"children_page":      sectionContext.ChildrenPage,
		"authored_relations": authoredRelations,
		"media_assets":       sectionContext.MediaAssets,
		"subtree":            subtree,
		"subtree_truncated":  subtreeTruncated,
		"snapshot":           sectionContext.Snapshot,
		"document":           document,
		"source":             source,
		"feature_inventory":  featureInventory,
	})
}

func loadSectionSubtree(ctx context.Context, store storage.SectionContextStore, documentID, rootSectionID string, maxDepth, limit int) ([]storage.SectionSummary, bool, error) {
	if maxDepth <= 0 || limit <= 0 {
		return []storage.SectionSummary{}, false, nil
	}
	type pendingParent struct {
		id    string
		depth int
	}
	queue := []pendingParent{{id: rootSectionID, depth: 0}}
	sections := make([]storage.SectionSummary, 0, min(limit, 100))
	truncated := false
	for len(queue) > 0 && len(sections) < limit {
		parent := queue[0]
		queue = queue[1:]
		if parent.depth >= maxDepth {
			continue
		}
		remaining := limit - len(sections)
		outline, err := store.ListDocumentOutline(ctx, documentID, storage.OutlineOptions{ParentSectionID: parent.id, Limit: remaining})
		if err != nil {
			return nil, false, err
		}
		if outline.HasMore {
			truncated = true
		}
		for _, child := range outline.Sections {
			sections = append(sections, child)
			if parent.depth+1 < maxDepth {
				queue = append(queue, pendingParent{id: child.ID, depth: parent.depth + 1})
			}
			if len(sections) == limit {
				if len(queue) > 0 || outline.HasMore {
					truncated = true
				}
				break
			}
		}
	}
	if len(queue) > 0 {
		truncated = true
	}
	return sections, truncated, nil
}

func (s *Server) handleGetDocumentOutline(w http.ResponseWriter, r *http.Request) {
	documentID := strings.TrimSpace(r.PathValue("id"))
	if documentID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("document id is required"))
		return
	}
	limit, err := optionalPositiveInt(r.URL.Query().Get("limit"), 100)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	offset, err := optionalNonNegativeInt(r.URL.Query().Get("offset"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	contextStore, ok := s.store.(storage.SectionContextStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("document outline is not supported by this storage backend"))
		return
	}
	outline, err := contextStore.ListDocumentOutline(r.Context(), documentID, storage.OutlineOptions{
		ParentSectionID: strings.TrimSpace(r.URL.Query().Get("parent_section_id")),
		Limit:           limit,
		Offset:          offset,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("document %q not found", documentID))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, outline)
}

func (s *Server) handleListDocumentMediaAssets(w http.ResponseWriter, r *http.Request) {
	documentID := strings.TrimSpace(r.PathValue("id"))
	limit, err := optionalPositiveInt(r.URL.Query().Get("limit"), 100)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	offset, err := optionalNonNegativeInt(r.URL.Query().Get("offset"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	mediaStore, ok := s.store.(storage.MediaStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("media assets are not supported by this storage backend"))
		return
	}
	assets, err := mediaStore.ListDocumentMediaAssets(r.Context(), documentID, limit, offset)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("document %q not found", documentID))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"document_id": documentID, "media_assets": assets, "limit": limit, "offset": offset})
}

func (s *Server) handleGetMediaAsset(w http.ResponseWriter, r *http.Request) {
	asset, err := s.lookupMediaAsset(r)
	if err != nil {
		writeMediaLookupError(w, r.PathValue("id"), err)
		return
	}
	writeJSON(w, http.StatusOK, asset)
}

func (s *Server) handleGetMediaAssetContent(w http.ResponseWriter, r *http.Request) {
	asset, err := s.lookupMediaAsset(r)
	if err != nil {
		writeMediaLookupError(w, r.PathValue("id"), err)
		return
	}
	if strings.TrimSpace(asset.BlobSHA256) == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("media asset %q has no preserved content (status %s)", asset.ID, asset.Status))
		return
	}
	if s.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("blob store is not configured"))
		return
	}
	file, blob, err := s.blobs.Open(asset.BlobSHA256)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, fmt.Errorf("preserved content for media asset %q is missing", asset.ID))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer file.Close()
	mediaType := strings.TrimSpace(blob.MediaType)
	if mediaType == "" {
		mediaType = strings.TrimSpace(asset.MediaType)
	}
	serveStoredBlob(w, r, file, blob, firstNonEmpty(asset.OriginalName, asset.ID), mediaType, safeInlineRaster(mediaType))
}

func (s *Server) handleGetSourceSnapshot(w http.ResponseWriter, r *http.Request) {
	sourceID := strings.TrimSpace(r.PathValue("id"))
	mediaStore, ok := s.store.(storage.MediaStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("source snapshots are not supported by this storage backend"))
		return
	}
	snapshot, err := mediaStore.GetActiveSourceSnapshot(r.Context(), sourceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, fmt.Errorf("active snapshot for source %q not found", sourceID))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("blob store is not configured"))
		return
	}
	file, blob, err := s.blobs.Open(snapshot.BlobSHA256)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, fmt.Errorf("active snapshot content for source %q is missing", sourceID))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer file.Close()
	source, _ := s.store.GetSource(r.Context(), sourceID)
	filename := strings.TrimSuffix(firstNonEmpty(source.Name, sourceID), ".xmind") + ".xmind"
	serveStoredBlob(w, r, file, blob, filename, "application/octet-stream", false)
}

func (s *Server) lookupMediaAsset(r *http.Request) (storage.MediaAsset, error) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		return storage.MediaAsset{}, fmt.Errorf("media asset id is required")
	}
	mediaStore, ok := s.store.(storage.MediaStore)
	if !ok {
		return storage.MediaAsset{}, fmt.Errorf("media assets are not supported by this storage backend")
	}
	return mediaStore.GetMediaAsset(r.Context(), id)
}

func serveStoredBlob(w http.ResponseWriter, r *http.Request, file *os.File, blob blobstore.Blob, filename, mediaType string, inline bool) {
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", firstNonEmpty(mediaType, "application/octet-stream"))
	w.Header().Set("ETag", strconv.Quote(blob.SHA256))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	disposition := "attachment"
	if inline {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": filename}))
	http.ServeContent(w, r, filename, info.ModTime(), file)
}

func safeInlineRaster(mediaType string) bool {
	mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0]))
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp":
		return true
	default:
		return false
	}
}

func writeSectionLookupError(w http.ResponseWriter, id string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Errorf("section %q not found", id))
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

func writeMediaLookupError(w http.ResponseWriter, id string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Errorf("media asset %q not found", id))
		return
	}
	if strings.Contains(err.Error(), "not supported") || strings.Contains(err.Error(), "required") {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}
