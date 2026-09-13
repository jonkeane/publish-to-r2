package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jkeane/publish-to-r2/uploader/internal/manifest"
	"github.com/jkeane/publish-to-r2/uploader/internal/storage"
)

type gatedStore struct {
	storage.Store
	entered chan string
	release chan struct{}
	active  atomic.Int32
	peak    atomic.Int32
}

func newGate(s storage.Store) *gatedStore {
	return &gatedStore{Store: s, entered: make(chan string, 32), release: make(chan struct{})}
}

func (s *gatedStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, options storage.PutOptions) error {
	if strings.HasPrefix(key, "photos/") {
		n := s.active.Add(1)
		defer s.active.Add(-1)
		for old := s.peak.Load(); n > old && !s.peak.CompareAndSwap(old, n); old = s.peak.Load() {
		}
		s.entered <- key
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.Put(ctx, key, body, size, options)
}

type outcome struct {
	result Result
	err    error
}

func startRun(ctx context.Context, e Engine, j Job) <-chan outcome {
	done := make(chan outcome, 1)
	go func() { result, err := e.Run(ctx, j); done <- outcome{result, err} }()
	return done
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent operation did not finish")
		var zero T
		return zero
	}
}

func noUpload(t *testing.T, gate *gatedStore) {
	t.Helper()
	select {
	case key := <-gate.entered:
		t.Fatal("unexpected concurrent upload", key)
	case <-time.After(75 * time.Millisecond):
	}
}

func TestUploadsOverlapWithinBoundAndPreserveResultOrder(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	for i := 0; i < 7; i++ {
		add(t, e, &p, fmt.Sprintf("photo-%d", i), uint8(i*30))
	}
	gate := newGate(s)
	e.Store = gate
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startRun(ctx, e, p.Job)
	for range uploadWorkers {
		receive(t, gate.entered)
	}
	noUpload(t, gate)
	if _, _, err := s.Get(ctx, manifest.CurrentKey("gallery")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("gallery committed before uploads completed")
	}
	close(gate.release)
	out := receive(t, done)
	if out.err != nil || out.result.Status != "committed" {
		t.Fatalf("%+v", out)
	}
	if gate.peak.Load() != uploadWorkers || gate.active.Load() != 0 {
		t.Fatal("upload worker bound or shutdown violated")
	}
	for i, photo := range out.result.Photos {
		if photo.ID != p.Job.Photos[i].ID || photo.Status != "committed" {
			t.Fatal("completion order changed photo acknowledgments")
		}
	}
}

func TestDifferentGalleriesStageAndUploadConcurrently(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "one")
	add(t, e, &p, "photo-one", 5)
	gate := newGate(s)
	e.Store = gate
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := startRun(ctx, e, p.Job)
	receive(t, gate.entered)
	// These used to fail against Run's application-wide exclusive lock.
	q := prepare(t, e, "two")
	add(t, e, &q, "photo-two", 10)
	configUnlock, err := ConfigLock(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	configUnlock()
	second := startRun(ctx, e, q.Job)
	receive(t, gate.entered)
	if unlock, err := Lock(e.Root); err == nil {
		unlock()
		t.Fatal("maintenance admitted during uploads")
	}
	if _, err := DiscardSpool(e.Root, p.Job.JobID, true); err == nil {
		t.Fatal("active spool could be discarded")
	}
	close(gate.release)
	for _, done := range []<-chan outcome{first, second} {
		out := receive(t, done)
		if out.err != nil || out.result.Status != "committed" {
			t.Fatalf("%+v", out)
		}
	}
	unlock, err := Lock(e.Root)
	if err != nil {
		t.Fatal("maintenance lock leaked", err)
	}
	unlock()
}

func TestConcurrentDuplicatePhotoIsRejectedBeforeUpload(t *testing.T) {
	e, s := fixture(t)
	p, q := prepare(t, e, "one"), prepare(t, e, "two")
	add(t, e, &p, "shared", 5)
	add(t, e, &q, "shared", 70)
	gate := newGate(s)
	e.Store = gate
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := startRun(ctx, e, p.Job)
	receive(t, gate.entered)
	second := startRun(ctx, e, q.Job)
	noUpload(t, gate)
	close(gate.release)
	if out := receive(t, first); out.err != nil {
		t.Fatal(out.err)
	}
	out := receive(t, second)
	if out.err == nil || !strings.Contains(out.err.Error(), "already being published to gallery") {
		t.Fatalf("duplicate ownership: %+v", out)
	}
	if len(gate.entered) != 0 {
		t.Fatal("duplicate photo was uploaded before rejection")
	}
	if _, _, err := s.Get(ctx, manifest.CurrentKey("two")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("duplicate gallery committed")
	}
}

func TestSameGalleryWaitsAndRejectsStaleRevision(t *testing.T) {
	e, s := fixture(t)
	p, q := prepare(t, e, "same"), prepare(t, e, "same")
	add(t, e, &p, "one", 5)
	add(t, e, &q, "two", 70)
	gate := newGate(s)
	e.Store = gate
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := startRun(ctx, e, p.Job)
	receive(t, gate.entered)
	second := startRun(ctx, e, q.Job)
	noUpload(t, gate)
	close(gate.release)
	if out := receive(t, first); out.err != nil {
		t.Fatal(out.err)
	}
	if out := receive(t, second); !errors.Is(out.err, storage.ErrConflict) {
		t.Fatalf("stale revision accepted: %+v", out)
	}
}

