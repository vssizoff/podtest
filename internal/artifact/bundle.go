package artifact

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const maxManifestBytes = 1 << 20

// BundleFile is a normalized regular file owned by an immutable task revision.
type BundleFile struct {
	Path string
	Data []byte
}

type bundleManifest struct {
	FormatVersion          uint32        `json:"format_version"`
	ExecutionFormat        string        `json:"execution_format"`
	ExecutionFormatVersion uint32        `json:"execution_format_version"`
	RunnerAPIVersion       *uint32       `json:"runner_api_version,omitempty"`
	Files                  []bundleEntry `json:"files"`
}

type bundleEntry struct {
	Path   string `json:"path"`
	Size   uint64 `json:"size"`
	SHA256 string `json:"sha256"`
}

// BuildBundle creates deterministic USTAR v1 bytes and the trusted descriptor
// that must be persisted alongside the immutable revision and object key.
func BuildBundle(executionFormat string, executionFormatVersion uint32, runnerAPIVersion uint32, files []BundleFile) ([]byte, Ref, error) {
	if executionFormatVersion != 1 || (executionFormat != "standard" && executionFormat != "scripted_lua") {
		return nil, Ref{}, errors.New("unsupported execution format/version")
	}
	if executionFormat == "scripted_lua" && runnerAPIVersion != 1 {
		return nil, Ref{}, errors.New("unsupported runner API version")
	}
	if executionFormat == "standard" && runnerAPIVersion != 0 {
		return nil, Ref{}, errors.New("runner API version is only valid for scripted_lua")
	}
	ordered := append([]BundleFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	manifest := bundleManifest{FormatVersion: 1, ExecutionFormat: executionFormat, ExecutionFormatVersion: executionFormatVersion, Files: []bundleEntry{}}
	if executionFormat == "scripted_lua" {
		manifest.RunnerAPIVersion = &runnerAPIVersion
	}
	nodes := map[string]bool{}
	const maxEntries = 20000
	const maxArchiveBytes = 256 << 20
	archiveBytes := int64(1024)
	for _, file := range ordered {
		if err := validateBundlePath(file.Path); err != nil {
			return nil, Ref{}, err
		}
		if _, exists := nodes[file.Path]; exists {
			return nil, Ref{}, errors.New("duplicate or conflicting bundle path")
		}
		nodes[file.Path] = true
		for parent := path.Dir(file.Path); parent != "."; parent = path.Dir(parent) {
			if nodes[parent] {
				return nil, Ref{}, errors.New("bundle file conflicts with directory")
			}
			nodes[parent] = false
		}
		if len(nodes) >= maxEntries || len(file.Data) > maxArchiveBytes {
			return nil, Ref{}, errors.New("bundle exceeds entry or byte limit")
		}
		archiveBytes += 512 + (int64(len(file.Data))+511)/512*512
		if archiveBytes > maxArchiveBytes {
			return nil, Ref{}, errors.New("bundle exceeds archive byte limit")
		}
		sum := sha256.Sum256(file.Data)
		manifest.Files = append(manifest.Files, bundleEntry{Path: file.Path, Size: uint64(len(file.Data)), SHA256: hex.EncodeToString(sum[:])})
	}
	if executionFormat == "scripted_lua" {
		found := false
		for _, file := range ordered {
			if file.Path == "runner.lua" {
				found = true
				break
			}
		}
		if !found {
			return nil, Ref{}, errors.New("scripted_lua bundle requires runner.lua")
		}
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return nil, Ref{}, err
	}
	if len(manifestBytes) > maxManifestBytes {
		return nil, Ref{}, errors.New("bundle manifest exceeds limit")
	}
	if archiveBytes+512+(int64(len(manifestBytes))+511)/512*512 > maxArchiveBytes {
		return nil, Ref{}, errors.New("bundle exceeds archive byte limit")
	}

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	entries := make([]BundleFile, 0, len(ordered)+1)
	entries = append(entries, BundleFile{Path: "manifest.json", Data: manifestBytes})
	entries = append(entries, ordered...)
	for _, file := range entries {
		header := &tar.Header{Name: file.Path, Mode: 0600, Size: int64(len(file.Data)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0)}
		if err := writer.WriteHeader(header); err != nil {
			return nil, Ref{}, fmt.Errorf("write USTAR header: %w", err)
		}
		if _, err := writer.Write(file.Data); err != nil {
			return nil, Ref{}, fmt.Errorf("write USTAR body: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, Ref{}, fmt.Errorf("finish USTAR archive: %w", err)
	}
	data := archive.Bytes()
	sum := sha256.Sum256(data)
	return data, Ref{Digest: hex.EncodeToString(sum[:]), Size: int64(len(data))}, nil
}

func validateBundlePath(name string) error {
	if name == "" || !utf8.ValidString(name) || len(name) > 256 || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return errors.New("invalid bundle path")
	}
	root, _, _ := strings.Cut(name, "/")
	if root == "manifest.json" || root == "solution" {
		return errors.New("reserved bundle path")
	}
	components := strings.Split(name, "/")
	if len(components) > 32 {
		return errors.New("bundle path exceeds depth limit")
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return errors.New("invalid bundle path component")
		}
		if len(component) > 255 {
			return errors.New("bundle path component exceeds limit")
		}
	}
	if !ustarPathRepresentable(name) {
		return errors.New("USTAR path cannot be represented")
	}
	return nil
}

func ustarPathRepresentable(name string) bool {
	if len(name) <= 100 {
		return true
	}
	for i := strings.LastIndexByte(name, '/'); i > 0; i = strings.LastIndexByte(name[:i], '/') {
		if i <= 155 && len(name[i+1:]) <= 100 {
			return true
		}
	}
	return false
}
