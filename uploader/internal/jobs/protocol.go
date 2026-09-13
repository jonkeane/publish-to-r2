package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jkeane/publish-to-r2/uploader/internal/config"
	"github.com/jkeane/publish-to-r2/uploader/internal/manifest"
	"os"
	"path/filepath"
	"strings"
)

type Photo struct {
	Renditions map[string]string       `json:"renditions,omitempty"`
	ID         string                  `json:"id"`
	Path       string                  `json:"path"`
	Metadata   manifest.PublicMetadata `json:"metadata"`
}

func (p Photo) files() []Photo {
	files := []Photo{{ID: p.ID, Path: p.Path, Metadata: p.Metadata}}
	for _, name := range manifest.RenditionNames {
		if path, ok := p.Renditions[name]; ok {
			files = append(files, Photo{ID: p.ID, Path: path})
		}
	}
	return files
}

type Job struct {
	Version          int      `json:"version"`
	JobID            string   `json:"jobId"`
	Profile          string   `json:"profile"`
	CatalogPath      string   `json:"catalogPath"`
	Namespace        string   `json:"namespace"`
	ServiceID        string   `json:"serviceId"`
	GalleryID        string   `json:"galleryId"`
	ExpectedRevision string   `json:"expectedRevision"`
	Title            string   `json:"title"`
	Photos           []Photo  `json:"photos"`
	Removals         []string `json:"removals"`
	Order            []string `json:"order,omitempty"`
}
type PhotoResult struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Key       string `json:"key,omitempty"`
	URL       string `json:"url,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`
}
type Result struct {
	Version     int           `json:"version"`
	JobID       string        `json:"jobId"`
	Status      string        `json:"status"`
	Revision    string        `json:"revision,omitempty"`
	ManifestURL string        `json:"manifestUrl,omitempty"`
	Photos      []PhotoResult `json:"photos"`
	Warnings    []string      `json:"warnings,omitempty"`
	ErrorCode   string        `json:"errorCode,omitempty"`
	Message     string        `json:"message,omitempty"`
}
type Journal struct {
	Version  int               `json:"version"`
	Digest   string            `json:"digest"`
	State    string            `json:"state"`
	Job      Job               `json:"job"`
	Manifest manifest.Manifest `json:"manifest"`
	ETag     string            `json:"etag"`
}

func JobDir(root, id string) string      { return filepath.Join(root, "spool", id) }
func JournalPath(root, id string) string { return filepath.Join(root, "journals", id+".json") }
func Digest(j Job) string {
	b, _ := json.Marshal(j)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Load(path string) (Job, error) {
	var j Job
	f, err := os.Open(path)
	if err != nil {
		return j, errors.New("cannot read job")
	}
	defer f.Close()
	if err = manifest.Decode(f, &j); err != nil {
		return j, err
	}
	return j, nil
}
func Validate(j Job, p config.Profile) error {
	if j.Version != 1 || !manifest.ValidID(j.JobID) || !manifest.ValidID(j.GalleryID) || (j.ExpectedRevision != "" && !manifest.ValidID(j.ExpectedRevision)) || j.Photos == nil || j.Removals == nil {
		return errors.New("invalid job header or protocol version")
	}
	if j.Profile != p.Name || j.Namespace != p.Namespace || j.ServiceID != p.ServiceID || filepath.Clean(j.CatalogPath) != filepath.Clean(p.CatalogPath) {
		return errors.New("job destination or catalog differs from configured profile")
	}
	seen := map[string]bool{}
	for _, photo := range j.Photos {
		if !manifest.ValidID(photo.ID) || seen[photo.ID] || !filepath.IsAbs(photo.Path) {
			return errors.New("invalid photo identity or path")
		}
		if photo.Renditions != nil {
			if len(photo.Renditions) != len(manifest.RenditionNames) {
				return errors.New("incomplete rendition set")
			}
			for name, path := range photo.Renditions {
				if !manifest.ValidRendition(name) || !filepath.IsAbs(path) {
					return errors.New("invalid rendition name or path")
				}
			}
		}
		seen[photo.ID] = true
		b, _ := json.Marshal(photo.Metadata)
		if len(b) > 64<<10 {
			return errors.New("photo metadata exceeds 64 KiB")
		}
		if photo.Metadata.EXIF != nil && strings.ContainsAny(photo.Metadata.EXIF.PreservedFilename, "/\\") {
			return errors.New("preserved filename must not contain a path")
		}
	}
	for _, id := range j.Removals {
		if !manifest.ValidID(id) || seen[id] {
			return errors.New("duplicate or invalid removal")
		}
		seen[id] = true
	}
	return nil
}

// OwnedPath rejects lexical traversal and symlinks, including symlinked parents.
func OwnedPath(root, path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("staging path must be absolute")
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("path outside owned staging directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return errors.New("staging directory missing")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return errors.New("staging file missing")
	}
	if resolved != filepath.Join(resolvedRoot, rel) {
		return errors.New("symlinks in staging are forbidden")
	}
	return nil
}

func SpoolBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(filepath.Join(root, "spool"), func(path string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in spool")
		}
		if !d.IsDir() {
			info, err := d.Info()
			if os.IsNotExist(err) {
				return nil // Another job atomically replaced an advisory file.
			}
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}
