package credentials

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonkeane/publish-to-r2/uploader/internal/config"
	"github.com/jonkeane/publish-to-r2/uploader/internal/manifest"
)

type Secret struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
}

// Decode never includes input in errors: responses may be displayed or logged.
func Decode(r io.Reader) (Secret, error) {
	var s Secret
	d := json.NewDecoder(io.LimitReader(r, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(&s) != nil {
		return Secret{}, errors.New("invalid credentials")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || strings.TrimSpace(s.AccessKeyID) == "" || strings.TrimSpace(s.SecretAccessKey) == "" {
		return Secret{}, errors.New("enter both R2 access key ID and secret access key")
	}
	return s, nil
}

func Save(root, profile string, secret Secret) error {
	if !manifest.ValidID(profile) {
		return errors.New("invalid profile name")
	}
	if strings.TrimSpace(secret.AccessKeyID) == "" || strings.TrimSpace(secret.SecretAccessKey) == "" {
		return errors.New("enter both R2 access key ID and secret access key")
	}
	return config.AtomicJSON(filepath.Join(root, "credentials", profile+".json"), secret)
}

func Load(root, profile string) (Secret, error) {
	if !manifest.ValidID(profile) {
		return Secret{}, errors.New("invalid profile name")
	}
	f, err := os.Open(filepath.Join(root, "credentials", profile+".json"))
	if err != nil {
		return Secret{}, errors.New("credentials unavailable; enter both keys in the Cloudflare R2 plugin settings and click Test Connection")
	}
	defer f.Close()
	return Decode(f)
}
