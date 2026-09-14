package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jonkeane/publish-to-r2/uploader/internal/config"
	"github.com/jonkeane/publish-to-r2/uploader/internal/credentials"
	"github.com/jonkeane/publish-to-r2/uploader/internal/jobs"
	"github.com/jonkeane/publish-to-r2/uploader/internal/manifest"
	"github.com/jonkeane/publish-to-r2/uploader/internal/storage"
)

const usage = `r2publisher: Lightroom Classic → Cloudflare R2 (macOS)
  configure --profile website           Save Lightroom settings JSON from standard input
  doctor --profile website               Exercise PUT/HEAD/GET/DELETE
  info --profile website                Show nonsecret configuration
  init --profile website --gallery ID --catalog /path/catalog.lrcat --title Title
  stage --profile website --job-id ID --photo ID --file /path/render.jpg [--rendition thumbnail|gallery]
  run --job /absolute/spool/ID/job.json
  reconcile --profile website --gallery ID
  cleanup --profile website --dry-run --report /path/report.json
  cleanup --profile website --apply --report /path/report.json
  spool --job-id ID [--apply]             Inspect/discard local retry files
All commands accept --out /path/response.json for the plugin.
`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "r2publisher:", err)
		os.Exit(1)
	}
}
func execute(ctx context.Context, args []string) (err error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Print(usage)
		return nil
	}
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	profile := fs.String("profile", "website", "settings profile")
	out := fs.String("out", "", "atomic JSON response path")
	jobPath := fs.String("job", "", "job JSON path")
	gallery := fs.String("gallery", "", "gallery ID")
	title := fs.String("title", "", "gallery title")
	catalog := fs.String("catalog", "", "catalog path")
	jobID := fs.String("job-id", "", "job ID")
	photo := fs.String("photo", "", "photo UUID")
	source := fs.String("file", "", "rendered JPEG")
	rendition := fs.String("rendition", "", "additional rendition: thumbnail or gallery")
	apply := fs.Bool("apply", false, "apply reviewed cleanup")
	dry := fs.Bool("dry-run", false, "report only")
	report := fs.String("report", "", "cleanup report path")
	if err = fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	defer func() {
		if err != nil && *out != "" {
			_ = config.AtomicJSON(*out, map[string]any{"version": 1, "status": "failed", "message": err.Error()})
		}
	}()
	root, err := config.Root()
	if err != nil {
		return err
	}
	if command == "configure" {
		p, err := configure(root, *profile, os.Stdin)
		if err != nil {
			return err
		}
		return emit(*out, p)
	}
	if command == "spool" {
		n, err := jobs.DiscardSpool(root, *jobID, *apply)
		if err != nil {
			return err
		}
		return emit(*out, map[string]any{"bytes": n, "discarded": *apply})
	}
	var j jobs.Job
	if command == "run" {
		j, err = jobs.Load(*jobPath)
		if err != nil {
			return err
		}
		if !manifest.ValidID(j.JobID) {
			return errors.New("invalid job ID")
		}
		if err = jobs.OwnedPath(jobs.JobDir(root, j.JobID), *jobPath); err != nil {
			return err
		}
		*profile = j.Profile
	}
	releaseConfig, err := jobs.ConfigLock(root)
	if err != nil {
		return err
	}
	p, err := config.Load(root, *profile)
	if err != nil {
		releaseConfig()
		return err
	}
	var secret credentials.Secret
	if command != "info" && command != "stage" {
		secret, err = credentials.Load(root, p.Name)
	}
	releaseConfig()
	if err != nil {
		return err
	}
	if command == "info" {
		return emit(*out, p)
	}
	engine := jobs.Engine{Root: root, Profile: p}
	if command == "stage" {
		path, err := engine.Stage(*jobID, *photo, *source, *rendition)
		if err != nil {
			return err
		}
		return emit(*out, map[string]any{"path": path})
	}
	engine.Store = storage.New(p, secret)
	switch command {
	case "doctor":
		if err = engine.Doctor(ctx); err != nil {
			return err
		}
		return emit(*out, map[string]any{"version": 1, "status": "ok", "message": "PUT, HEAD, GET and DELETE succeeded"})
	case "init":
		prepared, err := engine.Prepare(ctx, *gallery, *title, *catalog)
		if err != nil {
			return err
		}
		return emit(*out, prepared)
	case "run":
		// The cancel file is written by Lightroom while LrTasks.execute yields.
		jobCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan struct{})
		defer close(done)
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-jobCtx.Done():
					return
				case <-ticker.C:
					if _, err := os.Stat(filepath.Join(jobs.JobDir(root, j.JobID), "cancel")); err == nil {
						cancel()
						return
					}
				}
			}
		}()
		r, runErr := engine.Run(jobCtx, j)
		if err = emit(*out, r); err != nil {
			return err
		}
		return runErr
	case "reconcile":
		results, err := engine.Reconcile(ctx, *gallery)
		if err != nil {
			return err
		}
		return emit(*out, results)
	case "cleanup":
		if *apply && *dry {
			return errors.New("choose dry-run or apply")
		}
		if *report == "" {
			return errors.New("cleanup requires --report")
		}
		if *apply {
			var r jobs.CleanupReport
			f, err := os.Open(*report)
			if err != nil {
				return err
			}
			err = manifest.Decode(f, &r)
			f.Close()
			if err != nil {
				return err
			}
			if err = engine.ApplyCleanup(ctx, r); err != nil {
				return err
			}
			return emit(*out, map[string]any{"status": "applied"})
		}
		unlock, err := jobs.Lock(root)
		if err != nil {
			return err
		}
		defer unlock()
		r, err := engine.PlanCleanup(ctx)
		if err != nil {
			return err
		}
		if err = config.AtomicJSON(*report, r); err != nil {
			return err
		}
		return emit(*out, r)
	default:
		return errors.New("unknown command; use help")
	}
}
func emit(path string, v any) error {
	if path != "" {
		return config.AtomicJSON(path, v)
	}
	return json.NewEncoder(os.Stdout).Encode(v)
}

