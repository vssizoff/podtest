package problem

import (
	"archive/zip"
	"bytes"
	"errors"
	"testing"
)

func makeZIP(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, entry := range entries {
		file, err := w.Create(entry[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err = file.Write([]byte(entry[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInspectFullPolygon(t *testing.T) {
	data := makeZIP(t,
		[2]string{"problem.xml", `<problem><names><name value="Example"/></names></problem>`},
		[2]string{"tests/01", "1 2\n"},
		[2]string{"tests/01.a", "3\n"},
	)
	report, err := InspectPolygon(data)
	if err != nil || report.Title != "Example" || report.Compatible {
		t.Fatalf("unexpected report: %+v, %v", report, err)
	}
}

func TestArchiveLimitsAndPaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries [][2]string
	}{
		{"traversal", [][2]string{{"../test", "1"}}},
		{"duplicate", [][2]string{{"tests/01", "1"}, {"tests/01", "2"}}},
		{"file as parent", [][2]string{{"tests", "1"}, {"tests/01", "2"}}},
		{"oversize file", [][2]string{{"tests/01", "123456789"}}},
		{"oversize total", [][2]string{{"tests/01", "12345678"}, {"tests/02", "12345"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := archiveFiles(makeZIP(t, tc.entries...), 10, 8, 12)
			if !errors.Is(err, ErrArchive) {
				t.Fatalf("expected invalid archive, got %v", err)
			}
		})
	}
}
