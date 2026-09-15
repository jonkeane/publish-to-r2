// Package manifest defines the public, versioned gallery format.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const Version = 1
const MaxJSON = 32 << 20

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ValidID(s string) bool   { return idPattern.MatchString(s) }
func ValidHash(s string) bool { return hashPattern.MatchString(s) }

// PublicMetadata maps the Flickr examples to Lightroom metadata. GPS is excluded.
type PublicMetadata struct {
	Title     string   `json:"title,omitempty"`
	Caption   string   `json:"caption,omitempty"`
	Alt       string   `json:"alt,omitempty"`
	Copyright string   `json:"copyright,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	DateTaken string   `json:"dateTaken,omitempty"`
	OwnerName string   `json:"ownerName,omitempty"`
	License   string   `json:"license,omitempty"`
	EXIF      *EXIF    `json:"exif,omitempty"`
}

type EXIF struct {
	Emulsion          string `json:"emulsion,omitempty"`
	Make              string `json:"make,omitempty"`
	Model             string `json:"model,omitempty"`
	Lens              string `json:"lens,omitempty"`
	FocalLength       string `json:"focallength,omitempty"`
	FStop             string `json:"fstop,omitempty"`
	Exposure          string `json:"exposure,omitempty"`
	ISO               string `json:"iso,omitempty"`
	Time              string `json:"time,omitempty"`
	Flash             string `json:"flash,omitempty"`
	PreservedFilename string `json:"preservedfilename,omitempty"`
}

// RenditionNames are the two additional files; the entry itself is the large image.
var RenditionNames = []string{"thumbnail", "gallery"}

func ValidRendition(name string) bool { return name == "thumbnail" || name == "gallery" }
func ValidImageName(name string) bool { return name == "large" || ValidRendition(name) }

type Image struct {
	Key    string `json:"key"`
	Source string `json:"source"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func (i Image) Validate(namespace, id, name, base string) error {
	u, err := url.Parse(i.Source)
	if !ValidHash(i.SHA256) || i.Key != Key(namespace, id, name) || i.Bytes <= 0 || i.Width <= 0 || i.Height <= 0 || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || i.Source != URL(base, i.Key) {
		return errors.New("invalid rendition or foreign image URL")
	}
	return nil
}

type Entry struct {
	Renditions     map[string]Image `json:"renditions,omitempty"`
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
	PublicMetadata
}

func (e Entry) Image() Image {
	return Image{Key: e.Key, Source: e.Source, Width: e.Width, Height: e.Height, Bytes: e.Bytes, SHA256: e.SHA256}
}

// Images enumerates every reference in a stable order for upload and retention.
func (e Entry) Images() []Image {
	images := []Image{e.Image()}
	for _, name := range RenditionNames {
		if i, ok := e.Renditions[name]; ok {
			images = append(images, i)
		}
	}
	return images
}

type Manifest struct {
	SchemaVersion  int       `json:"schemaVersion"`
	Revision       string    `json:"revision"`
	ParentRevision string    `json:"parentRevision"`
	GalleryID      string    `json:"galleryId"`
	Namespace      string    `json:"namespace"`
	ServiceID      string    `json:"serviceId"`
	Title          string    `json:"title"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Entries        []Entry   `json:"entries"`
}

// Key is the stable public object key for one image size.
func Key(namespace, id, name string) string {
	return "photos/" + namespace + "/" + id + "/" + name + ".jpg"
}

// StagingKey is an immutable, non-public upload location. It is promoted to
// the corresponding Key only after every image in the job has verified.
func StagingKey(namespace, id, hash string) string {
	return "staging/" + namespace + "/" + id + "/" + hash + ".jpg"
}

// LegacyKey identifies the hash-addressed public layout accepted only by
// migration and retention cleanup. New manifests must use Key.
func LegacyKey(namespace, id, hash string) string {
	return "photos/" + namespace + "/" + id + "/" + hash + ".jpg"
}
func CurrentKey(gallery string) string { return "galleries/" + gallery + "/current.json" }
func HistoryKey(gallery, revision string) string {
	return "galleries/" + gallery + "/history/" + revision + ".json"
}
func URL(base, key string) string { return strings.TrimRight(base, "/") + "/" + key }

func Decode(r io.Reader, v any) error {
	d := json.NewDecoder(io.LimitReader(r, MaxJSON+1))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid JSON or unknown field")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != Version || !ValidID(m.Revision) || !ValidID(m.GalleryID) || !ValidID(m.Namespace) || !ValidID(m.ServiceID) || (m.ParentRevision != "" && !ValidID(m.ParentRevision)) || m.Entries == nil || m.UpdatedAt.IsZero() {
		return errors.New("invalid manifest header")
	}
	seen := map[string]bool{}
	for _, e := range m.Entries {
		u, err := url.Parse(e.Source)
		if !ValidID(e.ID) || !ValidHash(e.SHA256) || e.Key != Key(m.Namespace, e.ID, "large") || e.Bytes <= 0 || e.Width <= 0 || e.Height <= 0 || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/"+e.Key) || seen[e.ID] {
			return fmt.Errorf("invalid or duplicate manifest entry")
		}
		if e.Renditions != nil {
			if len(e.Renditions) != len(RenditionNames) {
				return errors.New("incomplete rendition set")
			}
			base := strings.TrimSuffix(e.Source, "/"+e.Key)
			for name, image := range e.Renditions {
				if !ValidRendition(name) {
					return errors.New("unknown rendition")
				}
				if err := image.Validate(m.Namespace, e.ID, name, base); err != nil {
					return err
				}
			}
		}
		seen[e.ID] = true
	}
	return nil
}

// Merge keeps existing order, appends new IDs, and removes only explicit IDs.
func Merge(old Manifest, updates []Entry, removals []string, order []string) ([]Entry, error) {
	remove := map[string]bool{}
	index := map[string]Entry{}
	for _, id := range removals {
		if !ValidID(id) || remove[id] {
			return nil, errors.New("invalid removal")
		}
		remove[id] = true
	}
	for _, e := range updates {
		if _, ok := index[e.ID]; ok || remove[e.ID] {
			return nil, errors.New("duplicate or removed update")
		}
		index[e.ID] = e
	}
	entries := make([]Entry, 0, len(old.Entries)+len(updates))
	seen := map[string]bool{}
	for _, e := range old.Entries {
		if remove[e.ID] {
			continue
		}
		if u, ok := index[e.ID]; ok {
			e = u
		}
		entries = append(entries, e)

		seen[e.ID] = true
	}
	for _, e := range updates {
		if !seen[e.ID] {
			entries = append(entries, e)

			seen[e.ID] = true
		}
	}
	if order != nil {
		if len(order) != len(entries) {
			return nil, errors.New("order must name every gallery entry")
		}
		byID := map[string]Entry{}
		for _, e := range entries {
			byID[e.ID] = e
		}
		entries = make([]Entry, 0, len(order))
		for _, id := range order {
			e, ok := byID[id]
			if !ok {
				return nil, errors.New("invalid order")
			}
			entries = append(entries, e)
			delete(byID, id)
		}
	}
	return entries, nil
}
