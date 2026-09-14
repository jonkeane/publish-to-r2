package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonkeane/publish-to-r2/uploader/internal/config"
	"github.com/jonkeane/publish-to-r2/uploader/internal/manifest"
	"github.com/jonkeane/publish-to-r2/uploader/internal/photometadata"
	"github.com/jonkeane/publish-to-r2/uploader/internal/storage"
)

type Engine struct {
	Root    string
	Profile config.Profile
	Store   storage.Store
}

func (e Engine) Current(ctx context.Context, gallery string) (manifest.Manifest, string, error) {
	var m manifest.Manifest
	if !manifest.ValidID(gallery) {
		return m, "", errors.New("invalid gallery ID")
	}
	b, o, err := e.Store.Get(ctx, manifest.CurrentKey(gallery))
	if errors.Is(err, storage.ErrNotFound) {
		return manifest.Manifest{Entries: []manifest.Entry{}}, "", nil
	}
	if err != nil {
		return m, "", err
	}
	if err = manifest.Decode(bytes.NewReader(b), &m); err != nil {
		return m, "", err
	}
	if err = m.Validate(); err != nil {
		return m, "", err
	}
	if m.GalleryID != gallery || m.Namespace != e.Profile.Namespace || m.ServiceID != e.Profile.ServiceID {
		return m, "", errors.New("gallery belongs to another catalog or service")
	}
	if o.ETag == "" {
		return m, "", errors.New("remote manifest has no ETag")
	}
	return m, o.ETag, nil
}
func code(err error) string {
	switch {
	case errors.Is(err, storage.ErrConflict):
		return "revision_conflict"
	case errors.Is(err, storage.ErrPermission):
		return "permission_denied"
	case errors.Is(err, storage.ErrNetwork):
		return "network_error"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "validation_or_io_error"
	}
}
func (e Engine) Run(ctx context.Context, j Job) (result Result, runErr error) {
	result = Result{Version: 1, JobID: j.JobID, Status: "failed", Photos: []PhotoResult{}}
	if err := Validate(j, e.Profile); err != nil {
		result.ErrorCode = code(err)
		result.Message = err.Error()
		return result, err
	}
	unlock, err := activeLock(e.Root)
	if err != nil {
		return result, err
	}
	defer unlock()
	releasePublication, err := e.publicationLocks(ctx, j)
	if err != nil {
		return result, err
	}
	defer releasePublication()
	if err = OwnedPath(e.Root, JobDir(e.Root, j.JobID)); err != nil {
		return result, err
	}
	resultPath := filepath.Join(JobDir(e.Root, j.JobID), "result.json")
	defer func() {
		if runErr != nil {
			result.ErrorCode = code(runErr)
			result.Message = runErr.Error()
			for i := range result.Photos {
				result.Photos[i].Status = "pending"
				result.Photos[i].ErrorCode = result.ErrorCode
			}
		}
		if err := config.AtomicJSON(resultPath, result); err != nil && runErr == nil {
			runErr = errors.New("remote committed but result could not be saved; reconcile before retry")
		}
	}()
	for _, p := range j.Photos {
		result.Photos = append(result.Photos, PhotoResult{ID: p.ID, Status: "pending"})
	}
	var journal Journal
	jp := JournalPath(e.Root, j.JobID)
	f, err := os.Open(jp)
	if err == nil {
		err = manifest.Decode(f, &journal)
		f.Close()
		if err != nil {
			return result, err
		}
		if journal.Version != 1 || journal.Digest != Digest(j) {
			return result, errors.New("job ID reused with different contents")
		}
		if journal.Digest != Digest(journal.Job) || journal.Manifest.Validate() != nil ||
			journal.Manifest.GalleryID != j.GalleryID || journal.Manifest.Namespace != j.Namespace ||
			journal.Manifest.ServiceID != j.ServiceID || journal.Manifest.ParentRevision != j.ExpectedRevision ||
			(journal.State != "prepared" && journal.State != "committed") {
			return result, errors.New("journal contents are inconsistent; refusing recovery")
		}
	} else if !os.IsNotExist(err) {
		return result, errors.New("cannot read journal")
	}
	e.progress(j, "preparing", 0, 0)
	current, etag, err := e.Current(ctx, j.GalleryID)
	if err != nil {
		return result, err
	}
	releaseOwnership, err := e.reservePhotos(ctx, j)
	if err != nil {
		return result, err
	}
	defer releaseOwnership()
	if err = e.checkPhotoOwnership(ctx, j); err != nil {
		return result, err
	}
	if journal.Digest != "" && current.Revision == journal.Manifest.Revision {
		if !sameManifest(current, journal.Manifest) {
			return result, errors.New("remote revision contents differ from journal")
		}
		return e.finish(ctx, journal, result)
	}
	if current.Revision != j.ExpectedRevision {
		return result, storage.ErrConflict
	}
	if journal.State == "committed" {
		return result, storage.ErrConflict
	}
	if _, err = os.Stat(filepath.Join(JobDir(e.Root, j.JobID), "cancel")); err == nil {
		return result, context.Canceled
	}
	releaseSpool, err := spoolLock(e.Root)
	if err != nil {
		return result, err
	}
	used, err := SpoolBytes(e.Root)
	releaseSpool()
	if err != nil {
		return result, err
	}
	if used > e.Profile.SpoolLimitBytes {
		return result, errors.New("spool limit exceeded; discard old failed jobs explicitly")
	}
	if journal.Digest == "" {
		e.progress(j, "inspecting", 0, len(j.Photos))
		updates := make([]manifest.Entry, 0, len(j.Photos))
		previous := map[string]manifest.Entry{}
		for _, p := range current.Entries {
			previous[p.ID] = p
		}
		for _, p := range j.Photos {
			if err = ctx.Err(); err != nil {
				return result, err
			}
			entry, err := e.inspect(j, p)
			if err != nil {
				return result, err
			}
			entry.DateUpload = strconv.FormatInt(time.Now().Unix(), 10)
			entry.LastUpdate = entry.DateUpload
			if old, ok := previous[p.ID]; ok {
				entry.DateUpload = old.DateUpload
			}
			updates = append(updates, entry)
			e.progress(j, "inspecting", len(updates), len(j.Photos))
		}
		entries, err := manifest.Merge(current, updates, j.Removals, j.Order)
		if err != nil {
			return result, err
		}
		m := manifest.Manifest{SchemaVersion: 1, Revision: config.NewID(), ParentRevision: current.Revision, GalleryID: j.GalleryID, Namespace: j.Namespace, ServiceID: j.ServiceID, Title: j.Title, UpdatedAt: time.Now().UTC(), Entries: entries}
		if err = m.Validate(); err != nil {
			return result, err
		}
		journal = Journal{Version: 1, Digest: Digest(j), State: "prepared", Job: j, Manifest: m, ETag: etag}
		if err = config.AtomicJSON(jp, journal); err != nil {
			return result, errors.New("cannot persist journal; no network changes made")
		}
	}
	byID := map[string]manifest.Entry{}
	verifyTotal := 0
	for _, entry := range journal.Manifest.Entries {
		byID[entry.ID] = entry
		verifyTotal += len(entry.Images())
	}
	var uploads []uploadWork
	for _, p := range j.Photos {
		entry, ok := byID[p.ID]
		if !ok {
			return result, errors.New("incomplete journal")
		}
		images, files := entry.Images(), p.files()
		if len(images) != len(files) {
			return result, errors.New("journal rendition set differs from job")
		}
		for n, file := range files {
			uploads = append(uploads, uploadWork{file, images[n]})
		}
	}
	if err = e.uploadAll(ctx, j, uploads); err != nil {
		return result, err
	}
	for i, p := range j.Photos {
		entry := byID[p.ID]
		result.Photos[i] = PhotoResult{ID: p.ID, Status: "uploaded", Key: entry.Key, URL: entry.Source, Bytes: entry.Bytes, SHA256: entry.SHA256}
	}
	// Verify all references, including unchanged entries, before publishing a manifest.
	verified := 0
	e.progress(j, "verifying", 0, verifyTotal)
	for _, entry := range journal.Manifest.Entries {
		for _, image := range entry.Images() {
			o, err := e.Store.Head(ctx, image.Key)
			if err != nil {
				return result, err
			}
			if o.Hash != image.SHA256 || o.Size != image.Bytes {
				return result, errors.New("remote object verification failed")
			}
			verified++
			e.progress(j, "verifying", verified, verifyTotal)
		}
	}
	e.progress(j, "committing", 0, 2)
	if err = putJSON(ctx, e.Store, manifest.HistoryKey(j.GalleryID, journal.Manifest.Revision), journal.Manifest, storage.PutOptions{Create: true, ContentType: "application/json", CacheControl: "private, no-store"}); err != nil {
		if !errors.Is(err, storage.ErrConflict) {
			return result, err
		}
		b, _, readErr := e.Store.Get(ctx, manifest.HistoryKey(j.GalleryID, journal.Manifest.Revision))
		var m manifest.Manifest
		if readErr != nil || manifest.Decode(bytes.NewReader(b), &m) != nil || !sameManifest(m, journal.Manifest) {
			return result, errors.New("history revision collision")
		}
	}
	e.progress(j, "committing", 1, 2)
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = putJSON(ctx, e.Store, manifest.CurrentKey(j.GalleryID), journal.Manifest, storage.PutOptions{Create: journal.ETag == "", Match: journal.ETag, ContentType: "application/json", CacheControl: "no-store, max-age=0"}); err != nil {
		return result, err
	}
	e.progress(j, "committing", 2, 2)
	return e.finish(ctx, journal, result)
}
func sameManifest(a, b manifest.Manifest) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func putJSON(ctx context.Context, s storage.Store, key string, v any, o storage.PutOptions) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > manifest.MaxJSON {
		return errors.New("manifest exceeds JSON limit")
	}
	return s.Put(ctx, key, bytes.NewReader(b), int64(len(b)), o)
}
func (e Engine) inspect(j Job, p Photo) (manifest.Entry, error) {
	var entry manifest.Entry
	if err := OwnedPath(JobDir(e.Root, j.JobID), p.Path); err != nil {
		return entry, err
	}
	f, err := os.Open(p.Path)
	if err != nil {
		return entry, errors.New("cannot open staged JPEG")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > e.Profile.MaxFileBytes {
		return entry, errors.New("JPEG exceeds file limit or is not a regular file")
	}
	dim, err := jpeg.DecodeConfig(f)
	if err != nil {
		return entry, errors.New("only rendered JPEG images are supported")
	}
	if _, err = f.Seek(0, 0); err != nil {
		return entry, err
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return entry, errors.New("cannot hash JPEG")
	}
	hash := hex.EncodeToString(h.Sum(nil))
	if _, err = f.Seek(0, 0); err != nil {
		return entry, err
	}
	exif, err := photometadata.Read(f)
	if err != nil {
		return entry, fmt.Errorf("cannot read JPEG metadata: %w", err)
	}
	// Technical metadata is authoritative in the exported file. Do not retain
	// catalog EXIF (including stale scanner settings) when an exported tag is absent.
	if p.Metadata.EXIF != nil {
		exif.PreservedFilename = p.Metadata.EXIF.PreservedFilename
	}
	p.Metadata.EXIF = &exif
	p.Metadata.DateTaken = exif.Time
	key := manifest.Key(j.Namespace, p.ID, hash)
	entry = manifest.Entry{ID: p.ID, Key: key, Source: manifest.URL(e.Profile.PublicBaseURL, key), Width: dim.Width, Height: dim.Height, Bytes: info.Size(), SHA256: hash, OriginalFormat: "jpg", PublicMetadata: p.Metadata}
	if p.Renditions != nil {
		entry.Renditions = map[string]manifest.Image{}
		for _, name := range manifest.RenditionNames {
			child, err := e.inspect(j, Photo{ID: p.ID, Path: p.Renditions[name]})
			if err != nil {
				return entry, err
			}
			entry.Renditions[name] = child.Image()
		}
	}
	return entry, nil
}
func (e Engine) upload(ctx context.Context, j Job, p Photo, entry manifest.Image) error {
	o, err := e.Store.Head(ctx, entry.Key)
	if err == nil {
		if o.Hash == entry.SHA256 && o.Size == entry.Bytes {
			return nil
		}
		return errors.New("immutable key exists with different bytes")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	actual, err := e.inspect(j, p)
	if err != nil {
		return err
	}
	if actual.SHA256 != entry.SHA256 || actual.Bytes != entry.Bytes {
		return errors.New("staged JPEG changed after job was prepared")
	}
	f, err := os.Open(p.Path)
	if err != nil {
		return errors.New("cannot open staged JPEG")
	}
	defer f.Close()
	err = e.Store.Put(ctx, entry.Key, f, entry.Bytes, storage.PutOptions{Create: true, Hash: entry.SHA256, ContentType: "image/jpeg", CacheControl: "public, max-age=31536000, immutable"})
	if err != nil && !errors.Is(err, storage.ErrConflict) {
		return err
	}
	o, err = e.Store.Head(ctx, entry.Key)
	if err != nil {
		return err
	}
	if o.Hash != entry.SHA256 || o.Size != entry.Bytes {
		return errors.New("uploaded JPEG failed verification")
	}
	return nil
}
func (e Engine) finish(ctx context.Context, journal Journal, result Result) (Result, error) {
	e.progress(journal.Job, "cleanup", 0, 0)
	journal.State = "committed"
	if err := config.AtomicJSON(JournalPath(e.Root, journal.Job.JobID), journal); err != nil {
		return result, errors.New("remote committed; journal save failed; reconcile this job")
	}
	// Keep the journal and retry bytes until obsolete versions are gone. Recovery
	// enters here directly when the current manifest already matches this job.
	if err := e.cleanupPublishedPhotos(ctx, journal); err != nil {
		return result, fmt.Errorf("remote committed; photo cleanup incomplete; retry or reconcile this job: %w", err)
	}
	result.Status = "committed"
	result.Revision = journal.Manifest.Revision
	result.ManifestURL = manifest.URL(e.Profile.PublicBaseURL, manifest.CurrentKey(journal.Job.GalleryID))
	byID := map[string]manifest.Entry{}
	coverCount := 0
	for _, entry := range journal.Manifest.Entries {
		byID[entry.ID] = entry
		for _, tag := range entry.Tags {
			if strings.ToLower(strings.TrimSpace(tag)) == "gallery-cover" {
				coverCount++
				break // Count photos, even if a keyword appears more than once.
			}
		}
	}
	if coverCount != 1 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"Gallery %q has %d photos with the tag gallery-cover. Set the tag on exactly one photo and republish.",
			journal.Manifest.Title, coverCount))
	}
	for i, p := range journal.Job.Photos {
		entry := byID[p.ID]
		result.Photos[i] = PhotoResult{ID: p.ID, Status: "committed", Key: entry.Key, URL: entry.Source, Bytes: entry.Bytes, SHA256: entry.SHA256}
	}
	// Persist acknowledgment before removing retry bytes. Replaying a journal needs no JPEGs.
	if err := config.AtomicJSON(filepath.Join(JobDir(e.Root, journal.Job.JobID), "result.json"), result); err != nil {
		return result, errors.New("remote committed; result save failed; reconcile this job")
	}
	releaseSpool, err := spoolLock(e.Root)
	if err != nil {
		return result, err
	}
	defer releaseSpool()
	for _, p := range journal.Job.Photos {
		for _, file := range p.files() {
			if OwnedPath(JobDir(e.Root, journal.Job.JobID), file.Path) == nil {
				_ = os.Remove(file.Path)
			}
		}
	}
	return result, nil
}
func (e Engine) Reconcile(ctx context.Context, gallery string) ([]Result, error) {
	if !manifest.ValidID(gallery) {
		return nil, errors.New("invalid gallery ID")
	}
	paths, err := filepath.Glob(filepath.Join(e.Root, "journals", "*.json"))
	if err != nil {
		return nil, err
	}
	results := []Result{}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		var journal Journal
		err = manifest.Decode(f, &journal)
		f.Close()
		if err != nil {
			return nil, err
		}
		if journal.Job.Profile != e.Profile.Name || journal.Job.GalleryID != gallery {
			continue
		}
		// Run is idempotent and takes the gallery/job locks. A conflict is reported, never rebased.
		r, runErr := e.Run(ctx, journal.Job)
		results = append(results, r)
		if runErr != nil && !errors.Is(runErr, storage.ErrConflict) {
			return results, fmt.Errorf("reconciliation stopped: %w", runErr)
		}
	}
	return results, nil
}
