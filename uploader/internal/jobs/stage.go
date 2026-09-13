package jobs

import (
	"context"
	"errors"
	"github.com/jkeane/publish-to-r2/uploader/internal/config"
	"github.com/jkeane/publish-to-r2/uploader/internal/manifest"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
)

type Prepared struct {
	Job           Job              `json:"job"`
	JobPath       string           `json:"jobPath"`
	Directory     string           `json:"directory"`
	PublicBaseURL string           `json:"publicBaseUrl"`
	Entries       []manifest.Entry `json:"entries"`
}

func (e Engine) Prepare(ctx context.Context, gallery, title, catalog string) (Prepared, error) {
	var out Prepared
	if filepath.Clean(catalog) != filepath.Clean(e.Profile.CatalogPath) {
		return out, errors.New("catalog path does not match profile; cloned catalogs require a new profile")
	}
	unlock, err := activeLock(e.Root)
	if err != nil {
		return out, err
	}
	defer unlock()
	m, _, err := e.Current(ctx, gallery)
	if err != nil {
		return out, err
	}
	releaseSpool, err := spoolLock(e.Root)
	if err != nil {
		return out, err
	}
	defer releaseSpool()
	id := config.NewID()
	dir := JobDir(e.Root, id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return out, err
	}
	j := Job{Version: 1, JobID: id, Profile: e.Profile.Name, CatalogPath: catalog, Namespace: e.Profile.Namespace, ServiceID: e.Profile.ServiceID, GalleryID: gallery, ExpectedRevision: m.Revision, Title: title, Photos: []Photo{}, Removals: []string{}}
	out = Prepared{Job: j, JobPath: filepath.Join(dir, "job.json"), Directory: dir, PublicBaseURL: e.Profile.PublicBaseURL, Entries: m.Entries}
	return out, config.AtomicJSON(out.JobPath, j)
}

// Stage owns the copy before Lightroom may remove its rendered temporary file.
func (e Engine) Stage(id, photoID, source string, rendition ...string) (string, error) {
	if !manifest.ValidID(id) || !manifest.ValidID(photoID) || !filepath.IsAbs(source) {
		return "", errors.New("invalid staging request")
	}
	unlock, err := activeLock(e.Root)
	if err != nil {
		return "", err
	}
	defer unlock()
	releaseJob, err := resourceLock(context.Background(), e.Root, "job", id)
	if err != nil {
		return "", err
	}
	defer releaseJob()
	releaseSpool, err := spoolLock(e.Root)
	if err != nil {
		return "", err
	}
	defer releaseSpool()
	dir := JobDir(e.Root, id)
	j, err := Load(filepath.Join(dir, "job.json"))
	if err != nil {
		return "", err
	}
	if err = Validate(j, e.Profile); err != nil {
		return "", err
	}
	if j.JobID != id {
		return "", errors.New("staging job mismatch")
	}
	if _, err = os.Stat(JournalPath(e.Root, id)); !os.IsNotExist(err) {
		return "", errors.New("cannot change a prepared job")
	}
	if err = OwnedPath(filepath.Join(e.Root, "spool"), dir); err != nil {
		return "", err
	}
	f, err := os.Open(source)
	if err != nil {
		return "", errors.New("cannot open rendered JPEG")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > e.Profile.MaxFileBytes {
		return "", errors.New("rendered JPEG exceeds file limit")
	}
	if _, err = jpeg.DecodeConfig(f); err != nil {
		return "", errors.New("only JPEG renditions can be staged")
	}
	if _, err = f.Seek(0, 0); err != nil {
		return "", err
	}
	used, err := SpoolBytes(e.Root)
	if err != nil {
		return "", err
	}
	if used+info.Size()+1<<20 > e.Profile.SpoolLimitBytes {
		return "", errors.New("spool limit reached; discard old failed jobs")
	}
	suffix := ""
	if len(rendition) > 1 {
		return "", errors.New("invalid rendition")
	}
	if len(rendition) == 1 && rendition[0] != "" {
		if !manifest.ValidRendition(rendition[0]) {
			return "", errors.New("invalid rendition")
		}
		suffix = "." + rendition[0]
	}
	dest := filepath.Join(dir, photoID+suffix+".jpg")
	if _, err = os.Lstat(dest); !os.IsNotExist(err) {
		return "", errors.New("duplicate staged identity")
	}
	tmp, err := os.CreateTemp(dir, ".stage-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	n, copyErr := io.Copy(tmp, io.LimitReader(f, info.Size()+1))
	if copyErr == nil && n != info.Size() {
		copyErr = errors.New("rendered JPEG changed during staging")
	}
	if copyErr == nil {
		copyErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if copyErr != nil {
		return "", errors.New("staging copy failed; check disk space")
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err = os.Rename(tmp.Name(), dest); err != nil {
		return "", err
	}
	return dest, nil
}
