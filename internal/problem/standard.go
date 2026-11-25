package problem

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

var standardID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
var standardPathPart = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// Keep these limits aligned with the Invoker's Standard v1 contract. Re-emit
// the validated JSON so the executor receives a single canonical field set.
func validateStandard(files map[string][]byte) (string, error) {
	return validateStandardLanguages(files, nil)
}

func validateStandardLanguages(files map[string][]byte, allowed []string) (string, error) {
	invalid := fmt.Errorf("%w: invalid Standard v1 package", ErrInvalid)
	var task struct {
		Version   uint32   `json:"version"`
		Language  string   `json:"language"`
		Languages []string `json:"languages,omitempty"`
		Build     string   `json:"build"`
		Checker   string   `json:"checker"`
		Testset   string   `json:"testset,omitempty"`
		Limits    *struct {
			Memory uint64 `json:"memory_bytes"`
			Swap   uint64 `json:"swap_bytes"`
			PIDs   uint32 `json:"pids"`
			CPU    uint64 `json:"cpu_quota_micros"`
			Wall   uint64 `json:"wall_ms"`
			Output uint64 `json:"output_bytes"`
		} `json:"limits"`
		Groups []struct {
			ID      string `json:"id"`
			Scoring string `json:"scoring"`
		} `json:"groups"`
		Tests []struct {
			ID     string  `json:"id"`
			Group  string  `json:"group"`
			Input  string  `json:"input"`
			Answer string  `json:"answer"`
			Points *uint32 `json:"points_milli"`
		} `json:"tests"`
	}
	raw := files["standard.json"]
	if len(raw) > 64<<10 || !utf8.Valid(raw) {
		return "", invalid
	}
	// Decode limits separately to supply the same defaults as the executor.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", invalid
	}
	limits := fields["limits"]
	if !bytes.HasPrefix(bytes.TrimSpace(limits), []byte("{")) {
		return "", invalid
	}
	var limitFields map[string]json.RawMessage
	if err := json.Unmarshal(limits, &limitFields); err != nil {
		return "", invalid
	}
	for _, encoded := range limitFields {
		if bytes.Equal(bytes.TrimSpace(encoded), []byte("null")) {
			return "", invalid
		}
	}
	if err := json.Unmarshal([]byte(`{"limits":{"memory_bytes":134217728,"pids":64,"cpu_quota_micros":100000,"wall_ms":300000,"output_bytes":536870912}}`), &task); err != nil {
		panic(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&task) != nil || decoder.Decode(new(any)) != io.EOF || task.Version != 1 || task.Limits == nil || task.Build != "package_source" ||
		!slices.Contains([]string{"posix_sh", "javascript", "cpp23", "python314"}, task.Language) || !slices.Equal(task.Languages, allowed) || (len(allowed) != 0 && !slices.Contains(allowed, task.Language)) ||
		(task.Checker != "exact_bytes" && task.Checker != "ascii_tokens" && task.Checker != "testlib") ||
		(task.Testset != "" && !standardID.MatchString(task.Testset)) ||
		len(task.Tests) == 0 || len(task.Tests) > 64 || len(task.Groups) == 0 || len(task.Groups) > len(task.Tests) {
		return "", invalid
	}
	l := task.Limits
	fileLimit, payloadLimit, outputLimit := 1<<20, 16<<20, uint64(2<<20)
	if task.Checker == "testlib" {
		fileLimit, payloadLimit, outputLimit = 32<<20, 64<<20, 32<<20
	}
	if l.Memory == 0 || l.Memory > 512<<20 || l.Swap != 0 || l.PIDs == 0 || l.PIDs > 128 || l.CPU == 0 || l.CPU > 200000 || l.Wall == 0 || l.Wall > 300000 || l.Output == 0 || l.Output > outputLimit {
		return "", invalid
	}
	groups := make(map[string]int, len(task.Groups))
	for _, g := range task.Groups {
		if _, duplicate := groups[g.ID]; duplicate || !standardID.MatchString(g.ID) || (g.Scoring != "sum" && g.Scoring != "all_or_nothing") {
			return "", invalid
		}
		groups[g.ID] = 0
	}
	tests := make(map[string]bool, len(task.Tests))
	used := map[string]bool{"standard.json": true}
	if task.Checker == "testlib" {
		for _, path := range []string{"checker/checker.cpp", "checker/testlib.h"} {
			if n := len(files[path]); n == 0 || n > 1<<20 {
				return "", invalid
			}
			used[path] = true
		}
	}
	var total int
	for _, tc := range task.Tests {
		if _, known := groups[tc.Group]; !known || tests[tc.ID] || !standardID.MatchString(tc.ID) || tc.Points == nil {
			return "", invalid
		}
		groups[tc.Group]++
		tests[tc.ID] = true
		for i, path := range []string{tc.Input, tc.Answer} {
			file, found := files[path]
			if !found || !standardTestPath(path) || len(file) > fileLimit || (i == 0 && uint64(len(file)) > l.Output) {
				return "", invalid
			}
			used[path] = true
			total += len(file)
			if total > payloadLimit {
				return "", invalid
			}
		}
	}
	for _, count := range groups {
		if count == 0 {
			return "", invalid
		}
	}
	if len(used) != len(files) {
		return "", invalid
	}
	normalized, err := json.Marshal(task)
	if err != nil || len(normalized) > 64<<10 {
		return "", invalid
	}
	files["standard.json"] = normalized
	return task.Language, nil
}

func standardTestPath(path string) bool {
	if !strings.HasPrefix(path, "tests/") || len(path) > 200 {
		return false
	}
	parts := strings.Split(path, "/")
	if len(parts) > 16 {
		return false
	}
	for _, p := range parts {
		if p == "." || p == ".." || !standardPathPart.MatchString(p) {
			return false
		}
	}
	return true
}
