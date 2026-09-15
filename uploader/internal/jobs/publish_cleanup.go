package jobs

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/jonkeane/publish-to-r2/uploader/internal/manifest"
	"github.com/jonkeane/publish-to-r2/uploader/internal/storage"
)

// cleanupPublishedPhotos runs under the gallery lock and photo reservation.
// Stable filenames mean republishing a photo has nothing obsolete to remove;
// only an explicitly removed photo can lose its public objects. Hash-addressed
// legacy objects are deliberately left to grace-period maintenance so a site
// build still using them does not break during the filename migration.
func (e Engine) cleanupPublishedPhotos(ctx context.Context, journal Journal) error {
	if len(journal.Job.Photos) == 0 && len(journal.Job.Removals) == 0 {
		return nil
	}
	ids := map[string]bool{}
	for _, photo := range journal.Job.Photos {
		ids[photo.ID] = true
	}
	for _, id := range journal.Job.Removals {
		ids[id] = true
	}
	var candidates []string
	for id := range ids {
		prefix := "photos/" + journal.Job.Namespace + "/" + id + "/"
		objects, err := e.Store.List(ctx, prefix)
		if err != nil {
			return err
		}
		for _, object := range objects {
			name := strings.TrimSuffix(strings.TrimPrefix(object.Key, prefix), ".jpg")
			if manifest.ValidHash(name) && object.Key == manifest.LegacyKey(journal.Job.Namespace, id, name) {
				continue
			}
			if !manifest.ValidImageName(name) || object.Key != manifest.Key(journal.Job.Namespace, id, name) {
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
	return e.cleanupStagedPhotos(ctx, journal)
}

func (e Engine) cleanupStagedPhotos(ctx context.Context, journal Journal) error {
	keys := map[string]bool{}
	for _, photo := range journal.Job.Photos {
		for _, image := range journal.Manifest.Entries {
			if image.ID != photo.ID {
				continue
			}
			for _, rendition := range image.Images() {
				keys[manifest.StagingKey(journal.Job.Namespace, photo.ID, rendition.SHA256)] = true
			}
		}
	}
	for key := range keys {
		if err := e.Store.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	return nil
}
