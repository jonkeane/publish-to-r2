// r2import downloads public R2 gallery manifests for Hugo's content adapter.
// Run from the site root, before hugo. It never downloads JPEGs or needs S3 keys.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxManifestBytes = 32 << 20

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var validHash = regexp.MustCompile(`^[a-f0-9]{64}$`)
var galleryField = regexp.MustCompile(`(?m)^r2_gallery_id:[ \t]*(?:"([^"]+)"|'([^']+)'|([A-Za-z0-9_-]+))[ \t]*(?:#.*)?$`)

type gallery struct{ slug, id string }
type rendition struct {
	Key    string `json:"key"`
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bytes  int64  `json:"bytes"`
}
type manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Revision      string `json:"revision"`
	GalleryID     string `json:"galleryId"`
	Namespace     string `json:"namespace"`
	ServiceID     string `json:"serviceId"`
	Title         string `json:"title"`
	Entries       []struct {
		Renditions map[string]rendition `json:"renditions"`
		ID         string               `json:"id"`
		Key        string               `json:"key"`
		Source     string               `json:"source"`
		SHA256     string               `json:"sha256"`
		Width      int                  `json:"width"`
		Height     int                  `json:"height"`
		Bytes      int64                `json:"bytes"`
		DateTaken  string               `json:"dateTaken"`
	} `json:"entries"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "r2import:", err)
		os.Exit(1)
	}
}
func run() error {
	base := flag.String("base-url", os.Getenv("R2_PUBLIC_BASE_URL"), "public R2 custom domain")
	slug := flag.String("slug", "", "single gallery slug (with -gallery)")
	id := flag.String("gallery", "", "single gallery ID")
	file := flag.String("file", "", "import a local manifest instead of downloading (requires -slug and -gallery)")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	var galleries []gallery
	if *slug != "" || *id != "" {
		if !validID.MatchString(*slug) || !validID.MatchString(*id) {
			return errors.New("valid -slug and -gallery must be supplied together")
		}
		galleries = []gallery{{*slug, *id}}
	} else {
		var err error
		galleries, err = findGalleries("content/gallery")
		if err != nil {
			return err
		}
	}
	if len(galleries) == 0 {
		fmt.Println("No R2 galleries configured.")
		return pruneSnapshots("data/r2/galleries", galleries)
	}
	if *file != "" && (*slug == "" || *id == "") {
		return errors.New("-file requires -slug and -gallery")
	}
	if err := validateBase(*base); err != nil {
		return err
	}
	*base = strings.TrimRight(*base, "/")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return errors.New("R2 manifest redirects are not accepted")
	}}
	for _, g := range galleries {
		var data []byte
		var err error
		if *file != "" {
			f, openErr := os.Open(*file)
			if openErr != nil {
				return openErr
			}
			data, err = io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
			f.Close()
		} else {
			data, err = fetch(context.Background(), client, *base+"/galleries/"+g.id+"/current.json")
		}
		if err != nil {
			return fmt.Errorf("gallery %s: %w", g.slug, err)
		}
		if err = validate(data, *base, g.id); err != nil {
			return fmt.Errorf("gallery %s: %w", g.slug, err)
		}
		if err = writeAtomic(filepath.Join("data", "r2", "galleries", g.slug+".json"), data); err != nil {
			return err
		}
		fmt.Printf("Imported R2 gallery %s\n", g.slug)
	}
	if *slug == "" {
		return pruneSnapshots("data/r2/galleries", galleries)
	}
	return nil
}

// Only generated JSON snapshots are pruned; no content pages or remote images.
func pruneSnapshots(dir string, galleries []gallery) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, g := range galleries {
		keep[g.slug+".json"] = true
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || keep[entry.Name()] {
			continue
		}
		if !validID.MatchString(strings.TrimSuffix(entry.Name(), ".json")) {
			return errors.New("unexpected file in generated R2 data directory")
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
func validateBase(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("R2_PUBLIC_BASE_URL must be a public HTTPS base URL")
	}
	return nil
}
func findGalleries(root string) ([]gallery, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := []gallery{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), "_index.md")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		text := strings.ReplaceAll(string(data), "\r\n", "\n")
		if !strings.HasPrefix(text, "---\n") {
			continue
		}
		end := strings.Index(text[4:], "\n---")
		if end < 0 {
			return nil, fmt.Errorf("unclosed YAML front matter: %s", path)
		}
		front := text[4 : 4+end]
		if !strings.Contains(front, "r2_gallery_id:") {
			continue
		}
		match := galleryField.FindStringSubmatch(front)
		if match == nil {
			return nil, fmt.Errorf("invalid r2_gallery_id in %s", path)
		}
		id := match[1] + match[2] + match[3]
		if !validID.MatchString(id) || !validID.MatchString(e.Name()) {
			return nil, errors.New("invalid R2 gallery ID or slug")
		}
		out = append(out, gallery{e.Name(), id})
	}
	return out, nil
}
func fetch(ctx context.Context, client *http.Client, address string) ([]byte, error) {
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<(attempt-1)) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Cache-Control", "no-cache, no-store")
		res, err := client.Do(req)
		if err != nil {
			if attempt < 3 {
				continue
			}
			return nil, errors.New("manifest request failed")
		}
		if res.StatusCode == 429 || res.StatusCode >= 500 {
			res.Body.Close()
			if attempt < 3 {
				continue
			}
			return nil, fmt.Errorf("manifest HTTP %d", res.StatusCode)
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return nil, fmt.Errorf("manifest HTTP %d; existing local data was preserved", res.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, maxManifestBytes+1))
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	return nil, errors.New("manifest retries exhausted")
}
func validate(data []byte, base, id string) error {
	if len(data) > maxManifestBytes {
		return errors.New("manifest exceeds 32 MiB")
	}
	var m manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&m); err != nil {
		return errors.New("invalid manifest JSON")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return errors.New("trailing manifest data")
	}
	if m.SchemaVersion != 1 || m.GalleryID != id || !validID.MatchString(m.Revision) || !validID.MatchString(m.Namespace) || !validID.MatchString(m.ServiceID) || m.Entries == nil {
		return errors.New("unsupported manifest or wrong gallery")
	}
	seen := map[string]bool{}
	for _, p := range m.Entries {
		key := imageKey(m.Namespace, p.ID, "large")
		if !validID.MatchString(p.ID) || seen[p.ID] || !validHash.MatchString(p.SHA256) || p.Key != key || p.Source != strings.TrimRight(base, "/")+"/"+key || p.Width < 1 || p.Height < 1 || p.Bytes < 1 {
			return errors.New("invalid photograph or foreign image URL")
		}
		if p.Renditions != nil {
			if len(p.Renditions) != 2 {
				return errors.New("incomplete rendition set")
			}
			for name, image := range p.Renditions {
				key := imageKey(m.Namespace, p.ID, name)
				if (name != "thumbnail" && name != "gallery") || !validHash.MatchString(image.SHA256) || image.Key != key || image.Source != strings.TrimRight(base, "/")+"/"+key || image.Width < 1 || image.Height < 1 || image.Bytes < 1 {
					return errors.New("invalid rendition or foreign image URL")
				}
			}
		}
		seen[p.ID] = true
		if p.DateTaken != "" {
			if _, err := time.Parse(time.RFC3339, p.DateTaken); err != nil {
				if _, err = time.Parse("2006-01-02T15:04:05", p.DateTaken); err != nil {
					return errors.New("invalid capture date")
				}
			}
		}
	}
	return nil
}

func imageKey(namespace, id, name string) string {
	return "photos/" + namespace + "/" + id + "/" + name + ".jpg"
}
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".manifest-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
