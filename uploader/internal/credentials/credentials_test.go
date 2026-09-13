package credentials

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalCredentials(t *testing.T) {
	root := t.TempDir()
	first := Secret{AccessKeyID: "example-id", SecretAccessKey: "example-secret"}
	if _, err := Load(root, "website"); err == nil {
		t.Fatal("missing credentials accepted")
	}
	for _, secret := range []Secret{first, {AccessKeyID: "rotated-id", SecretAccessKey: "rotated-secret"}} {
		if err := Save(root, "website", secret); err != nil {
			t.Fatal(err)
		}
		got, err := Load(root, "website")
		if err != nil || got != secret {
			t.Fatal("credentials did not round trip")
		}
		for path, want := range map[string]os.FileMode{"credentials": 0700, "credentials/website.json": 0600} {
			info, err := os.Stat(filepath.Join(root, path))
			if err != nil || info.Mode().Perm() != want {
				t.Fatalf("incorrect permissions for %s", path)
			}
		}
	}
	if _, err := Load(root, "other"); err == nil {
		t.Fatal("credentials leaked between profiles")
	}
	for _, name := range []string{"../escape", "", "/absolute"} {
		if Save(root, name, first) == nil {
			t.Fatal("invalid profile accepted")
		}
		if _, err := Load(root, name); err == nil {
			t.Fatal("invalid profile loaded")
		}
	}
	if Save(root, "website", Secret{AccessKeyID: " "}) == nil {
		t.Fatal("blank credentials accepted")
	}
}

func TestDecodeDoesNotEchoSecrets(t *testing.T) {
	for _, input := range []string{
		`{"accessKeyId":"example-id","secretAccessKey":""}`,
		`{"accessKeyId":"example-id","secretAccessKey":" "}`,
		`{"accessKeyId":"example-id","secretAccessKey":"example-secret","example-secret":true}`,
		`{"accessKeyId":"example-id","secretAccessKey":"example-secret"} {}`,
		`{"accessKeyId":"example-id","secretAccessKey":example-secret}`,
	} {
		_, err := Decode(strings.NewReader(input))
		if err == nil || strings.Contains(err.Error(), "example-") {
			t.Fatal("invalid credentials accepted or echoed")
		}
	}
}
