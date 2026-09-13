package jobs

import (
	"path/filepath"

	"github.com/jkeane/publish-to-r2/uploader/internal/config"
)

type Progress struct {
	Version   int    `json:"version"`
	Phase     string `json:"phase"`
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
}

func (e Engine) progress(j Job, phase string, completed, total int) {
	// Progress is advisory. An unavailable UI must not fail a publication,
	// especially once the remote manifest has already committed.
	_ = config.AtomicJSON(filepath.Join(JobDir(e.Root, j.JobID), "progress.json"),
		Progress{Version: 1, Phase: phase, Completed: completed, Total: total})
}
