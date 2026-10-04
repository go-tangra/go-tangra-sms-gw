//go:build ui

package ui

import (
	"encoding/json"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/go-tangra/go-tangra-sms-gw/v4/pkg/smsgwmanifest"
)

// The built remote must be what the manifest registers: the shell loads
// /m/<module>/mf-manifest.json under the module name and imports Exposes.
func TestBuiltRemoteMatchesManifest(t *testing.T) {
	remote, ok := Remote()
	if !ok {
		t.Fatal("built remote absent: run npm run build in ui/")
	}
	raw, err := fs.ReadFile(remote, "mf-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Name     string `json:"name"`
		MetaData struct {
			PublicPath  string `json:"publicPath"`
			RemoteEntry struct {
				Name string `json:"name"`
			} `json:"remoteEntry"`
		} `json:"metaData"`
		Exposes []struct {
			Path string `json:"path"`
		} `json:"exposes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("federation manifest: %v", err)
	}
	if doc.Name != smsgwmanifest.Module {
		t.Errorf("remote name %q, want %q", doc.Name, smsgwmanifest.Module)
	}
	if want := "/m/" + smsgwmanifest.Module + "/"; doc.MetaData.PublicPath != want {
		t.Errorf("public path %q, want %q", doc.MetaData.PublicPath, want)
	}
	if _, err := fs.Stat(remote, doc.MetaData.RemoteEntry.Name); err != nil {
		t.Errorf("remote entry %q: %v", doc.MetaData.RemoteEntry.Name, err)
	}
	var exposes []string
	for _, e := range doc.Exposes {
		exposes = append(exposes, e.Path)
	}
	slices.Sort(exposes)
	want := slices.Clone(smsgwmanifest.Exposes)
	slices.Sort(want)
	if !slices.Equal(exposes, want) {
		t.Errorf("exposes %v, want %v", exposes, want)
	}
	// Nothing in the remote may point at a development server or carry source maps.
	_ = fs.WalkDir(remote, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(p, ".map") {
			t.Errorf("source map shipped: %s", p)
		}
		return nil
	})
}
