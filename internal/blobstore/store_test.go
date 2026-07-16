package blobstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPutUsesContentAddressedAtomicObject(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
	first, err := store.Put(context.Background(), "job-one", bytes.NewReader(png), int64(len(png)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(context.Background(), "job-two", bytes.NewReader(png), int64(len(png)))
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 || first.StorageKey != second.StorageKey {
		t.Fatalf("same bytes produced different blobs: %#v %#v", first, second)
	}
	if first.MediaType != "image/png" {
		t.Fatalf("media type = %q", first.MediaType)
	}
	if want := filepath.ToSlash(filepath.Join("assets", "blobs", "sha256", first.SHA256[:2], first.SHA256[2:4], first.SHA256)); first.StorageKey != want {
		t.Fatalf("storage key = %q, want %q", first.StorageKey, want)
	}
	f, stat, err := store.Open(first.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, png) || stat.Size != int64(len(png)) {
		t.Fatalf("opened blob mismatch size=%d", stat.Size)
	}
}

func TestPutRejectsLimitAndInvalidDigest(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), "job", bytes.NewReader([]byte("too large")), 3); !errors.Is(err, ErrBlobTooLarge) {
		t.Fatalf("Put error = %v", err)
	}
	if _, _, err := store.Open("../../etc/passwd"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("Open error = %v", err)
	}
}

func TestCollectGarbageKeepsReferencedAndRecentBlobs(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.Put(context.Background(), "job", bytes.NewReader([]byte("old unreferenced")), 100)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := store.Put(context.Background(), "job", bytes.NewReader([]byte("old referenced")), 100)
	if err != nil {
		t.Fatal(err)
	}
	recent, err := store.Put(context.Background(), "job", bytes.NewReader([]byte("recent")), 100)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	for _, digest := range []string{old.SHA256, kept.SHA256} {
		path, _, err := store.objectPath(digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.CollectGarbage(context.Background(), time.Now().Add(-time.Hour), func(_ context.Context, digest string) (bool, error) {
		return digest == kept.SHA256, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1", result.Deleted)
	}
	if len(result.DeletedDigests) != 1 || result.DeletedDigests[0] != old.SHA256 {
		t.Fatalf("deleted digests = %v, want %s", result.DeletedDigests, old.SHA256)
	}
	if _, err := store.Stat(old.SHA256); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old blob still exists: %v", err)
	}
	for _, digest := range []string{kept.SHA256, recent.SHA256} {
		if _, err := store.Stat(digest); err != nil {
			t.Fatalf("kept blob %s missing: %v", digest, err)
		}
	}
}

func TestReferenceUpdateSerializesGarbageCollection(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.Put(context.Background(), "job", bytes.NewReader([]byte("old candidate")), 100)
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := store.objectPath(blob.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	release := store.BeginReferenceUpdate()
	done := make(chan error, 1)
	go func() {
		_, err := store.CollectGarbage(context.Background(), time.Now().Add(-time.Hour), func(context.Context, string) (bool, error) {
			return false, nil
		})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("CollectGarbage completed before reference update release: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
