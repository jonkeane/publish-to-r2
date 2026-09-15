package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jonkeane/publish-to-r2/uploader/internal/manifest"
	"github.com/jonkeane/publish-to-r2/uploader/internal/storage"
)

func readProgress(t *testing.T, e Engine, j Job) Progress {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(JobDir(e.Root, j.JobID), "progress.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p Progress
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.Version != 1 || p.Completed < 0 || p.Completed > p.Total {
		t.Fatalf("invalid progress: %+v", p)
	}
	return p
}

type progressStore struct {
	storage.Store
	head func()
}

func (s progressStore) Head(ctx context.Context, key string) (storage.Object, error) {
	s.head()
	return s.Store.Head(ctx, key)
}

func TestProgressTracksRenditionsAndVerification(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	addRenditions(t, e, &p)
	var progressMu sync.Mutex
	uploads, verified, commits := map[int]bool{}, map[int]bool{}, map[int]bool{}
	e.Store = progressStore{Store: s, head: func() {
		progressMu.Lock()
		defer progressMu.Unlock()
		progress := readProgress(t, e, p.Job)
		switch progress.Phase {
		case "uploading":
			if progress.Total != 3 {
				t.Fatalf("upload progress counts photos instead of JPEGs: %+v", progress)
			}
			uploads[progress.Completed] = true
		case "verifying":
			if progress.Total != 3 {
				t.Fatalf("wrong verification total: %+v", progress)
			}
			verified[progress.Completed] = true
		}
	}}
	s.beforePut = func(key string) {
		progress := readProgress(t, e, p.Job)
		if progress.Phase == "committing" {
			if progress.Total != 2 {
				t.Fatalf("wrong commit total: %+v", progress)
			}
			commits[progress.Completed] = true
		}
	}
	run(t, e, p.Job)
	for i := 0; i < 3; i++ {
		if !verified[i] {
			t.Fatalf("missing JPEG progress %d: uploads=%v verification=%v", i, uploads, verified)
		}
	}
	if !uploads[0] {
		t.Fatal("missing initial upload progress")
	}
	if !commits[0] || !commits[1] || readProgress(t, e, p.Job).Phase != "cleanup" {
		t.Fatal("missing finalization progress")
	}
	// Recovery after cleanup still reports the correct phase without staged JPEGs.
	run(t, e, p.Job)
	if readProgress(t, e, p.Job).Phase != "cleanup" {
		t.Fatal("replay retained stale upload progress")
	}
}

func TestFailedUploadDoesNotAdvanceProgress(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	addRenditions(t, e, &p)
	entry, err := e.inspect(p.Job, p.Job.Photos[0])
	if err != nil {
		t.Fatal(err)
	}
	s.fail = manifest.StagingKey(p.Job.Namespace, "photo", entry.Renditions["gallery"].SHA256)
	if _, err := e.Run(context.Background(), p.Job); err == nil {
		t.Fatal("expected upload failure")
	}
	progress := readProgress(t, e, p.Job)
	if progress.Phase != "uploading" || progress.Completed > 2 || progress.Total != 3 {
		t.Fatalf("failed upload counted as completed: %+v", progress)
	}
	s.fail = ""
	run(t, e, p.Job)
}
