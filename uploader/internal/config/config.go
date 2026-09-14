package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonkeane/publish-to-r2/uploader/internal/manifest"
)

type Profile struct {
	Version         int    `json:"version"`
	Name            string `json:"name"`
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	PublicBaseURL   string `json:"publicBaseUrl"`
	CatalogPath     string `json:"catalogPath"`
	Namespace       string `json:"namespace"`
	ServiceID       string `json:"serviceId"`
	SpoolLimitBytes int64  `json:"spoolLimitBytes"`
	MaxFileBytes    int64  `json:"maxFileBytes"`
	HistoryKeep     int    `json:"historyKeep"`
	GraceDays       int    `json:"graceDays"`
}

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func Root() (string, error) {
	if root := os.Getenv("R2PUBLISHER_HOME"); root != "" {
		if !filepath.IsAbs(root) {
			return "", errors.New("R2PUBLISHER_HOME must be absolute")
		}
		return root, nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "R2Publisher"), err
}
func (p Profile) Validate() error {
	endpoint, err := url.Parse(p.Endpoint)
	if err != nil || endpoint.Scheme != "https" || !strings.HasSuffix(endpoint.Hostname(), ".r2.cloudflarestorage.com") || endpoint.User != nil || endpoint.Port() != "" || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("endpoint must be the exact HTTPS R2 S3 endpoint")
	}
	base, err := url.Parse(p.PublicBaseURL)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return errors.New("public base URL must be HTTPS without credentials or query")
	}
	if p.Version != 1 || !manifest.ValidID(p.Name) || !manifest.ValidID(p.Namespace) || !manifest.ValidID(p.ServiceID) || len(p.Bucket) < 3 || strings.ContainsAny(p.Bucket, "/\\ :") || !filepath.IsAbs(p.CatalogPath) || p.SpoolLimitBytes < 1 || p.MaxFileBytes < 1 || p.MaxFileBytes > p.SpoolLimitBytes || p.HistoryKeep < 1 || p.GraceDays < 1 {
		return errors.New("invalid profile settings")
	}
	return nil
}
func Load(root, name string) (Profile, error) {
	var p Profile
	if !manifest.ValidID(name) {
		return p, errors.New("invalid profile name")
	}
	f, err := os.Open(filepath.Join(root, "profiles", name+".json"))
	if err != nil {
		return p, errors.New("profile not found; complete the Cloudflare R2 settings in Lightroom")
	}
	defer f.Close()
	if err = manifest.Decode(f, &p); err != nil {
		return p, err
	}
	if p.Name != name {
		return p, errors.New("profile name mismatch")
	}
	return p, p.Validate()
}
func AtomicJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
