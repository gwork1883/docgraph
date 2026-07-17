package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultMaxBlobBytes int64 = 1 << 30

var ErrBlobTooLarge = errors.New("blob exceeds size limit")
var ErrInvalidDigest = errors.New("invalid sha256 digest")

// Blob describes an immutable object stored below the configured DocGraph
// data directory. StorageKey is relative to data_dir and is safe to persist in
// SQLite; callers must never turn user input into a filesystem path directly.
type Blob struct {
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size_bytes"`
	MediaType  string `json:"media_type"`
	StorageKey string `json:"storage_key"`
}

type GCResult struct {
	Scanned        int      `json:"scanned"`
	Deleted        int      `json:"deleted"`
	DeletedDigests []string `json:"deleted_digests,omitempty"`
}

// Store is a central content-addressed Blob store. Staging and final objects
// live on the same filesystem so promotion can use an atomic rename.
type Store struct {
	dataDir     string
	blobRoot    string
	stagingRoot string
	mu          sync.RWMutex
	referenceMu sync.RWMutex
}

func New(dataDir string) (*Store, error) {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return nil, fmt.Errorf("blobstore data directory is required")
	}
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve blobstore data directory: %w", err)
	}
	assets := filepath.Join(dataDir, "assets")
	s := &Store{
		dataDir:     dataDir,
		blobRoot:    filepath.Join(assets, "blobs", "sha256"),
		stagingRoot: filepath.Join(assets, "staging"),
	}
	for _, dir := range []string{s.blobRoot, s.stagingRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create blobstore directory %s: %w", dir, err)
		}
	}
	return s, nil
}

func (s *Store) DataDir() string {
	return s.dataDir
}

// BeginReferenceUpdate prevents in-process garbage collection from deleting
// immutable objects between Blob staging and the relational transaction that
// makes those objects reachable. The returned release function must be called.
func (s *Store) BeginReferenceUpdate() func() {
	s.referenceMu.RLock()
	return s.referenceMu.RUnlock
}

func (s *Store) Put(ctx context.Context, jobScope string, src io.Reader, maxBytes int64) (Blob, error) {
	if src == nil {
		return Blob{}, fmt.Errorf("blob source is required")
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBlobBytes
	}
	stageDir := filepath.Join(s.stagingRoot, safeScope(jobScope))
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return Blob{}, fmt.Errorf("create blob staging directory: %w", err)
	}
	staged, err := os.CreateTemp(stageDir, "blob-*")
	if err != nil {
		return Blob{}, fmt.Errorf("create staged blob: %w", err)
	}
	stagedPath := staged.Name()
	removeStaged := true
	defer func() {
		_ = staged.Close()
		if removeStaged {
			_ = os.Remove(stagedPath)
		}
	}()

	hash := sha256.New()
	head := make([]byte, 0, 512)
	buf := make([]byte, 64*1024)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return Blob{}, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			size += int64(n)
			if size > maxBytes {
				return Blob{}, fmt.Errorf("%w: maximum %d bytes", ErrBlobTooLarge, maxBytes)
			}
			if len(head) < cap(head) {
				remaining := cap(head) - len(head)
				if remaining > n {
					remaining = n
				}
				head = append(head, buf[:remaining]...)
			}
			if _, err := hash.Write(buf[:n]); err != nil {
				return Blob{}, fmt.Errorf("hash staged blob: %w", err)
			}
			if _, err := staged.Write(buf[:n]); err != nil {
				return Blob{}, fmt.Errorf("write staged blob: %w", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return Blob{}, fmt.Errorf("read blob: %w", readErr)
		}
	}
	if err := staged.Sync(); err != nil {
		return Blob{}, fmt.Errorf("fsync staged blob: %w", err)
	}
	if err := staged.Close(); err != nil {
		return Blob{}, fmt.Errorf("close staged blob: %w", err)
	}

	digest := hex.EncodeToString(hash.Sum(nil))
	finalPath, storageKey, err := s.objectPath(digest)
	if err != nil {
		return Blob{}, err
	}
	mediaType := http.DetectContentType(head)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(finalPath); err == nil {
		return Blob{SHA256: digest, Size: size, MediaType: mediaType, StorageKey: storageKey}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Blob{}, fmt.Errorf("stat final blob: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
		return Blob{}, fmt.Errorf("create final blob directory: %w", err)
	}
	if err := os.Rename(stagedPath, finalPath); err != nil {
		// Another process may have promoted the same digest between Stat and
		// Rename. Treat an existing immutable object as success.
		if _, statErr := os.Stat(finalPath); statErr != nil {
			return Blob{}, fmt.Errorf("promote staged blob: %w", err)
		}
	} else {
		removeStaged = false
		if err := syncDirectory(filepath.Dir(finalPath)); err != nil {
			return Blob{}, err
		}
	}
	return Blob{SHA256: digest, Size: size, MediaType: mediaType, StorageKey: storageKey}, nil
}

