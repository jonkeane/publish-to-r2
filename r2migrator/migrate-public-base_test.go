package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"
)

type memoryStore struct {
	data       []byte
	etag       string
	puts       map[string][]byte
	conditions map[string]string
	copies     map[string]string
	deletes    map[string]bool
}

func (s *memoryStore) get(context.Context, string) ([]byte, string, error) {
	return s.data, s.etag, nil
}

func (s *memoryStore) put(_ context.Context, key string, data []byte, condition string, create bool) error {
	if s.puts == nil {
		s.puts = map[string][]byte{}
		s.conditions = map[string]string{}
	}
	if create != strings.Contains(key, "/history/") {
		return errTest("unexpected create condition")
	}
	s.puts[key] = data
	s.conditions[key] = condition
	return nil
}

func (s *memoryStore) copy(_ context.Context, source, destination, _ string) error {
	if s.copies == nil {
		s.copies = map[string]string{}
	}
	s.copies[destination] = source
	return nil
}

func (s *memoryStore) delete(_ context.Context, key string) error {
	if s.deletes == nil {
		s.deletes = map[string]bool{}
	}
	s.deletes[key] = true
	return nil
}

func (s *memoryStore) listCurrent(context.Context) ([]string, error) { return nil, nil }

type errTest string

func (e errTest) Error() string { return string(e) }

func testManifest(t *testing.T) []byte {
	t.Helper()
	hash := strings.Repeat("a", 64)
	thumbnailHash := strings.Repeat("b", 64)
	galleryHash := strings.Repeat("c", 64)
	namespace, photoID := "catalog", "photo"
	base := "https://old.example"
	makeImage := func(hash string) image {
		key := legacyImageKey(namespace, photoID, hash)
		return image{Key: key, Source: imageURL(base, key), Width: 10, Height: 20, Bytes: 30, SHA256: hash}
	}
	main := makeImage(hash)
	m := manifest{
		SchemaVersion: 1, Revision: "old", GalleryID: "gallery", Namespace: namespace, ServiceID: "service",
		Title: "Gallery", UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Entries: []entry{{
			ID: photoID, Key: main.Key, Source: main.Source, Width: main.Width, Height: main.Height, Bytes: main.Bytes, SHA256: main.SHA256,
			Renditions: map[string]image{"thumbnail": makeImage(thumbnailHash), "gallery": makeImage(galleryHash)},
		}},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMigrateWritesHistoryThenETagProtectedCurrent(t *testing.T) {
	store := &memoryStore{data: testManifest(t), etag: "etag-before"}
	result, err := migrate(context.Background(), store, "gallery", "https://new.example", true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.changed || result.images != 3 || result.oldRevision != "old" || result.newRevision == "" {
		t.Fatalf("unexpected migration result: %#v", result)
	}
	current, found := store.puts["galleries/gallery/current.json"]
	if !found {
		t.Fatal("current manifest was not written")
	}
	if store.conditions["galleries/gallery/current.json"] != "etag-before" {
		t.Fatalf("current manifest was not protected by the read ETag: %#v", store.conditions)
	}
	historyKey := "galleries/gallery/history/" + result.newRevision + ".json"
	if store.conditions[historyKey] != "" {
		t.Fatalf("history manifest did not use create-only semantics: %#v", store.conditions)
	}
	updated, err := decodeManifest(current)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != result.newRevision || updated.ParentRevision != "old" {
		t.Fatalf("revision chain was not preserved: %#v", updated)
	}
	if err := validateManifest(updated); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range append([]image{{Key: updated.Entries[0].Key, Source: updated.Entries[0].Source}}, updated.Entries[0].Renditions["thumbnail"], updated.Entries[0].Renditions["gallery"]) {
		if !strings.HasPrefix(candidate.Source, "https://new.example/photos/") || strings.Contains(candidate.Source, "/"+candidate.SHA256+".jpg") {
			t.Fatalf("source was not migrated: %q", candidate.Source)
		}
	}
	if len(store.copies) != 3 {
		t.Fatalf("expected three server-side image promotions: %#v", store.copies)
	}
	if len(store.deletes) != 3 {
		t.Fatalf("expected three legacy JPEG deletions: %#v", store.deletes)
	}
}

func TestMigrateDryRunDoesNotWrite(t *testing.T) {
	store := &memoryStore{data: testManifest(t), etag: "etag-before"}
	result, err := migrate(context.Background(), store, "gallery", "https://new.example", false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.changed || len(store.puts) != 0 || len(store.deletes) != 0 {
		t.Fatalf("dry run mutated objects: puts=%#v deletes=%#v", store.puts, store.deletes)
	}
}

func TestMigrateCleansLegacyJPEGsFromAnAlreadyStableManifest(t *testing.T) {
	initial := &memoryStore{data: testManifest(t), etag: "etag-before"}
	if _, err := migrate(context.Background(), initial, "gallery", "https://new.example", true); err != nil {
		t.Fatal(err)
	}
	stable := initial.puts["galleries/gallery/current.json"]
	store := &memoryStore{data: stable, etag: "etag-after"}
	result, err := migrate(context.Background(), store, "gallery", "https://new.example", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.changed || result.deleted != 3 || len(store.copies) != 0 || len(store.puts) != 0 || len(store.deletes) != 3 {
		t.Fatalf("stable cleanup had unexpected result=%#v copies=%#v puts=%#v deletes=%#v", result, store.copies, store.puts, store.deletes)
	}
}

func TestSafeRemoteErrorClassifiesOnlySafeFailures(t *testing.T) {
	if got := safeRemoteError(nil); got != nil {
		t.Fatalf("safeRemoteError(nil) = %v, want nil", got)
	}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"cancelled", context.Canceled, "R2 request cancelled"},
		{"timeout", context.DeadlineExceeded, "R2 request timed out"},
		{"missing object", &smithy.GenericAPIError{Code: "NoSuchKey", Message: "private source details"}, "R2 object not found"},
		{"denied", &smithy.GenericAPIError{Code: "AccessDenied", Message: "private source details"}, "R2 access denied; the token needs Object Read & Write access to this bucket"},
		{"invalid key", &smithy.GenericAPIError{Code: "InvalidAccessKeyId", Message: "private source details"}, "R2 credentials were rejected; update the saved access key and secret"},
		{"bad signature", &smithy.GenericAPIError{Code: "SignatureDoesNotMatch", Message: "private source details"}, "R2 request signature was rejected; update the saved access key and secret"},
		{"temporary service failure", &smithy.GenericAPIError{Code: "SlowDown", Message: "private source details"}, "R2 is temporarily unavailable; retry the migration"},
		{"unknown API failure", &smithy.GenericAPIError{Code: "Unexpected", Message: "private source details"}, "R2 rejected the request with S3 error code \"Unexpected\""},
		{"invalid response", &smithy.DeserializationError{Err: errors.New("private source details")}, "R2 returned an invalid response; check whether the destination object was copied before retrying"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := safeRemoteError(test.err)
			if got.Error() != test.want {
				t.Fatalf("safeRemoteError() = %q, want %q", got, test.want)
			}
			if strings.Contains(got.Error(), "private source details") {
				t.Fatalf("safeRemoteError exposed the service response: %q", got)
			}
		})
	}
}

func TestSafeRemoteErrorDoesNotExposeUnknownErrorText(t *testing.T) {
	secret := "not-a-secret-but-must-not-appear"
	got := safeRemoteError(errTest(secret))
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("safeRemoteError exposed the wrapped error: %q", got)
	}
	if !strings.Contains(got.Error(), "main.errTest") {
		t.Fatalf("safeRemoteError did not identify the error type: %q", got)
	}
}
