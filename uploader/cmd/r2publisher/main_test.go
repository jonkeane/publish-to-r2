package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jkeane/publish-to-r2/uploader/internal/config"
	"github.com/jkeane/publish-to-r2/uploader/internal/credentials"
)

func TestLightroomConfiguration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("R2PUBLISHER_HOME", root)
	catalog := filepath.Join(root, "catalog.lrcat")
	if err := os.WriteFile(catalog, nil, 0600); err != nil {
		t.Fatal(err)
	}
	settings := pluginSettings{Endpoint: "https://example.r2.cloudflarestorage.com", Bucket: "photos",
		PublicBaseURL: "https://images.example.com", CatalogPath: catalog,
		SpoolLimitBytes: 5 << 30, MaxFileBytes: 100 << 20, HistoryKeep: 10, GraceDays: 30,
		Secret: credentials.Secret{AccessKeyID: "test-access-key", SecretAccessKey: "test-secret-key"}}
	out := filepath.Join(root, "response.json")
	call := func(payload []byte) error {
		t.Helper()
		input, err := os.CreateTemp(root, "input")
		if err != nil {
			t.Fatal(err)
		}
		defer input.Close()
		if _, err := input.Write(payload); err != nil {
			t.Fatal(err)
		}
		if _, err := input.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		oldStdin := os.Stdin
		os.Stdin = input
		defer func() { os.Stdin = oldStdin }()
		err = execute(context.Background(), []string{"configure", "--profile", "website", "--out", out})
		response, readErr := os.ReadFile(out)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(response), settings.AccessKeyID) || strings.Contains(string(response), settings.SecretAccessKey) {
			t.Fatal("credentials exposed in helper response")
		}
		return err
	}
	save := func() {
		t.Helper()
		payload, err := json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		if err := call(payload); err != nil {
			t.Fatal(err)
		}
	}
	save() // A clean installation requires no pre-existing profile.
	first, err := config.Load(root, "website")
	if err != nil || first.ServiceID == "" || first.Namespace == "" {
		t.Fatal("missing profile identities", err)
	}
	settings.Bucket = "other-photos"
	settings.PublicBaseURL = "https://other.example.com"
	settings.SpoolLimitBytes = 2 << 30
	settings.MaxFileBytes = 50 << 20
	settings.HistoryKeep = 4
	settings.GraceDays = 7
	save()
	updated, err := config.Load(root, "website")
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServiceID != first.ServiceID || updated.Namespace != first.Namespace {
		t.Fatal("settings update changed gallery or photo identity")
	}
	if updated.Bucket != settings.Bucket || updated.PublicBaseURL != settings.PublicBaseURL ||
		updated.SpoolLimitBytes != settings.SpoolLimitBytes || updated.MaxFileBytes != settings.MaxFileBytes ||
		updated.HistoryKeep != settings.HistoryKeep || updated.GraceDays != settings.GraceDays {
		t.Fatal("settings update not persisted")
	}
	secret, err := credentials.Load(root, "website")
	if err != nil || secret != settings.Secret {
		t.Fatal("credentials not saved", err)
	}
	for _, directory := range []string{"profiles", "credentials"} {
		info, err := os.Stat(filepath.Join(root, directory, "website.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("settings file is not private", err)
		}
	}
	profilePath := filepath.Join(root, "profiles", "website.json")
	before, _ := os.ReadFile(profilePath)
	good, _ := json.Marshal(settings)
	invalid := []string{
		strings.Replace(string(good), `"historyKeep":4`, `"historyKeep":0`, 1),
		strings.Replace(string(good), `"maxFileBytes":52428800`, `"maxFileBytes":9000000000`, 1),
		strings.Replace(string(good), `"secretAccessKey":"test-secret-key"`, `"secretAccessKey":""`, 1),
		strings.Replace(string(good), `https://other.example.com`, `http://other.example.com`, 1),
		strings.Replace(string(good), `https://example.r2.cloudflarestorage.com`, `https://example.com`, 1),
		strings.Replace(string(good), catalog, "/missing/catalog.lrcat", 1),
		string(good) + `{}`,
		`{"secretAccessKey":"test-secret-key","namespace":"injected"}`,
		`{"secretAccessKey":"test-secret-key","historyKeep":"test-secret-key"}`,
	}
	for _, payload := range invalid {
		if err := call([]byte(payload)); err == nil {
			t.Fatal("invalid settings accepted")
		}
		after, _ := os.ReadFile(profilePath)
		if string(before) != string(after) {
			t.Fatal("invalid settings changed saved profile")
		}
		afterSecret, err := credentials.Load(root, "website")
		if err != nil || afterSecret != secret {
			t.Fatal("invalid settings changed credentials")
		}
	}
	if err := execute(context.Background(), []string{"info", "--profile", "website", "--out", out}); err != nil {
		t.Fatal(err)
	}
	response, _ := os.ReadFile(out)
	if strings.Contains(string(response), settings.SecretAccessKey) || strings.Contains(string(response), settings.AccessKeyID) {
		t.Fatal("public info exposed credentials")
	}
}
