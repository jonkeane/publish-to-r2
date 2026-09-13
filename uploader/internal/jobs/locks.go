package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Lock excludes every active job for maintenance (and remains compatible with
// older helpers). Normal jobs share this gate and take narrower locks below.
func Lock(root string) (func(), error) {
	return fileLock(context.Background(), filepath.Join(root, "writer.lock"), syscall.LOCK_EX, false)
}

func activeLock(root string) (func(), error) {
	return fileLock(context.Background(), filepath.Join(root, "writer.lock"), syscall.LOCK_SH, false)
}

// Never remove lock files: another process may already have the inode open.
func fileLock(ctx context.Context, path string, mode int, wait bool) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err = ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, err
		}
		if !wait {
			f.Close()
			return nil, errors.New("another publisher or cleanup is running")
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func resourcePath(root, kind, key string) string {
	hash := sha256.Sum256([]byte(key))
	return filepath.Join(root, "locks", kind+"-"+hex.EncodeToString(hash[:])+".lock")
}

func resourceLock(ctx context.Context, root, kind, key string) (func(), error) {
	return fileLock(ctx, resourcePath(root, kind, key), syscall.LOCK_EX, true)
}

// ConfigLock protects the profile/credential pair during both saves and reads.
func ConfigLock(root string) (func(), error) {
	return resourceLock(context.Background(), root, "config", "")
}

func spoolLock(root string) (func(), error) {
	return resourceLock(context.Background(), root, "spool", "")
}

func (e Engine) destinationKey() string {
	return strings.ToLower(strings.TrimRight(e.Profile.Endpoint, "/")) + "/" + e.Profile.Bucket
}

// Lock ordering: active gate, gallery, job, short ownership/spool locks.
// A job's lock also proves its ownership reservation is still active.
func (e Engine) publicationLocks(ctx context.Context, j Job) (func(), error) {
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	keys := []struct{ kind, key string }{
		{"gallery", e.destinationKey() + "/" + j.GalleryID},
		{"job", j.JobID},
	}
	for _, key := range keys {
		unlock, err := resourceLock(ctx, e.Root, key.kind, key.key)
		if err != nil {
			release()
			return nil, err
		}
		releases = append(releases, unlock)
	}
	return release, nil
}
