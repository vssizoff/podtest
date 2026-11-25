package artifact

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"
)

func TestBuildBundleStandardDeterministicAndVerified(t *testing.T) {
	files := []BundleFile{{Path: "tests/input.txt", Data: []byte("42\n")}, {Path: "tests/answer.txt", Data: []byte("42\n")}}
	one, desc, err := BuildBundle("standard", 1, 0, files)
	if err != nil {
		t.Fatal(err)
	}
	two, desc2, err := BuildBundle("standard", 1, 0, files)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, two) || desc != desc2 {
		t.Fatal("bundle output is not deterministic")
	}
	sum := sha256.Sum256(one)
	if desc.Size != int64(len(one)) || desc.Digest != hex.EncodeToString(sum[:]) {
		t.Fatal("descriptor does not match archive")
	}
	reader := tar.NewReader(bytes.NewReader(one))
	manifestHeader, err := reader.Next()
	if err != nil || manifestHeader.Name != "manifest.json" || manifestHeader.Format != tar.FormatUSTAR {
		t.Fatalf("manifest header: %#v, %v", manifestHeader, err)
	}
	manifestBytes, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var manifest bundleManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.FormatVersion != 1 || manifest.RunnerAPIVersion != nil || len(manifest.Files) != 2 || manifest.Files[0].Path != "tests/answer.txt" {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	for _, expected := range []string{"tests/answer.txt", "tests/input.txt"} {
		h, err := reader.Next()
		if err != nil || h.Name != expected || h.Format != tar.FormatUSTAR {
			t.Fatalf("entry %q: %#v %v", expected, h, err)
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("tar did not terminate: %v", err)
	}
}

func TestBuildBundleLuaVersionAndPathValidation(t *testing.T) {
	if _, _, err := BuildBundle("scripted_lua", 1, 1, []BundleFile{{Path: "runner.lua", Data: []byte("return 0\n")}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		format  string
		version uint32
		files   []BundleFile
	}{
		{"scripted_lua", 0, []BundleFile{{Path: "runner.lua"}}},
		{"scripted_lua", 1, nil},
		{"standard", 0, []BundleFile{{Path: "solution/main.c"}}},
		{"standard", 0, []BundleFile{{Path: "../escape"}}},
		{"standard", 0, []BundleFile{{Path: "manifest.json"}}},
		{"standard", 0, []BundleFile{{Path: "manifest.json/child"}}},
		{"standard", 0, []BundleFile{{Path: "file"}, {Path: "file/child"}}},
		{"standard", 0, []BundleFile{{Path: "file"}, {Path: "file"}}},
		{"standard", 1, []BundleFile{{Path: "valid.txt"}}},
	} {
		if _, _, err := BuildBundle(tc.format, 1, tc.version, tc.files); err == nil {
			t.Fatalf("expected rejection: %#v", tc)
		}
	}
}

func TestBuildBundleGoldenMatchesInvokerDecoderContract(t *testing.T) {
	files := []BundleFile{{Path: "runner.lua", Data: []byte("return 0\n")}, {Path: "tests/basic/input.txt", Data: []byte("42\n")}}
	archive, ref, err := BuildBundle("scripted_lua", 1, 1, files)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"format_version":1,"execution_format":"scripted_lua","execution_format_version":1,"runner_api_version":1,"files":[{"path":"runner.lua","size":9,"sha256":"` + hex.EncodeToString(sha256Bytes(files[0].Data)) + `"},{"path":"tests/basic/input.txt","size":3,"sha256":"` + hex.EncodeToString(sha256Bytes(files[1].Data)) + `"}]}`)
	r := tar.NewReader(bytes.NewReader(archive))
	h, err := r.Next()
	if err != nil || h.Name != "manifest.json" || h.Format != tar.FormatUSTAR {
		t.Fatalf("manifest: %#v %v", h, err)
	}
	gotManifest, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(gotManifest, manifest) {
		t.Fatalf("manifest mismatch: %s (%v)", gotManifest, err)
	}
	for _, file := range files {
		h, err = r.Next()
		if err != nil || h.Name != file.Path || h.Format != tar.FormatUSTAR || h.Mode != 0600 || h.Size != int64(len(file.Data)) {
			t.Fatalf("entry %s: %#v %v", file.Path, h, err)
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, file.Data) {
			t.Fatalf("entry data %s: %v", file.Path, err)
		}
	}
	if _, err = r.Next(); err != io.EOF {
		t.Fatalf("trailing entry: %v", err)
	}
	if ref.Size != int64(len(archive)) || ref.Digest != hex.EncodeToString(sha256Bytes(archive)) {
		t.Fatalf("descriptor: %+v", ref)
	}
}

func sha256Bytes(b []byte) []byte { sum := sha256.Sum256(b); return sum[:] }
