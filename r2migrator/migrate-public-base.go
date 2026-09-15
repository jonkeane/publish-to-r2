// r2manifest-migrate promotes hash-addressed R2 JPEGs to stable public image
// paths and rewrites existing gallery manifests. It uses server-side copies;
// JPEGs are neither downloaded nor re-rendered.
//
// Usage:
//
//	go run . --profile website --all
//	go run . --profile website --gallery GALLERY_ID --apply
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const maxManifestBytes = 32 << 20

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var validHash = regexp.MustCompile(`^[a-f0-9]{64}$`)
var safeRemoteErrorCode = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	if !validID.MatchString(value) {
		return fmt.Errorf("invalid gallery ID %q", value)
	}
	*s = append(*s, value)
	return nil
}

type profile struct {
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

type credentials struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
}

type image struct {
	Key    string `json:"key"`
	Source string `json:"source"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type entry struct {
	Renditions     map[string]image `json:"renditions,omitempty"`
	ID             string           `json:"id"`
	Key            string           `json:"key"`
	Source         string           `json:"source"`
	Width          int              `json:"width"`
	Height         int              `json:"height"`
	Bytes          int64            `json:"bytes"`
	SHA256         string           `json:"sha256"`
	DateUpload     string           `json:"dateUpload"`
	LastUpdate     string           `json:"lastUpdate"`
	OriginalFormat string           `json:"originalFormat"`
	Title          string           `json:"title,omitempty"`
	Caption        string           `json:"caption,omitempty"`
	Alt            string           `json:"alt,omitempty"`
	Copyright      string           `json:"copyright,omitempty"`
	Tags           []string         `json:"tags,omitempty"`
	DateTaken      string           `json:"dateTaken,omitempty"`
	OwnerName      string           `json:"ownerName,omitempty"`
	License        string           `json:"license,omitempty"`
	EXIF           json.RawMessage  `json:"exif,omitempty"`
}

type manifest struct {
	SchemaVersion  int       `json:"schemaVersion"`
	Revision       string    `json:"revision"`
	ParentRevision string    `json:"parentRevision"`
	GalleryID      string    `json:"galleryId"`
	Namespace      string    `json:"namespace"`
	ServiceID      string    `json:"serviceId"`
	Title          string    `json:"title"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Entries        []entry   `json:"entries"`
}

type objectStore interface {
	get(context.Context, string) ([]byte, string, error)
	put(context.Context, string, []byte, string, bool) error
	copy(context.Context, string, string, string) error
	delete(context.Context, string) error
	listCurrent(context.Context) ([]string, error)
}

type r2Store struct {
	client *s3.Client
	bucket string
}

func (s r2Store) get(ctx context.Context, key string) ([]byte, string, error) {
	request, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, err := s.client.GetObject(request, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, "", safeRemoteError(err)
	}
	defer result.Body.Close()
	data, err := io.ReadAll(io.LimitReader(result.Body, maxManifestBytes+1))
	if err != nil {
		return nil, "", errors.New("could not read manifest")
	}
	if len(data) > maxManifestBytes {
		return nil, "", errors.New("manifest exceeds 32 MiB")
	}
	if aws.ToString(result.ETag) == "" {
		return nil, "", errors.New("manifest has no ETag")
	}
	return data, aws.ToString(result.ETag), nil
}

func (s r2Store) put(ctx context.Context, key string, data []byte, condition string, create bool) error {
	request, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	size := int64(len(data))
	in := &s3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           &key,
		Body:          bytes.NewReader(data),
		ContentLength: &size,
		ContentType:   aws.String("application/json"),
		CacheControl:  aws.String("no-store, max-age=0"),
	}
	if create {
		in.IfNoneMatch = aws.String("*")
	} else {
		in.IfMatch = &condition
	}
	if _, err := s.client.PutObject(request, in); err != nil {
		return safeRemoteError(err)
	}
	return nil
}

func (s r2Store) copy(ctx context.Context, source, destination, hash string) error {
	request, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	copySource := url.PathEscape(s.bucket + "/" + source)
	contentType, cacheControl := "image/jpeg", "public, max-age=86400, s-maxage=2592000"
	_, err := s.client.CopyObject(request, &s3.CopyObjectInput{
		Bucket: &s.bucket, Key: &destination, CopySource: &copySource,
		MetadataDirective: types.MetadataDirectiveReplace,
		ContentType:       &contentType, CacheControl: &cacheControl,
		Metadata: map[string]string{"sha256": hash},
	})
	return safeRemoteError(err)
}

