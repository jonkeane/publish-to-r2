package jobs

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jonkeane/publish-to-r2/uploader/internal/manifest"
)

func addRenditions(t *testing.T, e Engine, p *Prepared) {
	t.Helper()
	add(t, e, p, "photo", 20)
	photo := &p.Job.Photos[len(p.Job.Photos)-1]
	photo.Renditions = map[string]string{}
	for n, name := range manifest.RenditionNames {
		path, err := e.Stage(p.Job.JobID, photo.ID, sourceJPEG(t, uint8(120+n*135)), name)
		if err != nil {
			t.Fatal(err)
		}
		photo.Renditions[name] = path
	}
}

func TestRenditionsCommitRetryAndCleanupRetention(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	addRenditions(t, e, &p)
	entry, err := e.inspect(p.Job, p.Job.Photos[0])
	if err != nil {
		t.Fatal(err)
	}
	// Fail the final rendition after the first two uploads have succeeded.
	s.fail = manifest.StagingKey(p.Job.Namespace, "photo", entry.Renditions["gallery"].SHA256)
	if r, err := e.Run(context.Background(), p.Job); err == nil || r.Status == "committed" {
		t.Fatal("partial set committed")
	}
	m, _, err := e.Current(context.Background(), "gallery")
	if err != nil || len(m.Entries) != 0 {
		t.Fatal("failed publish changed gallery")
	}
	for _, file := range p.Job.Photos[0].files() {
		if _, err := os.Stat(file.Path); err != nil {
			t.Fatal("retry file removed", err)
		}
	}
	s.fail = ""
	s.loseCommit = true
	if _, err := e.Run(context.Background(), p.Job); err == nil {
		t.Fatal("lost acknowledgment was not simulated")
	}
	result := run(t, e, p.Job)
	if len(result.Photos) != 1 {
		t.Fatal("renditions became separate published photos")
	}
	for _, image := range entry.Images() {
		if s.puts[image.Key] != 1 {
			t.Fatal("rendition re-uploaded or missing", image.Key)
		}
	}
	for _, file := range p.Job.Photos[0].files() {
		if _, err := os.Stat(file.Path); !os.IsNotExist(err) {
			t.Fatal("successful rendition left in spool")
		}
	}
	run(t, e, p.Job) // Fully committed replay requires no files.
	m, _, err = e.Current(context.Background(), "gallery")
	if err != nil || len(m.Entries[0].Renditions) != 2 {
		t.Fatal("manifest lost variants", err)
	}
	// All old objects referenced by a current manifest are retained.
	age := func() {
		for k, o := range s.objects {
			o.info.Modified = time.Now().Add(-60 * 24 * time.Hour)
			s.objects[k] = o
		}
	}
	age()
	report, err := e.PlanCleanup(context.Background())
	if err != nil || len(report.Objects) != 0 {
		t.Fatal("cleanup would delete current renditions", err)
	}
	q := prepare(t, e, "gallery")
	q.Job.Removals = []string{"photo"}
	run(t, e, q.Job)
	age()
	e.Profile.HistoryKeep = 2
	report, err = e.PlanCleanup(context.Background())
	if err != nil || len(report.Objects) != 0 {
		t.Fatal("cleanup would delete historical renditions", err)
	}
	// Removed stable public objects are deleted as part of publication cleanup;
	// only the expired history manifest remains for maintenance to collect.
	e.Profile.HistoryKeep = 0
	report, err = e.PlanCleanup(context.Background())
	if err != nil || len(report.Objects) != 0 {
		t.Fatalf("unexpected unreferenced rendition set: %+v %v", report, err)
	}
}

func TestRenditionsVerifyUnchangedReferencesAndTamperedStaging(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	addRenditions(t, e, &p)
	entry, err := e.inspect(p.Job, p.Job.Photos[0])
	if err != nil {
		t.Fatal(err)
	}
	s.fail = manifest.StagingKey(p.Job.Namespace, "photo", entry.Renditions["gallery"].SHA256)
	if _, err := e.Run(context.Background(), p.Job); err == nil {
		t.Fatal("expected failed upload")
	}
	s.fail = ""
	path := p.Job.Photos[0].Renditions["gallery"]
	original, _ := os.ReadFile(path)
	changed, _ := os.ReadFile(sourceJPEG(t, 70))
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(context.Background(), p.Job); err == nil {
		t.Fatal("tampered rendition uploaded")
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	run(t, e, p.Job)
	q := prepare(t, e, "gallery")
	q.Job.Title = "Rename"
	delete(s.objects, entry.Renditions["thumbnail"].Key)
	if _, err := e.Run(context.Background(), q.Job); err == nil {
		t.Fatal("missing unchanged thumbnail not verified")
	}
}

func TestRenditionValidationAndDeduplication(t *testing.T) {
	e, s := fixture(t)
	p := prepare(t, e, "gallery")
	addRenditions(t, e, &p)
	for _, bad := range []map[string]string{
		{}, {"thumbnail": "/one"}, {"thumbnail": "/one", "other": "/two"}, {"thumbnail": "relative", "gallery": "/two"},
	} {
		j := p.Job
		j.Photos = append([]Photo(nil), j.Photos...)
		j.Photos[0].Renditions = bad
		if Validate(j, e.Profile) == nil {
			t.Fatal("invalid job rendition set accepted")
		}
	}
	if _, err := e.Stage(p.Job.JobID, "photo", sourceJPEG(t, 0), "../bad"); err == nil {
		t.Fatal("invalid stage name accepted")
	}
	if _, err := e.Stage(p.Job.JobID, "photo", sourceJPEG(t, 0), "thumbnail"); err == nil {
		t.Fatal("duplicate staged rendition accepted")
	}
	// No-enlargement exports of a tiny original may have identical bytes.
	for _, path := range p.Job.Photos[0].Renditions {
		b, _ := os.ReadFile(p.Job.Photos[0].Path)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	result := run(t, e, p.Job)
	if s.puts[result.Photos[0].Key] != 1 {
		t.Fatal("identical renditions were not deduplicated")
	}
	m, _, _ := e.Current(context.Background(), "gallery")
	for _, mutate := range []func(*manifest.Manifest){
		func(m *manifest.Manifest) { delete(m.Entries[0].Renditions, "thumbnail") },
		func(m *manifest.Manifest) {
			i := m.Entries[0].Renditions["thumbnail"]
			i.Source = "https://foreign.example/" + i.Key
			m.Entries[0].Renditions["thumbnail"] = i
		},
		func(m *manifest.Manifest) {
			i := m.Entries[0].Renditions["thumbnail"]
			i.Width = 0
			m.Entries[0].Renditions["thumbnail"] = i
		},
		func(m *manifest.Manifest) {
			i := m.Entries[0].Renditions["thumbnail"]
			i.SHA256 = "not-a-hash"
			m.Entries[0].Renditions["thumbnail"] = i
		},
	} {
		b, _ := json.Marshal(m)
		var invalid manifest.Manifest
		_ = json.Unmarshal(b, &invalid)
		mutate(&invalid)
		if invalid.Validate() == nil {
			t.Fatal("invalid public rendition accepted")
		}
	}
}
