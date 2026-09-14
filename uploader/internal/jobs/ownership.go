package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jonkeane/publish-to-r2/uploader/internal/config"

	"github.com/jonkeane/publish-to-r2/uploader/internal/manifest"
)

type photoReservation struct {
	Destination string   `json:"destination"`
	Namespace   string   `json:"namespace"`
	Gallery     string   `json:"gallery"`
	Photos      []string `json:"photos"`
}

// Registration is atomic, but remote reads and uploads run outside this short
// lock. Job locks distinguish live reservations from files left after a crash.
// This uses a constant number of open files even for very large galleries.
// Reserve uploads only: removals must allow legacy shared photos to leave a gallery.
func (e Engine) reservePhotos(ctx context.Context, j Job) (func(), error) {
	noop := func() {}
	if len(j.Photos) == 0 {
		return noop, nil
	}
	unlock, err := resourceLock(ctx, e.Root, "ownership", "")
	if err != nil {
		return nil, err
	}
	defer unlock()
	directory := filepath.Join(e.Root, "claims")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	photos := map[string]bool{}
	claim := photoReservation{Destination: e.destinationKey(), Namespace: j.Namespace, Gallery: j.GalleryID}
	for _, photo := range j.Photos {
		photos[photo.ID] = true
		claim.Photos = append(claim.Photos, photo.ID)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), ".write-") {
			continue
		}
		id := strings.TrimSuffix(file.Name(), ".json")
		if !strings.HasSuffix(file.Name(), ".json") || !manifest.ValidID(id) || file.IsDir() {
			return nil, errors.New("unrecognized photo reservation")
		}
		if id == j.JobID {
			continue
		}
		path := filepath.Join(directory, file.Name())
		// Never wait for another job here: it may need the ownership lock too.
		releaseJob, probeErr := fileLock(ctx, resourcePath(e.Root, "job", id), syscall.LOCK_EX, false)
		if probeErr == nil {
			err = os.Remove(path)
			releaseJob()
			if err != nil {
				return nil, err
			}
			continue
		}
		// A failed probe is conservatively treated as a live reservation.
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		var other photoReservation
		err = manifest.Decode(f, &other)
		f.Close()
		if err != nil || other.Destination == "" || other.Namespace == "" || other.Gallery == "" || other.Photos == nil {
			return nil, errors.New("unreadable photo reservation")
		}
		if other.Destination != claim.Destination || other.Namespace != j.Namespace {
			continue
		}
		for _, id := range other.Photos {
			if photos[id] {
				return nil, fmt.Errorf("photo %s is already being published to gallery %s; wait for that job and remove it from that gallery before publishing it here", id, other.Gallery)
			}
		}
	}
	path := filepath.Join(directory, j.JobID+".json")
	if err := config.AtomicJSON(path, claim); err != nil {
		return nil, err
	}
	return func() {
		unlock, err := resourceLock(context.Background(), e.Root, "ownership", "")
		if err == nil {
			defer unlock()
			_ = os.Remove(path) // A leftover claim is reclaimed after the job unlocks.
		}
	}, nil
}

// Photos are reserved before this scan until publication and cleanup finish.
// Existing galleries are checked as well as simultaneous first publications;
// no ownership cache can silently omit galleries created by older versions.
// Removals do not acquire ownership and must not be checked against other galleries.
func (e Engine) checkPhotoOwnership(ctx context.Context, j Job) error {
	if len(j.Photos) == 0 {
		return nil
	}
	photos := map[string]bool{}
	for _, photo := range j.Photos {
		photos[photo.ID] = true
	}
	current, err := e.currentManifests(ctx)
	if err != nil {
		return err
	}
	for _, m := range current {
		if m.GalleryID == j.GalleryID || m.Namespace != j.Namespace {
			continue
		}
		for _, entry := range m.Entries {
			if photos[entry.ID] {
				return fmt.Errorf("photo %s already belongs to gallery %q (%s); remove it from that gallery before publishing it here", entry.ID, m.Title, m.GalleryID)
			}
		}
	}
	return nil
}

func (e Engine) currentManifests(ctx context.Context) ([]manifest.Manifest, error) {
	objects, err := e.Store.List(ctx, "galleries/")
	if err != nil {
		return nil, err
	}
	var current []manifest.Manifest
	for _, object := range objects {
		parts := strings.Split(object.Key, "/")
		if len(parts) == 4 && manifest.ValidID(parts[1]) && parts[2] == "history" &&
			strings.HasSuffix(parts[3], ".json") && manifest.ValidID(strings.TrimSuffix(parts[3], ".json")) {
			continue
		}
		if len(parts) != 3 || !manifest.ValidID(parts[1]) || parts[2] != "current.json" {
			return nil, errors.New("publication refused: unknown gallery object")
		}
		data, _, err := e.Store.Get(ctx, object.Key)
		if err != nil {
			return nil, err
		}
		var m manifest.Manifest
		if manifest.Decode(bytes.NewReader(data), &m) != nil || m.Validate() != nil || object.Key != manifest.CurrentKey(m.GalleryID) {
			return nil, errors.New("publication refused: unreadable or inconsistent current manifest")
		}
		current = append(current, m)
	}
	return current, nil
}
