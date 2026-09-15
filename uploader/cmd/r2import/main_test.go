package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture() map[string]any {
	hash := strings.Repeat("a", 64)
	key := "photos/catalog/photo/large.jpg"
	return map[string]any{"schemaVersion": 1, "revision": "rev", "galleryId": "gallery", "namespace": "catalog", "serviceId": "service", "entries": []any{map[string]any{"id": "photo", "key": key, "source": "https://images.example.com/" + key, "sha256": hash, "width": 3000, "height": 2000, "bytes": 1234, "dateTaken": "2026-09-09T12:00:00"}}}
}
func TestValidate(t *testing.T) {
	for _, change := range []func(map[string]any){func(m map[string]any) { m["schemaVersion"] = 2 }, func(m map[string]any) { m["galleryId"] = "other" }, func(m map[string]any) { m["entries"] = nil }, func(m map[string]any) { p := m["entries"].([]any)[0].(map[string]any); p["source"] = "javascript:bad" }, func(m map[string]any) { p := m["entries"].([]any)[0]; m["entries"] = []any{p, p} }} {
		m := fixture()
		change(m)
		b, _ := json.Marshal(m)
		if validate(b, "https://images.example.com", "gallery") == nil {
			t.Fatal("invalid manifest accepted")
		}
	}
	m := fixture()
	b, _ := json.Marshal(m)
	if err := validate(b, "https://images.example.com", "gallery"); err != nil {
		t.Fatal(err)
	}
	m["entries"] = []any{}
	b, _ = json.Marshal(m)
	if err := validate(b, "https://images.example.com", "gallery"); err != nil {
		t.Fatal("empty gallery rejected", err)
	}
}
func TestDiscoveryUsesFrontMatterOnly(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "one"), 0755)
	os.WriteFile(filepath.Join(root, "one", "_index.md"), []byte("---\ntitle: One\nr2_gallery_id: 'gallery'\n---\n"), 0644)
	os.Mkdir(filepath.Join(root, "two"), 0755)
	os.WriteFile(filepath.Join(root, "two", "_index.md"), []byte("---\ntitle: Two\n---\nr2_gallery_id: ignored\n"), 0644)
	g, err := findGalleries(root)
	if err != nil || len(g) != 1 || g[0].id != "gallery" {
		t.Fatalf("bad discovery: %+v %v", g, err)
	}
}
func TestFetchAndWritePreservesMetadata(t *testing.T) {
	m := fixture()
	m["title"] = "日本"
	b, _ := json.Marshal(m)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cache-Control") != "no-cache, no-store" {
			t.Error("missing revalidation header")
		}
		w.Write(b)
	}))
	defer s.Close()
	got, err := fetch(context.Background(), s.Client(), s.URL)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "gallery.json")
	if err = writeAtomic(path, got); err != nil {
		t.Fatal(err)
	}
	actual, _ := os.ReadFile(path)
	if string(actual) != string(b) {
		t.Fatal("metadata changed")
	}
}

func TestValidateRenditions(t *testing.T) {
	m := fixture()
	p := m["entries"].([]any)[0].(map[string]any)
	variant := func(hash string) map[string]any {
		name := "thumbnail"
		if hash == "c" {
			name = "gallery"
		}
		key := "photos/catalog/photo/" + name + ".jpg"
		return map[string]any{"key": key, "source": "https://images.example.com/" + key, "sha256": strings.Repeat(hash, 64), "width": 384, "height": 256, "bytes": 200}
	}
	p["renditions"] = map[string]any{"thumbnail": variant("b"), "gallery": variant("c")}
	data, _ := json.Marshal(m)
	if err := validate(data, "https://images.example.com", "gallery"); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(map[string]any){
		func(r map[string]any) { delete(r, "thumbnail") },
		func(r map[string]any) { r["other"] = r["thumbnail"]; delete(r, "thumbnail") },
		func(r map[string]any) {
			r["thumbnail"].(map[string]any)["source"] = "https://foreign.example/photo.jpg"
		},
		func(r map[string]any) { r["thumbnail"].(map[string]any)["width"] = 0 },
		func(r map[string]any) { r["thumbnail"].(map[string]any)["sha256"] = "not-a-hash" },
	} {
		var invalid map[string]any
		_ = json.Unmarshal(data, &invalid)
		r := invalid["entries"].([]any)[0].(map[string]any)["renditions"].(map[string]any)
		change(r)
		b, _ := json.Marshal(invalid)
		if validate(b, "https://images.example.com", "gallery") == nil {
			t.Fatal("invalid rendition accepted")
		}
	}
}
