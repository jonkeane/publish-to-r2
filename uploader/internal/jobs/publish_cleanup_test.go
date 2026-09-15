package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jonkeane/publish-to-r2/uploader/internal/manifest"
	"github.com/jonkeane/publish-to-r2/uploader/internal/storage"
)

func orphan(s *fakeStore, namespace, id, hash string) string {
	key := manifest.Key(namespace, id, strings.Repeat(hash, 64))
	s.objects[key] = fakeObject{body: []byte("old export"), info: storage.Object{Key: key}}
	return key
}

func TestRepublishRemovesAllObsoleteVersions(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	add(t, e, &p, "photo", 70)
	old := run(t, e, p.Job)
	stale := orphan(s, "catalog", "photo", "a") // Legacy migration source.
	q := prepare(t, e, "gallery")
	addRenditions(t, e, &q)
	entry, err := e.inspect(q.Job, q.Job.Photos[0])
	if err != nil {
		t.Fatal(err)
	}
	run(t, e, q.Job)
	if _, ok := s.objects[old.Photos[0].Key]; !ok {
		t.Fatal("stable large image was removed")
	}
	for _, image := range entry.Images() {
		if _, ok := s.objects[image.Key]; !ok {
			t.Fatal("current rendition missing", image.Key)
		}
	}
	if _, ok := s.objects[stale]; !ok {
		t.Fatal("legacy object was removed before grace-period maintenance")
	}
	// An unchanged republish does not rewrite stable public objects. A replay
	// after acknowledgment needs no staged files.
	r := prepare(t, e, "gallery")
	addRenditions(t, e, &r)
	counts := map[string]int{}
	for _, image := range entry.Images() {
		counts[image.Key] = s.puts[image.Key]
	}
	run(t, e, r.Job)
	run(t, e, r.Job)
	for _, image := range entry.Images() {
		if s.puts[image.Key] != counts[image.Key] {
			t.Fatal("unchanged image uploaded again")
		}
	}
	// Switching back to a single-image job removes now-unused small renditions.
	u := prepare(t, e, "gallery")
	add(t, e, &u, "photo", 255)
	latest := run(t, e, u.Job)
	photos, err := s.List(context.Background(), "photos/catalog/photo/")
	if err != nil || len(photos) != 2 {
		t.Fatalf("obsolete renditions retained: %+v %v", photos, err)
	}
	if _, ok := s.objects[latest.Photos[0].Key]; !ok {
		t.Fatal("latest large image missing")
	}
	if _, ok := s.objects[manifest.Key("catalog", "photo", "thumbnail")]; ok {
		t.Fatal("obsolete thumbnail retained")
	}
}

func TestRejectsPhotoAlreadyInAnotherGallery(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "one")
	addRenditions(t, e, &p)
	run(t, e, p.Job)
	q := prepare(t, e, "two")
	add(t, e, &q, "photo", 70)
	before := len(s.objects)
	deletes := len(s.deletes)
	result, err := e.Run(context.Background(), q.Job)
	if err == nil || !strings.Contains(err.Error(), "already belongs to gallery") || result.Status == "committed" {
		t.Fatalf("duplicate ownership accepted: %+v %v", result, err)
	}
	if len(s.objects) != before || len(s.deletes) != deletes {
		t.Fatal("duplicate export changed remote storage")
	}
	// Moving a photo explicitly is supported once its previous owner removes it.
	p = prepare(t, e, "one")
	p.Job.Removals = []string{"photo"}
	run(t, e, p.Job)
	run(t, e, q.Job)
}

func TestRemoveLegacySharedPhoto(t *testing.T) {
	for _, uploadUnrelated := range []bool{false, true} {
		name := "removal only"
		if uploadUnrelated {
			name = "removal with unrelated upload"
		}
		t.Run(name, func(t *testing.T) {
			e, s := fixture(t)
			p := prepare(t, e, "one")
			addRenditions(t, e, &p)
			run(t, e, p.Job)
			other, _, err := e.Current(context.Background(), "one")
			if err != nil {
				t.Fatal(err)
			}
			// Seed a second gallery as an older helper could, sharing all images.
			legacy := other
			legacy.GalleryID = "two"
			data, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			key := manifest.CurrentKey("two")
			s.objects[key] = fakeObject{body: data, info: storage.Object{Key: key, ETag: "legacy"}}

			// Even a live upload reservation for this photo must not prevent
			// removing its legacy membership from a different gallery.
			active := prepare(t, e, "one")
			add(t, e, &active, "photo", 70)
			releaseJob, err := e.publicationLocks(context.Background(), active.Job)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseJob()
			releaseClaim, err := e.reservePhotos(context.Background(), active.Job)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseClaim()

			q := prepare(t, e, "two")
			q.Job.Removals = []string{"photo"}
			wantEntries := 0
			if uploadUnrelated {
				add(t, e, &q, "unrelated", 5)
				wantEntries = 1
			}
			run(t, e, q.Job)
			run(t, e, q.Job) // Removal acknowledgment can be retried.
			current, _, err := e.Current(context.Background(), "two")
			if err != nil || len(current.Entries) != wantEntries {
				t.Fatalf("unexpected remaining entries: %+v %v", current, err)
			}
			for _, entry := range current.Entries {
				if entry.ID != "unrelated" {
					t.Fatal("removed photo retained", entry.ID)
				}
			}
			retained, _, err := e.Current(context.Background(), "one")
			if err != nil || !sameManifest(retained, other) {
				t.Fatalf("other gallery changed: %+v %v", retained, err)
			}
			for _, image := range other.Entries[0].Images() {
				if _, ok := s.objects[image.Key]; !ok {
					t.Fatal("shared image deleted", image.Key)
				}
			}
		})
	}
}