func (s *Store) Open(digest string) (*os.File, Blob, error) {
	path, key, err := s.objectPath(digest)
	if err != nil {
		return nil, Blob{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, err := os.Open(path)
	if err != nil {
		return nil, Blob{}, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Blob{}, err
	}
	head := make([]byte, 512)
	n, readErr := f.Read(head)
	if readErr != nil && readErr != io.EOF {
		_ = f.Close()
		return nil, Blob{}, readErr
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, Blob{}, err
	}
	return f, Blob{SHA256: strings.ToLower(digest), Size: info.Size(), MediaType: http.DetectContentType(head[:n]), StorageKey: key}, nil
}

func (s *Store) Stat(digest string) (Blob, error) {
	f, blob, err := s.Open(digest)
	if err != nil {
		return Blob{}, err
	}
	_ = f.Close()
	return blob, nil
}

// CollectGarbage removes old physical objects that are not referenced by the
// relational store. isReferenced must query current state; it is called while
// the in-process Blob store write lock is held immediately before deletion.
// Callers should additionally serialize GC with bundle replacement across
// processes when multiple DocGraph processes share one data directory.
func (s *Store) CollectGarbage(ctx context.Context, olderThan time.Time, isReferenced func(context.Context, string) (bool, error)) (GCResult, error) {
	if isReferenced == nil {
		return GCResult{}, fmt.Errorf("blob reference check is required")
	}
	s.referenceMu.Lock()
	defer s.referenceMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	result := GCResult{}
	err := filepath.WalkDir(s.blobRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		digest := entry.Name()
		if !validDigest(digest) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		result.Scanned++
		if !olderThan.IsZero() && !info.ModTime().Before(olderThan) {
			return nil
		}
		referenced, err := isReferenced(ctx, digest)
		if err != nil {
			return err
		}
		if referenced {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		result.Deleted++
		result.DeletedDigests = append(result.DeletedDigests, strings.ToLower(digest))
		return nil
	})
	return result, err
}

func (s *Store) CleanupStaging(olderThan time.Time) (int, error) {
	removed := 0
	err := filepath.WalkDir(s.stagingRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == s.stagingRoot || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !olderThan.IsZero() && !info.ModTime().Before(olderThan) {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		removed++
		return nil
	})
	return removed, err
}

func (s *Store) objectPath(digest string) (string, string, error) {
	digest = strings.ToLower(strings.TrimSpace(digest))
	if !validDigest(digest) {
		return "", "", ErrInvalidDigest
	}
	key := filepath.ToSlash(filepath.Join("assets", "blobs", "sha256", digest[:2], digest[2:4], digest))
	path := filepath.Join(s.dataDir, filepath.FromSlash(key))
	return path, key, nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func safeScope(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "anonymous"
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open blob directory for fsync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("fsync blob directory: %w", err)
	}
	return nil
}