func (s r2Store) delete(ctx context.Context, key string) error {
	request, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err := s.client.DeleteObject(request, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	return safeRemoteError(err)
}

func (s r2Store) listCurrent(ctx context.Context) ([]string, error) {
	pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: aws.String("galleries/")})
	set := map[string]bool{}
	for pager.HasMorePages() {
		request, cancel := context.WithTimeout(ctx, 2*time.Minute)
		page, err := pager.NextPage(request)
		cancel()
		if err != nil {
			return nil, safeRemoteError(err)
		}
		for _, object := range page.Contents {
			parts := strings.Split(aws.ToString(object.Key), "/")
			if len(parts) == 3 && parts[0] == "galleries" && parts[2] == "current.json" && validID.MatchString(parts[1]) {
				set[parts[1]] = true
			}
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func safeRemoteError(err error) error {
	if err == nil {
		return nil
	}
	// SDK errors can contain endpoint and request details. Credentials must never
	// be included in command output. Expose only an allowlist of API codes whose
	// meaning is actionable and does not reveal response content.
	if errors.Is(err, context.Canceled) {
		return errors.New("R2 request cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("R2 request timed out")
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return errors.New("R2 object not found")
		case "AccessDenied":
			return errors.New("R2 access denied; the token needs Object Read & Write access to this bucket")
		case "InvalidAccessKeyId", "ExpiredToken":
			return errors.New("R2 credentials were rejected; update the saved access key and secret")
		case "SignatureDoesNotMatch":
			return errors.New("R2 request signature was rejected; update the saved access key and secret")
		case "SlowDown", "ServiceUnavailable", "InternalError":
			return errors.New("R2 is temporarily unavailable; retry the migration")
		default:
			// The S3 error code is a bounded protocol identifier, rather than the
			// potentially sensitive response message or request details.
			if safeRemoteErrorCode.MatchString(api.ErrorCode()) {
				return fmt.Errorf("R2 rejected the request with S3 error code %q", api.ErrorCode())
			}
		}
	}
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) {
		switch response.HTTPStatusCode() {
		case http.StatusBadRequest:
			return errors.New("R2 rejected the request; check the profile and object key")
		case http.StatusUnauthorized, http.StatusForbidden:
			return errors.New("R2 access denied; the token needs Object Read & Write access to this bucket")
		case http.StatusNotFound:
			return errors.New("R2 object not found")
		case http.StatusTooManyRequests:
			return errors.New("R2 is rate limiting requests; retry the migration")
		default:
			if response.HTTPStatusCode() >= 500 && response.HTTPStatusCode() <= 599 {
				return errors.New("R2 is temporarily unavailable; retry the migration")
			}
			return errors.New("R2 returned an unexpected HTTP response")
		}
	}
	var deserialize *smithy.DeserializationError
	if errors.As(err, &deserialize) {
		return errors.New("R2 returned an invalid response; check whether the destination object was copied before retrying")
	}
	return fmt.Errorf("R2 request failed (%s); check the profile, credentials, and connection", errorTypeChain(err))
}

func errorTypeChain(err error) string {
	const maxTypes = 4
	types := make([]string, 0, maxTypes)
	for err != nil && len(types) < maxTypes {
		types = append(types, fmt.Sprintf("%T", err))
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = unwrapper.Unwrap()
	}
	return strings.Join(types, " -> ")
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "r2manifest-migrate:", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("r2manifest-migrate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profileName := flags.String("profile", "website", "R2 Publisher settings profile")
	baseURL := flags.String("base-url", "", "public HTTPS base URL (defaults to the profile setting)")
	all := flags.Bool("all", false, "migrate every current gallery manifest in the bucket")
	apply := flags.Bool("apply", false, "write manifests; without this flag, only report changes")
	var galleries stringList
	flags.Var(&galleries, "gallery", "gallery ID to migrate (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if !validID.MatchString(*profileName) {
		return errors.New("invalid profile name")
	}
	if *all == (len(galleries) > 0) {
		return errors.New("supply exactly one of --all or --gallery")
	}

	root, err := publisherRoot()
	if err != nil {
		return err
	}
	p, secret, err := loadProfile(root, *profileName)
	if err != nil {
		return err
	}
	if *baseURL == "" {
		*baseURL = p.PublicBaseURL
	}
	base, err := normalizeBaseURL(*baseURL)
	if err != nil {
		return err
	}
	store := newStore(p, secret)
	if *all {
		galleries, err = store.listCurrent(context.Background())
		if err != nil {
			return err
		}
	}
	if len(galleries) == 0 {
		return errors.New("no current gallery manifests found")
	}

	failed := 0
	for _, gallery := range galleries {
		result, err := migrate(context.Background(), store, gallery, base, *apply)
		if err != nil {
			failed++
			fmt.Fprintf(output, "%s: failed: %v\n", gallery, err)
			continue
		}
		if result.changed {
			action := "would migrate"
			if *apply {
				action = "migrated"
			}
			fmt.Fprintf(output, "%s: %s %d image records (%s -> %s)", gallery, action, result.images, result.oldRevision, result.newRevision)
		} else {
			fmt.Fprintf(output, "%s: already uses stable image paths at %s", gallery, base)
		}
		if *apply {
			fmt.Fprintf(output, "; removed %d legacy JPEGs\n", result.deleted)
		} else {
			fmt.Fprintln(output)
		}
	}
	if failed != 0 {
		return fmt.Errorf("%d gallery migration(s) failed", failed)
	}
	return nil
}

type migrationResult struct {
	changed     bool
	images      int
	deleted     int
	oldRevision string
	newRevision string
}

func migrate(ctx context.Context, store objectStore, gallery, base string, apply bool) (migrationResult, error) {
	var result migrationResult
	key := "galleries/" + gallery + "/current.json"
	data, etag, err := store.get(ctx, key)
	if err != nil {
		return result, err
	}
	m, err := decodeManifest(data)
	if err != nil {
		return result, err
	}
	if m.GalleryID != gallery {
		return result, errors.New("manifest gallery ID does not match its object key")
	}
	if err = validateManifest(m); err != nil {
		return result, err
	}
	result.oldRevision = m.Revision
	type promotion struct{ source, destination, hash string }
	promotions := []promotion{}
	legacy := map[string]struct{}{}
	addLegacy := func(id, hash string) {
		legacy[legacyImageKey(m.Namespace, id, hash)] = struct{}{}
	}
	for index := range m.Entries {
		entry := &m.Entries[index]
		addLegacy(entry.ID, entry.SHA256)
		var changed bool
		oldKey := entry.Key
		entry.Key, entry.Source, changed = stableImage(entry.Key, entry.Source, base, m.Namespace, entry.ID, "large")
		if changed {
			result.changed = true
			result.images++
		}
		if oldKey != entry.Key {
			// The old key was replaced above. The source copy is recorded before
			// mutation so an apply never downloads an image.
			promotions = append(promotions, promotion{oldKey, entry.Key, entry.SHA256})
		}
		for _, name := range []string{"thumbnail", "gallery"} {
			image, exists := entry.Renditions[name]
			if !exists {
				continue
			}
			addLegacy(entry.ID, image.SHA256)
			oldKey := image.Key
			image.Key, image.Source, changed = stableImage(image.Key, image.Source, base, m.Namespace, entry.ID, name)
			if changed {
				entry.Renditions[name] = image
				result.changed = true
				result.images++
			}
			if oldKey != image.Key {
				promotions = append(promotions, promotion{oldKey, image.Key, image.SHA256})
			}
		}
	}
	if !result.changed && !apply {
		return result, nil
	}
	if result.changed {
		newRevision, err := newID()
		if err != nil {
			return result, err
		}
		m.ParentRevision = m.Revision
		m.Revision = newRevision
		m.UpdatedAt = time.Now().UTC()
		if err = validateManifest(m); err != nil {
			return result, fmt.Errorf("rewritten manifest is invalid: %w", err)
		}
		result.newRevision = newRevision
		if !apply {
			return result, nil
		}
		for _, promotion := range promotions {
			if err = store.copy(ctx, promotion.source, promotion.destination, promotion.hash); err != nil {
				return result, fmt.Errorf("could not promote %s: %w", promotion.destination, err)
			}
		}
		encoded, err := json.Marshal(m)
		if err != nil {
			return result, errors.New("could not encode rewritten manifest")
		}
		historyKey := "galleries/" + gallery + "/history/" + newRevision + ".json"
		if err = store.put(ctx, historyKey, encoded, "", true); err != nil {
			return result, fmt.Errorf("could not create history revision: %w", err)
		}
		if err = store.put(ctx, key, encoded, etag, false); err != nil {
			return result, fmt.Errorf("history revision was created but current manifest was not updated: %w", err)
		}
	}
	keys := make([]string, 0, len(legacy))
	for legacyKey := range legacy {
		keys = append(keys, legacyKey)
	}
	sort.Strings(keys)
	for _, legacyKey := range keys {
		if err = store.delete(ctx, legacyKey); err != nil {
			return result, fmt.Errorf("could not delete legacy JPEG %s after updating the manifest: %w", legacyKey, err)
		}
		result.deleted++
	}
	return result, nil
}

func stableImage(key, source, base, namespace, id, name string) (string, string, bool) {
	target := stableImageKey(namespace, id, name)
	targetURL := imageURL(base, target)
	return target, targetURL, key != target || source != targetURL
}

func decodeManifest(data []byte) (manifest, error) {
	var m manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, errors.New("invalid manifest JSON")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return m, errors.New("trailing manifest data")
	}
	return m, nil
}

func validateManifest(m manifest) error {
	if m.SchemaVersion != 1 || !validID.MatchString(m.Revision) || !validID.MatchString(m.GalleryID) || !validID.MatchString(m.Namespace) || !validID.MatchString(m.ServiceID) || (m.ParentRevision != "" && !validID.MatchString(m.ParentRevision)) || m.UpdatedAt.IsZero() || m.Entries == nil {
		return errors.New("invalid manifest header")
	}
	seen := map[string]bool{}
	for _, entry := range m.Entries {
		if !validID.MatchString(entry.ID) || seen[entry.ID] || errImage(m.Namespace, entry.ID, "large", image{Key: entry.Key, Source: entry.Source, Width: entry.Width, Height: entry.Height, Bytes: entry.Bytes, SHA256: entry.SHA256}) != nil {
			return errors.New("invalid or duplicate manifest entry")
		}
		if entry.Renditions != nil {
			if len(entry.Renditions) != 2 {
				return errors.New("incomplete rendition set")
			}
			base := strings.TrimSuffix(entry.Source, "/"+entry.Key)
			for name, rendition := range entry.Renditions {
				if (name != "thumbnail" && name != "gallery") || errImage(m.Namespace, entry.ID, name, rendition) != nil || rendition.Source != imageURL(base, rendition.Key) {
					return errors.New("invalid rendition or foreign image URL")
				}
			}
		}
		seen[entry.ID] = true
	}
	return nil
}

func errImage(namespace, id, name string, image image) error {
	u, err := url.Parse(image.Source)
	if !validHash.MatchString(image.SHA256) || (image.Key != legacyImageKey(namespace, id, image.SHA256) && image.Key != stableImageKey(namespace, id, name)) || image.Width < 1 || image.Height < 1 || image.Bytes < 1 || err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/"+image.Key) {
		return errors.New("invalid image")
	}
	return nil
}