func TestRepublishFailureAndRecovery(t *testing.T) {
	for _, phase := range []string{"upload", "history", "commit", "lost commit", "delete", "photo list", "gallery list", "manifest read", "corrupt manifest"} {
		t.Run(phase, func(t *testing.T) {
			e, s := fixture(t)
			p := prepare(t, e, "gallery")
			add(t, e, &p, "photo", 70)
			old := run(t, e, p.Job)
			q := prepare(t, e, "gallery")
			addRenditions(t, e, &q)
			updated, inspectErr := e.inspect(q.Job, q.Job.Photos[0])
			if inspectErr != nil {
				t.Fatal(inspectErr)
			}
			// Use another current manifest to test failed reads during cleanup.
			other := prepare(t, e, "other")
			run(t, e, other.Job)
			otherKey := manifest.CurrentKey("other")
			validOther := s.objects[otherKey]
			switch phase {
			case "upload":
				s.fail = "staging/"
			case "history":
				s.fail = "/history/"
			case "commit":
				s.fail = "current.json"
			case "lost commit":
				s.loseCommit = true
			case "delete":
				s.failDelete = manifest.StagingKey(q.Job.Namespace, "photo", updated.SHA256)
			case "photo list":
				s.failList = "photos/"
			case "gallery list":
				s.failList = "galleries/"
			case "manifest read":
				s.failGet = otherKey
			case "corrupt manifest":
				s.objects[otherKey] = fakeObject{body: []byte(`{"schemaVersion":999}`), info: validOther.info}
			}
			result, err := e.Run(context.Background(), q.Job)
			if err == nil || result.Status == "committed" || result.Photos[0].Status != "pending" {
				t.Fatalf("failure acknowledged as success: %+v %v", result, err)
			}
			if phase == "delete" && !errors.Is(err, storage.ErrPermission) {
				t.Fatalf("cleanup error lost its cause: %v", err)
			}
			if _, ok := s.objects[old.Photos[0].Key]; !ok {
				t.Fatal("stable image disappeared before successful commit")
			}
			for _, file := range q.Job.Photos[0].files() {
				if _, err := os.Stat(file.Path); err != nil {
					t.Fatal("retry bytes removed", err)
				}
			}
			var saved Result
			b, err := os.ReadFile(q.Directory + "/result.json")
			if err != nil || json.Unmarshal(b, &saved) != nil || saved.Status == "committed" {
				t.Fatal("failed cleanup persisted successful acknowledgment")
			}
			s.fail, s.failDelete, s.failList, s.failGet = "", "", "", ""
			s.objects[otherKey] = validOther
			run(t, e, q.Job)
			if got := s.objects[old.Photos[0].Key].info.Hash; got != updated.SHA256 {
				t.Fatal("retry did not preserve the promoted stable image")
			}
			if s.puts[manifest.CurrentKey("gallery")] != 2 {
				t.Fatal("recovery rewrote the committed manifest")
			}
			for key, count := range s.puts {
				if strings.HasPrefix(key, "staging/") && count != 1 {
					t.Fatal("recovery uploaded an image again")
				}
			}
			deletes := len(s.deletes)
			if _, err := e.Run(context.Background(), p.Job); !errors.Is(err, storage.ErrConflict) || len(s.deletes) != deletes {
				t.Fatal("stale replay performed cleanup")
			}
		})
	}
}

func TestPartialCleanupCanResume(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	addRenditions(t, e, &p)
	entry, err := e.inspect(p.Job, p.Job.Photos[0])
	if err != nil {
		t.Fatal(err)
	}
	second := manifest.StagingKey(p.Job.Namespace, "photo", entry.SHA256)
	s.failDelete = second
	if _, err := e.Run(context.Background(), p.Job); err == nil {
		t.Fatal("expected partial cleanup failure")
	}
	if _, ok := s.objects[second]; !ok {
		t.Fatal("failed staged cleanup removed its retry target")
	}
	s.failDelete = ""
	results, err := e.Reconcile(context.Background(), "gallery")
	if err != nil || len(results) != 1 || results[0].Status != "committed" {
		t.Fatalf("cleanup reconciliation failed: %+v %v", results, err)
	}
	if _, ok := s.objects[second]; ok {
		t.Fatal("reconciliation left obsolete image")
	}
}
