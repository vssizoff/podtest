package problem

import (
	"strings"
	"testing"
)

func TestValidateStandard(t *testing.T) {
	const manifest = `{"version":1,"language":"posix_sh","build":"package_source","checker":"exact_bytes","limits":{"output_bytes":4096},"groups":[{"id":"main","scoring":"sum"}],"tests":[{"id":"echo","group":"main","input":"tests/in","answer":"tests/out","points_milli":1000}]}`
	for _, tc := range []struct {
		name, manifest string
		extra          bool
		valid          bool
	}{
		{name: "valid", manifest: manifest, valid: true},
		{name: "missing answer", manifest: strings.Replace(manifest, "tests/out", "tests/missing", 1)},
		{name: "unknown limit", manifest: strings.Replace(manifest, "output_bytes", "output_byte", 1)},
		{name: "unreferenced file", manifest: manifest, extra: true},
		{name: "Polygon package", extra: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string][]byte{"standard.json": []byte(tc.manifest), "tests/in": []byte("in"), "tests/out": []byte("out")}
			if tc.extra {
				files["problem.xml"] = []byte("<problem/>")
			}
			language, err := validateStandard(files)
			if tc.valid && (err != nil || language != "posix_sh" || !strings.Contains(string(files["standard.json"]), `"wall_ms":300000`)) {
				t.Fatalf("valid package was not normalized: %q, %v", language, err)
			}
			if !tc.valid && err == nil {
				t.Fatal("invalid package was accepted")
			}
		})
	}
}
