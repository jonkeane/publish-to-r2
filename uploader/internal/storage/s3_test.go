package storage

import (
	"bytes"
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jkeane/publish-to-r2/uploader/internal/config"
	"github.com/jkeane/publish-to-r2/uploader/internal/credentials"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) Do(r *http.Request) (*http.Response, error) { return f(r) }
func TestS3WireContract(t *testing.T) {
	store := New(config.Profile{Endpoint: "https://account.r2.cloudflarestorage.com", Bucket: "photos"}, credentials.Secret{AccessKeyID: "test-key", SecretAccessKey: "test-secret"})
	clientOptions := store.client.Options()
	clientOptions.HTTPClient = transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "account.r2.cloudflarestorage.com" || r.URL.Path != "/photos/gallery/current.json" {
			t.Errorf("unexpected destination %s", r.URL)
		}
		if !strings.Contains(r.Header.Get("Authorization"), "/auto/s3/aws4_request") {
			t.Error("missing SDK SigV4 auto region")
		}
		if r.Header.Get("If-Match") != "etag" || r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("X-Amz-Meta-Sha256") != "hash" {
			t.Error("conditional/cache/hash headers missing")
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != "jpeg" {
			t.Errorf("unexpected body: %q", b)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Etag": []string{"new"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	store.client = s3.New(clientOptions)
	if err := store.Put(context.Background(), "gallery/current.json", bytes.NewReader([]byte("jpeg")), 4, PutOptions{Match: "etag", Hash: "hash", CacheControl: "no-store", ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}
func TestS3ErrorsAreRedacted(t *testing.T) {
	store := New(config.Profile{Endpoint: "https://account.r2.cloudflarestorage.com", Bucket: "photos"}, credentials.Secret{AccessKeyID: "test-key", SecretAccessKey: "test-secret"})
	o := store.client.Options()
	o.RetryMaxAttempts = 1
	o.HTTPClient = transport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Header: http.Header{"Content-Type": []string{"application/xml"}}, Body: io.NopCloser(strings.NewReader(`<Error><Code>AccessDenied</Code><Message>SECRET must never be exposed</Message></Error>`))}, nil
	})
	store.client = s3.New(o)
	_, err := store.Head(context.Background(), "photo.jpg")
	if !errors.Is(err, ErrPermission) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe permission error: %v", err)
	}
}