func legacyImageKey(namespace, id, hash string) string {
	return "photos/" + namespace + "/" + id + "/" + hash + ".jpg"
}

func stableImageKey(namespace, id, name string) string {
	return "photos/" + namespace + "/" + id + "/" + name + ".jpg"
}

func imageURL(base, key string) string { return strings.TrimRight(base, "/") + "/" + key }

func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("--base-url must be a public HTTPS base URL")
	}
	return strings.TrimRight(raw, "/"), nil
}

func newID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", errors.New("could not generate a manifest revision")
	}
	return hex.EncodeToString(bytes[:]), nil
}

func publisherRoot() (string, error) {
	if root := os.Getenv("R2PUBLISHER_HOME"); root != "" {
		if !filepath.IsAbs(root) {
			return "", errors.New("R2PUBLISHER_HOME must be absolute")
		}
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("could not locate the R2 Publisher settings directory")
	}
	return filepath.Join(home, "Library", "Application Support", "R2Publisher"), nil
}

func loadProfile(root, name string) (profile, credentials, error) {
	var p profile
	var secret credentials
	if err := decodeFile(filepath.Join(root, "profiles", name+".json"), &p); err != nil {
		return p, secret, errors.New("could not load R2 Publisher profile")
	}
	if err := decodeFile(filepath.Join(root, "credentials", name+".json"), &secret); err != nil {
		return p, secret, errors.New("could not load R2 Publisher credentials")
	}
	endpoint, err := url.Parse(p.Endpoint)
	if err != nil || endpoint.Scheme != "https" || !strings.HasSuffix(endpoint.Hostname(), ".r2.cloudflarestorage.com") || endpoint.User != nil || endpoint.Port() != "" || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" || strings.TrimSpace(p.Bucket) == "" || strings.TrimSpace(secret.AccessKeyID) == "" || strings.TrimSpace(secret.SecretAccessKey) == "" {
		return p, secret, errors.New("invalid R2 Publisher profile or credentials")
	}
	return p, secret, nil
}

func decodeFile(path string, into any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 64<<10))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(into); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func newStore(p profile, secret credentials) r2Store {
	client := s3.New(s3.Options{
		Region: "auto", BaseEndpoint: aws.String(p.Endpoint), UsePathStyle: true,
		Credentials:                awscreds.NewStaticCredentialsProvider(secret.AccessKeyID, secret.SecretAccessKey, ""),
		HTTPClient:                 &http.Client{Timeout: 90 * time.Second},
		Retryer:                    retry.NewStandard(func(options *retry.StandardOptions) { options.MaxAttempts = 4; options.MaxBackoff = 5 * time.Second }),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return r2Store{client: client, bucket: p.Bucket}
}
