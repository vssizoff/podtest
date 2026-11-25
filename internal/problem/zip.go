package problem

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"
)

var ErrArchive = errors.New("archive must contain unique relative regular files within size limits")

type PolygonReport struct {
	Title      string   `json:"title"`
	Warnings   []string `json:"warnings"`
	Compatible bool     `json:"compatible"`
}

func Files(data []byte) (map[string][]byte, error) {
	return archiveFiles(data, 2048, 32<<20, 68<<20)
}

func archiveFiles(data []byte, entries, fileLimit, totalLimit int) (map[string][]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(reader.File) > entries {
		return nil, ErrArchive
	}
	files := map[string][]byte{}
	var total int64
	for _, f := range reader.File {
		name := strings.TrimSuffix(f.Name, "/")
		if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:\x00") {
			return nil, ErrArchive
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() || files[name] != nil || f.UncompressedSize64 > uint64(fileLimit) {
			return nil, ErrArchive
		}
		rc, err := f.Open()
		if err != nil {
			return nil, ErrArchive
		}
		b, err := io.ReadAll(io.LimitReader(rc, int64(fileLimit)+1))
		rc.Close()
		total += int64(len(b))
		if err != nil || len(b) > fileLimit || total > int64(totalLimit) {
			return nil, ErrArchive
		}
		files[name] = b
	}
	for name := range files {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if files[parent] != nil {
				return nil, ErrArchive
			}
		}
	}
	return files, nil
}

func InspectPolygon(data []byte) (PolygonReport, error) {
	files, err := polygonFiles(data)
	if err != nil {
		return PolygonReport{}, err
	}
	report, err := inspectPolygonFiles(files)
	if err != nil {
		return report, err
	}
	_, _, reason := polygonTestlib(files)
	report.Compatible = reason == ""
	if reason != "" {
		report.Warnings = append(report.Warnings, reason)
	}
	return report, nil
}

func polygonFiles(data []byte) (map[string][]byte, error) {
	if len(data) > MaxPolygonBytes {
		return nil, ErrArchive
	}
	return archiveFiles(data, 12000, 32<<20, 128<<20)
}

var ErrPolygon = errors.New("expected Polygon problem.xml with a task name at the archive root")

func inspectPolygonFiles(files map[string][]byte) (PolygonReport, error) {
	var manifest struct {
		XMLName xml.Name `xml:"problem"`
		Names   []struct {
			Value string `xml:"value,attr"`
		} `xml:"names>name"`
	}
	if len(files["problem.xml"]) > 1<<20 || xml.Unmarshal(files["problem.xml"], &manifest) != nil || len(manifest.Names) == 0 {
		return PolygonReport{}, ErrPolygon
	}
	return PolygonReport{Title: manifest.Names[0].Value, Warnings: []string{}}, nil
}
