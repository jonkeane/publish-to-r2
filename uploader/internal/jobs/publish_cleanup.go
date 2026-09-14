package jobs

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/jonkeane/publish-to-r2/uploader/internal/manifest"
	"github.com/jonkeane/publish-to-r2/uploader/internal/storage"
)

// cleanupPublishedPhotos runs under the gallery lock and photo reservation. History is an
// audit trail, not a reason to retain superseded JPEGs. Retain the full reference
// scan defensively for legacy galleries created before exclusive ownership.
func (e Engine) cleanupPublishedPhotos(ctx context.Context, journal Journal) error {
	if len(journal.Job.Photos) == 0 {
		return nil
	}
	var candidates []string
	for _, photo := range journal.Job.Photos {
		prefix := "photos/" + journal.Job.Namespace + "/" + photo.ID + "/"
		objects, err := e.Store.List(ctx, prefix)
		if err != nil {
			return err
		}
		for _, object := range objects {
			hash := strings.TrimSuffix(strings.TrimPrefix(object.Key, prefix), ".jpg")
			if !manifest.ValidHash(hash) || object.Key != manifest.Key(journal.Job.Namespace, photo.ID, hash) {
				return errors.New("cleanup refused: unrecognized photo object")
			}
			candidates = append(candidates, object.Key)
		}
	}
	// Read every current manifest before deleting anything. Never infer that a
	// reference is absent from an unreadable manifest or a failed inventory.
	current, err := e.currentManifests(ctx)
	if err != nil {
		return err
	}
	references := map[string]bool{}
	foundCurrent := false
	for _, m := range current {
		if m.GalleryID == journal.Job.GalleryID {
			if !sameManifest(m, journal.Manifest) {
				return storage.ErrConflict
			}
			foundCurrent = true
		}
		for _, entry := range m.Entries {
			for _, image := range entry.Images() {
				references[image.Key] = true
			}
		}
	}
	if !foundCurrent {
		return errors.New("cleanup refused: committed current manifest is missing")
	}
	sort.Strings(candidates)
	total := 0
	for _, key := range candidates {
		if !references[key] {
			total++
		}
	}
	completed := 0
	e.progress(journal.Job, "cleanup", completed, total)
	for _, key := range candidates {
		if references[key] {
			continue
		}
		if err := e.Store.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		completed++
		e.progress(journal.Job, "cleanup", completed, total)
	}
	return nil
}