// Settings are supplied by Lightroom; identities remain owned by the helper.
type pluginSettings struct {
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	PublicBaseURL   string `json:"publicBaseUrl"`
	CatalogPath     string `json:"catalogPath"`
	SpoolLimitBytes int64  `json:"spoolLimitBytes"`
	MaxFileBytes    int64  `json:"maxFileBytes"`
	HistoryKeep     int    `json:"historyKeep"`
	GraceDays       int    `json:"graceDays"`
	credentials.Secret
}

func configure(root, name string, input io.Reader) (config.Profile, error) {
	var settings pluginSettings
	var empty config.Profile
	decoder := json.NewDecoder(io.LimitReader(input, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&settings) != nil {
		return empty, errors.New("invalid Lightroom R2 settings")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return empty, errors.New("invalid Lightroom R2 settings")
	}
	if !manifest.ValidID(name) {
		return empty, errors.New("invalid profile name")
	}
	unlock, err := jobs.ConfigLock(root)
	if err != nil {
		return empty, err
	}
	defer unlock()
	p := config.Profile{Version: 1, Name: name, Namespace: config.NewID(), ServiceID: config.NewID()}
	path := filepath.Join(root, "profiles", name+".json")
	if _, err := os.Stat(path); err == nil {
		p, err = config.Load(root, name)
		if err != nil {
			return empty, err
		}
	} else if !os.IsNotExist(err) {
		return empty, err
	}
	p.Endpoint = settings.Endpoint
	p.Bucket = settings.Bucket
	p.PublicBaseURL = settings.PublicBaseURL
	p.CatalogPath = settings.CatalogPath
	p.SpoolLimitBytes = settings.SpoolLimitBytes
	p.MaxFileBytes = settings.MaxFileBytes
	p.HistoryKeep = settings.HistoryKeep
	p.GraceDays = settings.GraceDays
	if err := p.Validate(); err != nil {
		return empty, err
	}
	if _, err := os.Stat(p.CatalogPath); err != nil {
		return empty, errors.New("Lightroom catalog path must exist")
	}
	if err := credentials.Save(root, name, settings.Secret); err != nil {
		return empty, err
	}
	if err := config.AtomicJSON(path, p); err != nil {
		return empty, err
	}
	return p, nil
}
