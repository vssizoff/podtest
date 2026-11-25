package problem

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"testing"
)

func TestRevisionBundle(t *testing.T) {
	m := revisionManifest{Format: "scripted_lua", FormatVersion: 1, RunnerVersion: 1}
	b, err := buildRevisionBundle(m, map[string][]byte{"runner.lua": []byte("return 0\n")})
	if err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(bytes.NewReader(b))
	h, err := r.Next()
	if err != nil || h.Name != "manifest.json" || h.Format != tar.FormatUSTAR {
		t.Fatalf("manifest header: %v %v", h, err)
	}
	var manifest struct {
		Format  string `json:"execution_format"`
		Version int    `json:"format_version"`
		API     int    `json:"runner_api_version"`
	}
	if err := json.NewDecoder(r).Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Format != m.Format || manifest.Version != 1 || manifest.API != 1 {
		t.Fatalf("manifest: %+v", manifest)
	}
	h, err = r.Next()
	if err != nil || h.Name != "runner.lua" {
		t.Fatalf("runner header: %v %v", h, err)
	}
}