func TestCancelledUploadsDrainAndCanRetry(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	addRenditions(t, e, &p)
	gate := newGate(s)
	e.Store = gate
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startRun(ctx, e, p.Job)
	for range 3 {
		receive(t, gate.entered)
	}
	cancel()
	out := receive(t, done)
	if !errors.Is(out.err, context.Canceled) || gate.active.Load() != 0 {
		t.Fatalf("uploads outlived cancellation: %+v", out)
	}
	if _, _, err := s.Get(context.Background(), manifest.CurrentKey("gallery")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("cancelled job committed")
	}
	for _, file := range p.Job.Photos[0].files() {
		if _, err := os.Stat(file.Path); err != nil {
			t.Fatal("retry bytes removed", err)
		}
	}
	e.Store = s
	run(t, e, p.Job)
}

func TestSpoolCapacityIsAtomicAcrossStages(t *testing.T) {
	e, _ := fixture(t)
	p, q := prepare(t, e, "one"), prepare(t, e, "two")
	source := sourceJPEG(t, 5)
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	used, err := SpoolBytes(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	e.Profile.SpoolLimitBytes = used + info.Size() + (1 << 20)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, job := range []Job{p.Job, q.Job} {
		go func() { <-start; _, err := e.Stage(job.JobID, "photo", source); results <- err }()
	}
	close(start)
	succeeded := 0
	for range 2 {
		if receive(t, results) == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("spool capacity admitted %d stages, want 1", succeeded)
	}
}

func TestWaitingPublicationCanBeCancelled(t *testing.T) {
	e, _ := fixture(t)
	p := prepare(t, e, "gallery")
	unlock, err := e.publicationLocks(context.Background(), p.Job)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := startRun(ctx, e, p.Job)
	cancel()
	if out := receive(t, done); !errors.Is(out.err, context.Canceled) {
		t.Fatal(out.err)
	}
	unlock()
	run(t, e, p.Job)
}

func TestSpoolInventoryToleratesConcurrentProgressReplacement(t *testing.T) {
	e, _ := fixture(t)
	p := prepare(t, e, "gallery")
	var writes sync.WaitGroup
	writes.Go(func() {
		for i := range 40 {
			e.progress(p.Job, "uploading", i, 40)
		}
	})
	for range 40 {
		if _, err := SpoolBytes(e.Root); err != nil {
			t.Error(err)
		}
	}
	writes.Wait()
	if _, err := os.Stat(filepath.Join(p.Directory, "progress.json")); err != nil {
		t.Fatal(err)
	}
}

func TestAbandonedPhotoReservationDoesNotBlockRetry(t *testing.T) {
	e, _ := fixture(t)
	p, q := prepare(t, e, "one"), prepare(t, e, "two")
	add(t, e, &p, "photo", 5)
	add(t, e, &q, "photo", 70)
	releaseJob, err := e.publicationLocks(context.Background(), p.Job)
	if err != nil {
		t.Fatal(err)
	}
	releaseClaim, err := e.reservePhotos(context.Background(), p.Job)
	if err != nil {
		releaseJob()
		t.Fatal(err)
	}
	defer releaseClaim()
	// Model process death: OS releases the job lock, reservation remains on disk.
	releaseJob()
	run(t, e, q.Job)
	claims, err := os.ReadDir(filepath.Join(e.Root, "claims"))
	if err != nil || len(claims) != 0 {
		t.Fatalf("stale reservation survived: %v %v", claims, err)
	}
}

type failingUploadStore struct {
	storage.Store
	failKey string
	entered chan struct{}
	failNow chan struct{}
	active  atomic.Int32
}

func (s *failingUploadStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, options storage.PutOptions) error {
	if !strings.HasPrefix(key, "photos/") {
		return s.Store.Put(ctx, key, body, size, options)
	}
	s.active.Add(1)
	defer s.active.Add(-1)
	s.entered <- struct{}{}
	if key == s.failKey {
		select {
		case <-s.failNow:
			return storage.ErrNetwork
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestFailedUploadCancelsPeersBeforeReturning(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	addRenditions(t, e, &p)
	entry, err := e.inspect(p.Job, p.Job.Photos[0])
	if err != nil {
		t.Fatal(err)
	}
	failure := &failingUploadStore{Store: s, failKey: entry.Key, entered: make(chan struct{}, 3), failNow: make(chan struct{})}
	e.Store = failure
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startRun(ctx, e, p.Job)
	for range 3 {
		receive(t, failure.entered)
	}
	close(failure.failNow)
	out := receive(t, done)
	if !errors.Is(out.err, storage.ErrNetwork) || failure.active.Load() != 0 {
		t.Fatalf("lost original error or left uploads running: %+v", out)
	}
	progress := readProgress(t, e, p.Job)
	if progress.Phase != "uploading" || progress.Completed != 0 {
		t.Fatalf("failed uploads advanced progress: %+v", progress)
	}
	for _, photo := range out.result.Photos {
		if photo.Status != "pending" {
			t.Fatal("failed upload acknowledged")
		}
	}
	e.Store = s
	run(t, e, p.Job)
}
