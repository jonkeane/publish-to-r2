package jobs

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/jkeane/publish-to-r2/uploader/internal/manifest"
)

const uploadWorkers = 4

type uploadWork struct {
	photo Photo
	image manifest.Image
}

func (e Engine) uploadAll(ctx context.Context, j Job, uploads []uploadWork) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan uploadWork)
	results := make(chan error, uploadWorkers)
	var workers sync.WaitGroup
	e.progress(j, "uploading", 0, len(uploads))
	for range min(uploadWorkers, len(uploads)) {
		workers.Go(func() {
			for item := range work {
				if ctx.Err() != nil {
					return
				}
				if _, err := os.Stat(filepath.Join(JobDir(e.Root, j.JobID), "cancel")); err == nil {
					results <- context.Canceled
					return
				}
				err := e.upload(ctx, j, item.photo, item.image)
				results <- err
				if err != nil {
					return
				}
			}
		})
	}
	go func() {
		defer close(work)
		for _, item := range uploads {
			select {
			case work <- item:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { workers.Wait(); close(results) }()
	var firstErr error
	completed := 0
	// Only this coordinator writes progress. Drain all workers before returning:
	// neither commit nor a retry may race an upload left running after failure.
	for err := range results {
		if err != nil {
			if firstErr == nil {
				firstErr = err
				cancel()
			}
			continue
		}
		completed++
		e.progress(j, "uploading", completed, len(uploads))
	}
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
