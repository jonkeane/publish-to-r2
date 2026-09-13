package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jkeane/publish-to-r2/uploader/internal/config"
	"github.com/jkeane/publish-to-r2/uploader/internal/manifest"
	"github.com/jkeane/publish-to-r2/uploader/internal/storage"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeObject struct {
	body []byte
	info storage.Object
}
type fakeStore struct {
	mu         sync.Mutex
	objects    map[string]fakeObject
	puts       map[string]int
	fail       string
	loseCommit bool
	beforePut  func(string)
	failDelete string
	failList   string
	failGet    string
	deletes    []string
}

func newFake() *fakeStore {
	return &fakeStore{objects: map[string]fakeObject{}, puts: map[string]int{}}
}
func (s *fakeStore) Get(ctx context.Context, key string) ([]byte, storage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, storage.Object{}, err
	}
	if s.failGet != "" && strings.Contains(key, s.failGet) {
		return nil, storage.Object{}, storage.ErrNetwork
	}
	o, ok := s.objects[key]
	if !ok {
		return nil, storage.Object{}, storage.ErrNotFound
	}
	return o.body, o.info, nil
}
func (s *fakeStore) Head(ctx context.Context, key string) (storage.Object, error) {
	_, o, err := s.Get(ctx, key)
	return o, err
}
func (s *fakeStore) Put(ctx context.Context, key string, r io.ReadSeeker, size int64, options storage.PutOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.beforePut != nil {
		s.beforePut(key)
	}
	if s.fail != "" && strings.Contains(key, s.fail) {
		return storage.ErrNetwork
	}
	old, exists := s.objects[key]
	if options.Create && exists {
		return storage.ErrConflict
	}
	if options.Match != "" && (!exists || old.info.ETag != options.Match) {
		return storage.ErrConflict
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return errors.New("wrong content length")
	}
	s.puts[key]++
	s.objects[key] = fakeObject{b, storage.Object{Key: key, Size: size, Hash: options.Hash, ETag: config.NewID(), Modified: time.Now().UTC()}}
	if s.loseCommit && strings.HasSuffix(key, "current.json") {
		s.loseCommit = false
		return storage.ErrNetwork
	}
	return nil
}
func (s *fakeStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.failDelete != "" && strings.Contains(key, s.failDelete) {
		return storage.ErrPermission
	}
	s.deletes = append(s.deletes, key)
	delete(s.objects, key)
	return nil
}
func (s *fakeStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.failList != "" && strings.Contains(prefix, s.failList) {
		return nil, storage.ErrNetwork
	}
	out := []storage.Object{}
	for k, o := range s.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, o.info)
		}
	}
	return out, nil
}
func fixture(t *testing.T) (Engine, *fakeStore) {
	t.Helper()
	s := newFake()
	p := config.Profile{Version: 1, Name: "test", Endpoint: "https://account.r2.cloudflarestorage.com", Bucket: "test-bucket", PublicBaseURL: "https://images.example.com", CatalogPath: "/catalog.lrcat", Namespace: "catalog", ServiceID: "service", MaxFileBytes: 1 << 20, SpoolLimitBytes: 10 << 20, HistoryKeep: 1, GraceDays: 30}
	return Engine{Root: t.TempDir(), Profile: p, Store: s}, s
}
func sourceJPEG(t *testing.T, seed uint8) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	img.Set(0, 0, color.RGBA{R: seed, A: 255})
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "same ' 日本 $(touch nope).jpg")
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func prepare(t *testing.T, e Engine, gallery string) Prepared {
	t.Helper()
	p, err := e.Prepare(context.Background(), gallery, "Gallery", e.Profile.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func add(t *testing.T, e Engine, p *Prepared, id string, seed uint8) {
	t.Helper()
	path, err := e.Stage(p.Job.JobID, id, sourceJPEG(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	p.Job.Photos = append(p.Job.Photos, Photo{ID: id, Path: path, Metadata: manifest.PublicMetadata{Caption: "Caption 日本 <script> & \"", Tags: []string{"film"}}})
}
func run(t *testing.T, e Engine, j Job) Result {
	t.Helper()
	r, err := e.Run(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "committed" {
		t.Fatalf("not committed: %+v", r)
	}
	return r
}

func TestGalleryCoverWarnings(t *testing.T) {
	e, _ := fixture(t)
	check := func(p Prepared, count int) {
		t.Helper()
		// Replaying the committed job must return the same warning without JPEGs.
		for attempt := 0; attempt < 2; attempt++ {
			result := run(t, e, p.Job)
			for _, photo := range result.Photos {
				if photo.Status != "committed" {
					t.Fatalf("warning prevented acknowledgment: %+v", photo)
				}
			}
			if count == 1 {
				if len(result.Warnings) != 0 {
					t.Fatalf("one cover produced warnings: %v", result.Warnings)
				}
			} else if len(result.Warnings) != 1 ||
				!strings.Contains(result.Warnings[0], `Gallery "Gallery"`) ||
				!strings.Contains(result.Warnings[0], fmt.Sprintf("has %d photos with the tag gallery-cover", count)) ||
				!strings.Contains(result.Warnings[0], "exactly one photo") {
				t.Fatalf("wrong warning for %d covers: %v", count, result.Warnings)
			}
		}
	}
	p := prepare(t, e, "covers")
	add(t, e, &p, "ordinary", 1)
	p.Job.Photos[0].Metadata.Tags = []string{"not-gallery-cover", "gallery-cover-extra"}
	check(p, 0)

	p = prepare(t, e, "covers")
	add(t, e, &p, "cover-one", 2)
	p.Job.Photos[0].Metadata.Tags = []string{" GALLERY-COVER ", "gallery-cover"}
	check(p, 1) // Duplicate tags still count as one photo.

	p = prepare(t, e, "covers")
	add(t, e, &p, "ordinary", 3)
	check(p, 1) // The cover is already published, outside this export batch.

	p = prepare(t, e, "covers")
	add(t, e, &p, "cover-two", 4)
	p.Job.Photos[0].Metadata.Tags = []string{"gallery-cover"}
	check(p, 2) // Include the previously published cover in the total.

	p = prepare(t, e, "covers")
	add(t, e, &p, "cover-one", 5)
	check(p, 1) // Republishing without the keyword clears that cover.

	p = prepare(t, e, "covers")
	p.Job.Removals = []string{"cover-two"}
	check(p, 0)

	p = prepare(t, e, "covers")
	p.Job.Removals = []string{"ordinary", "cover-one"}
	check(p, 0) // Empty galleries also have no cover.
}

func TestExportedTechnicalMetadata(t *testing.T) {
	e, _ := fixture(t)
	p := prepare(t, e, "film")
	source, err := filepath.Abs("../photometadata/testdata/technical.jpg")
	if err != nil {
		t.Fatal(err)
	}
	path, err := e.Stage(p.Job.JobID, "film-photo", source)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &manifest.EXIF{Model: "Scanner", ISO: "100", Emulsion: "Stale", PreservedFilename: "scan.tif"}
	p.Job.Photos = []Photo{{ID: "film-photo", Path: path, Metadata: manifest.PublicMetadata{Title: "Catalog title", DateTaken: "2000-01-01T00:00:00", EXIF: catalog}}}
	run(t, e, p.Job)
	m, _, err := e.Current(context.Background(), "film")
	if err != nil || len(m.Entries) != 1 {
		t.Fatalf("manifest: %+v, %v", m, err)
	}
	got := m.Entries[0]
	if got.EXIF == nil || got.EXIF.Emulsion != "Kodak Portra 400" || got.EXIF.ISO != "800" || got.EXIF.Make != "Not film stock" || got.EXIF.Model != "Film Camera" || got.Title != "Catalog title" || got.DateTaken != "2025-07-06T14:03:02.125-05:00" || got.EXIF.Time != got.DateTaken || got.EXIF.PreservedFilename != "scan.tif" {
		t.Fatalf("exported film metadata lost or mixed with catalog metadata: %+v", got)
	}
	if catalog.Emulsion != "Stale" {
		t.Fatal("mutated input metadata")
	}
	// A subsequent JPEG without metadata must remove every old technical field.
	p = prepare(t, e, "film")
	add(t, e, &p, "film-photo", 20)
	p.Job.Photos[0].Metadata.EXIF = catalog
	p.Job.Photos[0].Metadata.DateTaken = "2000-01-01T00:00:00"
	run(t, e, p.Job)
	m, _, err = e.Current(context.Background(), "film")
	if err != nil {
		t.Fatal(err)
	}
	if *m.Entries[0].EXIF != (manifest.EXIF{PreservedFilename: "scan.tif"}) || m.Entries[0].DateTaken != "" {
		t.Fatalf("missing film retained stale value: %+v, %v", m, err)
	}
}

func TestMalformedExportedEXIFDoesNotCommit(t *testing.T) {
	e, store := fixture(t)
	initial := prepare(t, e, "metadata-error")
	add(t, e, &initial, "photo", 1)
	run(t, e, initial.Job)
	before := append([]byte(nil), store.objects[manifest.CurrentKey("metadata-error")].body...)

	next := prepare(t, e, "metadata-error")
	source := sourceJPEG(t, 2)
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	// A valid JPEG container with a truncated EXIF TIFF header.
	bad := append([]byte{0xff, 0xd8, 0xff, 0xe1, 0, 11}, []byte("Exif\x00\x00bad")...)
	bad = append(bad, data[2:]...)
	if err := os.WriteFile(source, bad, 0600); err != nil {
		t.Fatal(err)
	}
	path, err := e.Stage(next.Job.JobID, "photo", source)
	if err != nil {
		t.Fatal(err)
	}
	next.Job.Photos = []Photo{{ID: "photo", Path: path}}
	if _, err := e.Run(context.Background(), next.Job); err == nil || !strings.Contains(err.Error(), "JPEG metadata") {
		t.Fatalf("malformed EXIF was not rejected: %v", err)
	}
	if !bytes.Equal(before, store.objects[manifest.CurrentKey("metadata-error")].body) {
		t.Fatal("malformed EXIF changed the live gallery")
	}
}

func TestIncrementalIdentityAndMetadata(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	add(t, e, &p, "photo1", 20)
	add(t, e, &p, "virtual-copy", 20)
	r := run(t, e, p.Job)
	if len(r.Photos) != 2 || r.Photos[0].Key == r.Photos[1].Key {
		t.Fatal("duplicate basenames replaced identity")
	}
	if _, err := os.Stat(p.Job.Photos[0].Path); !os.IsNotExist(err) {
		t.Fatal("successful staging file retained")
	}
	puts := len(s.puts)
	again := run(t, e, p.Job)
	if again.Revision != r.Revision || len(s.puts) != puts {
		t.Fatal("successful retry wrote duplicate objects")
	}
	q := prepare(t, e, "gallery")
	add(t, e, &q, "photo1", 20)
	q.Job.Photos[0].Metadata.Caption = "Caption-only update"
	r2 := run(t, e, q.Job)
	if r2.Photos[0].Key != r.Photos[0].Key || s.puts[r.Photos[0].Key] != 1 {
		t.Fatal("unchanged bytes uploaded again")
	}
	m, _, _ := e.Current(context.Background(), "gallery")
	if len(m.Entries) != 2 || m.Entries[0].Caption != "Caption-only update" {
		t.Fatal("incremental merge lost entries or metadata")
	}
	q = prepare(t, e, "gallery")
	add(t, e, &q, "photo1", 255)
	r3 := run(t, e, q.Job)
	if r3.Photos[0].Key == r.Photos[0].Key {
		t.Fatal("edit reused immutable URL")
	}
	q = prepare(t, e, "gallery")
	q.Job.Title = "Renamed"
	q.Job.Removals = []string{"photo1"}
	run(t, e, q.Job)
	m, _, _ = e.Current(context.Background(), "gallery")
	if m.Title != "Renamed" || len(m.Entries) != 1 || m.Entries[0].ID != "virtual-copy" {
		t.Fatal("explicit removal failed")
	}
	if _, err := e.Run(context.Background(), p.Job); err == nil {
		t.Fatal("stale successful replay should conflict")
	}
}
func TestFailureLeavesOldGalleryAndResumes(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	add(t, e, &p, "old", 1)
	old := run(t, e, p.Job)
	q := prepare(t, e, "gallery")
	add(t, e, &q, "first", 2)
	add(t, e, &q, "second", 3)
	s.fail = "/second/"
	r, err := e.Run(context.Background(), q.Job)
	if err == nil || r.Status == "committed" {
		t.Fatal("failure reported success")
	}
	m, _, _ := e.Current(context.Background(), "gallery")
	if m.Revision != old.Revision {
		t.Fatal("partial gallery committed")
	}
	s.fail = ""
	run(t, e, q.Job)
	for key, n := range s.puts {
		if strings.Contains(key, "/first/") && n != 1 {
			t.Fatal("resume duplicated upload")
		}
	}
}
func TestLostCommitResponseReconciles(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	add(t, e, &p, "photo", 3)
	s.loseCommit = true
	if _, err := e.Run(context.Background(), p.Job); err == nil {
		t.Fatal("expected uncertain commit")
	}
	r := run(t, e, p.Job)
	if s.puts[manifest.CurrentKey("gallery")] != 1 {
		t.Fatal("recovery rewrote current")
	}
	if r.Photos[0].Status != "committed" {
		t.Fatal("recovery acknowledgment failed")
	}
}
func TestConditionalWriteStopsCompetingWriter(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	add(t, e, &p, "photo", 5)
	s.beforePut = func(key string) {
		if key == manifest.CurrentKey("gallery") {
			s.objects[key] = fakeObject{[]byte("another writer"), storage.Object{Key: key, ETag: "other"}}
		}
	}
	if _, err := e.Run(context.Background(), p.Job); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("expected CAS conflict: %v", err)
	}
	if string(s.objects[manifest.CurrentKey("gallery")].body) != "another writer" {
		t.Fatal("clobbered another writer")
	}
}
func TestInvalidProtocolPathsAndCancellation(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Prepared, Engine)
	}{
		{"version", func(p *Prepared, e Engine) { p.Job.Version = 2 }},
		{"catalog clone", func(p *Prepared, e Engine) { p.Job.CatalogPath = "/cloned.lrcat" }},
		{"destination", func(p *Prepared, e Engine) { p.Job.Namespace = "other" }},
		{"outside staging", func(p *Prepared, e Engine) { p.Job.Photos[0].Path = sourceJPEG(t, 1) }},
		{"symlink", func(p *Prepared, e Engine) {
			path := p.Job.Photos[0].Path
			os.Remove(path)
			os.Symlink(sourceJPEG(t, 1), path)
		}},
		{"cancel", func(p *Prepared, e Engine) { os.WriteFile(filepath.Join(p.Directory, "cancel"), nil, 0600) }},
		{"duplicate identity", func(p *Prepared, e Engine) { p.Job.Photos = append(p.Job.Photos, p.Job.Photos[0]) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, s := fixture(t)
			p := prepare(t, e, "gallery")
			add(t, e, &p, "photo", 1)
			test.change(&p, e)
			if _, err := e.Run(context.Background(), p.Job); err == nil {
				t.Fatal("invalid job accepted")
			}
			if len(s.objects) != 0 {
				t.Fatal("invalid job changed remote state")
			}
		})
	}
}
func TestReusedJobIDAndChangedStaging(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	add(t, e, &p, "photo", 3)
	s.fail = "photos/"
	e.Run(context.Background(), p.Job)
	changed := p.Job
	changed.Title = "Different request"
	if _, err := e.Run(context.Background(), changed); err == nil {
		t.Fatal("job ID reused")
	}
	b, _ := os.ReadFile(sourceJPEG(t, 255))
	os.WriteFile(p.Job.Photos[0].Path, b, 0600)
	s.fail = ""
	if _, err := e.Run(context.Background(), p.Job); err == nil {
		t.Fatal("changed staging accepted")
	}
}
func TestStageLimitsAndLock(t *testing.T) {
	e, _ := fixture(t)
	p := prepare(t, e, "gallery")
	e.Profile.SpoolLimitBytes = 10
	if _, err := e.Stage(p.Job.JobID, "photo", sourceJPEG(t, 1)); err == nil {
		t.Fatal("spool bound ignored")
	}
	unlock, err := Lock(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Lock(e.Root); err == nil {
		t.Fatal("second writer acquired lock")
	}
}
func TestCleanupReferencesAndReview(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "one")
	add(t, e, &p, "shared", 5)
	r := run(t, e, p.Job)
	legacy, _, err := e.Current(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	legacy.GalleryID = "two"
	if err := putJSON(context.Background(), s, manifest.CurrentKey("two"), legacy, storage.PutOptions{Create: true}); err != nil {
		t.Fatal(err)
	}
	p = prepare(t, e, "one")
	p.Job.Removals = []string{"shared"}
	run(t, e, p.Job)
	orphan := "photos/catalog/orphan/" + strings.Repeat("a", 64) + ".jpg"
	s.objects[orphan] = fakeObject{[]byte("x"), storage.Object{Key: orphan, ETag: "orphan", Size: 1, Modified: time.Now().Add(-40 * 24 * time.Hour)}}
	for key, o := range s.objects {
		if key == r.Photos[0].Key {
			o.info.Modified = time.Now().Add(-40 * 24 * time.Hour)
			s.objects[key] = o
		}
	}
	report, err := e.PlanCleanup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Objects) != 1 || report.Objects[0] != orphan {
		t.Fatal("cleanup proposed shared photo deletion")
	}
	s.objects[orphan] = fakeObject{[]byte("xx"), storage.Object{Key: orphan, ETag: "changed", Size: 2, Modified: time.Now().Add(-40 * 24 * time.Hour)}}
	if err = e.ApplyCleanup(context.Background(), report); err == nil {
		t.Fatal("stale review accepted")
	}
	report, err = e.PlanCleanup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = e.ApplyCleanup(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.objects[r.Photos[0].Key]; !ok {
		t.Fatal("shared image deleted")
	}
	s.objects["galleries/unknown/current.json"] = fakeObject{[]byte(`{"schemaVersion":999}`), storage.Object{Key: "galleries/unknown/current.json"}}
	if _, err = e.PlanCleanup(context.Background()); err == nil {
		t.Fatal("cleanup ignored unknown manifest")
	}
}
func TestScaleAndUnknownFields(t *testing.T) {
	old := manifest.Manifest{Entries: make([]manifest.Entry, 6000)}
	for i := range old.Entries {
		old.Entries[i] = manifest.Entry{ID: fmt.Sprintf("photo-%d", i)}
	}
	entries, err := manifest.Merge(old, []manifest.Entry{{ID: "photo-5999", PublicMetadata: manifest.PublicMetadata{Caption: "changed"}}}, []string{"photo-1"}, nil)
	if err != nil || len(entries) != 5999 || entries[len(entries)-1].Caption != "changed" {
		t.Fatal("scale merge failed")
	}
	var job Job
	if err = manifest.Decode(strings.NewReader(`{"version":1,"secretAccessKey":"never"}`), &job); err == nil {
		t.Fatal("unknown job fields allowed")
	}
	if err = manifest.Decode(strings.NewReader(`{} {}`), &job); err == nil {
		t.Fatal("trailing JSON allowed")
	}
}
func TestDoctor(t *testing.T) {
	e, s := fixture(t)
	if err := e.Doctor(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.objects) != 0 {
		t.Fatal("diagnostic object leaked")
	}
}
func TestJournalBeforeNetwork(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	add(t, e, &p, "photo", 3)
	s.beforePut = func(key string) {
		data, err := os.ReadFile(JournalPath(e.Root, p.Job.JobID))
		if err != nil {
			t.Fatal("network change before journal")
		}
		var j Journal
		if json.Unmarshal(data, &j) != nil || j.State != "prepared" {
			t.Fatal("invalid journal")
		}
	}
	run(t, e, p.Job)
}
