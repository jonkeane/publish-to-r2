package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jkeane/publish-to-r2/uploader/internal/config"
	"github.com/jkeane/publish-to-r2/uploader/internal/manifest"
	"github.com/jkeane/publish-to-r2/uploader/internal/storage"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type CleanupReport struct {
	Version       int       `json:"version"`
	Profile       string    `json:"profile"`
	Bucket        string    `json:"bucket"`
	Endpoint      string    `json:"endpoint"`
	CreatedAt     time.Time `json:"createdAt"`
	InventoryHash string    `json:"inventoryHash"`
	History       []string  `json:"history"`
	Objects       []string  `json:"objects"`
	Bytes         int64     `json:"bytes"`
}

// Cleanup fails closed on any unreadable/unknown manifest, even in another gallery.
func (e Engine) PlanCleanup(ctx context.Context) (CleanupReport, error) {
	r := CleanupReport{Version: 1, Profile: e.Profile.Name, Bucket: e.Profile.Bucket, Endpoint: e.Profile.Endpoint, CreatedAt: time.Now().UTC(), History: []string{}, Objects: []string{}}
	objects, err := e.Store.List(ctx, "")
	if err != nil {
		return r, err
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	b, _ := json.Marshal(objects)
	h := sha256.Sum256(b)
	r.InventoryHash = hex.EncodeToString(h[:])
	type stored struct {
		object   storage.Object
		manifest manifest.Manifest
	}
	history := map[string][]stored{}
	current := map[string]manifest.Manifest{}
	retained := []manifest.Manifest{}
	cutoff := time.Now().Add(-time.Duration(e.Profile.GraceDays) * 24 * time.Hour)
	for _, o := range objects {
		if !strings.HasPrefix(o.Key, "galleries/") {
			continue
		}
		data, _, err := e.Store.Get(ctx, o.Key)
		if err != nil {
			return r, err
		}
		var m manifest.Manifest
		if err = manifest.Decode(bytes.NewReader(data), &m); err != nil {
			return r, errors.New("cleanup refused: unreadable gallery manifest")
		}
		if err = m.Validate(); err != nil {
			return r, err
		}
		if o.Key == manifest.CurrentKey(m.GalleryID) {
			current[m.GalleryID] = m
			retained = append(retained, m)
		} else if o.Key == manifest.HistoryKey(m.GalleryID, m.Revision) {
			history[m.GalleryID] = append(history[m.GalleryID], stored{o, m})
		} else {
			return r, errors.New("cleanup refused: unknown gallery object")
		}
	}
	for gallery, versions := range history {
		sort.Slice(versions, func(i, j int) bool { return versions[i].object.Modified.After(versions[j].object.Modified) })
		for i, v := range versions {
			if i < e.Profile.HistoryKeep || v.manifest.Revision == current[gallery].Revision || !v.object.Modified.Before(cutoff) {
				retained = append(retained, v.manifest)
			} else {
				r.History = append(r.History, v.object.Key)
				r.Bytes += v.object.Size
			}
		}
	}
	references := map[string]bool{}
	for _, m := range retained {
		for _, entry := range m.Entries {
			for _, image := range entry.Images() {
				references[image.Key] = true
			}
		}
	}
	for _, o := range objects {
		if !strings.HasPrefix(o.Key, "photos/") {
			continue
		}
		parts := strings.Split(o.Key, "/")
		if len(parts) != 4 || !manifest.ValidID(parts[1]) || !manifest.ValidID(parts[2]) || !strings.HasSuffix(parts[3], ".jpg") || !manifest.ValidHash(strings.TrimSuffix(parts[3], ".jpg")) {
			return r, errors.New("cleanup refused: unrecognized photo object")
		}
		if !references[o.Key] && o.Modified.Before(cutoff) {
			r.Objects = append(r.Objects, o.Key)
			r.Bytes += o.Size
		}
	}
	sort.Strings(r.History)
	sort.Strings(r.Objects)
	return r, nil
}
func (e Engine) ApplyCleanup(ctx context.Context, report CleanupReport) error {
	unlock, err := Lock(e.Root)
	if err != nil {
		return err
	}
	defer unlock()
	if report.Version != 1 || report.Profile != e.Profile.Name || report.Bucket != e.Profile.Bucket || report.Endpoint != e.Profile.Endpoint || time.Since(report.CreatedAt) > 24*time.Hour || report.CreatedAt.After(time.Now()) {
		return errors.New("cleanup report is stale or for another destination")
	}
	fresh, err := e.PlanCleanup(ctx)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(append(append([]string{}, report.History...), report.Objects...))
	b, _ := json.Marshal(append(append([]string{}, fresh.History...), fresh.Objects...))
	if report.InventoryHash != fresh.InventoryHash || !bytes.Equal(a, b) {
		return errors.New("bucket changed since dry run; create and review a new report")
	}
	for _, key := range append(fresh.History, fresh.Objects...) {
		if err = e.Store.Delete(ctx, key); err != nil {
			return err
		}
	}
	return nil
}
func (e Engine) Doctor(ctx context.Context) (err error) {
	key := "diagnostics/" + config.NewID() + ".txt"
	data := []byte("R2Publisher connection test\n")
	h := sha256.Sum256(data)
	hash := hex.EncodeToString(h[:])
	// Attempt cleanup even if PUT timed out after succeeding remotely.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupErr := e.Store.Delete(cleanupCtx, key)
		if cleanupErr != nil {
			err = fmt.Errorf("diagnostic cleanup failed for %s: %w", key, cleanupErr)
		}
	}()
	if err = e.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{Create: true, ContentType: "text/plain", CacheControl: "no-store", Hash: hash}); err != nil {
		return err
	}
	o, err := e.Store.Head(ctx, key)
	if err != nil {
		return err
	}
	if o.Size != int64(len(data)) || o.Hash != hash {
		return errors.New("diagnostic HEAD verification failed")
	}
	b, _, err := e.Store.Get(ctx, key)
	if err != nil {
		return err
	}
	if !bytes.Equal(b, data) {
		return errors.New("diagnostic GET verification failed")
	}
	return nil
}
func DiscardSpool(root, id string, apply bool) (int64, error) {
	if !manifest.ValidID(id) {
		return 0, errors.New("invalid job ID")
	}
	unlock, err := Lock(root)
	if err != nil {
		return 0, err
	}
	defer unlock()
	dir := JobDir(root, id)
	if err = OwnedPath(filepath.Join(root, "spool"), dir); err != nil {
		return 0, err
	}
	var size int64
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in spool")
		}
		if !d.IsDir() {
			i, err := d.Info()
			if err != nil {
				return err
			}
			size += i.Size()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if apply {
		err = os.RemoveAll(dir)
	}
	return size, err
}
