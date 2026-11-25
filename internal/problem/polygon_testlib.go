package problem

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"path"
	"strings"
)

type polygonTestset struct {
	Name       string     `xml:"name,attr"`
	Time       uint64     `xml:"time-limit"`
	Memory     uint64     `xml:"memory-limit"`
	Count      int        `xml:"test-count"`
	Input      string     `xml:"input-path-pattern"`
	Answer     string     `xml:"answer-path-pattern"`
	GroupCount int        `xml:"group-count"`
	Groups     []struct{} `xml:"groups>group"`
	Tests      []struct {
		Group  string `xml:"group,attr"`
		Points string `xml:"points,attr"`
	} `xml:"tests>test"`
}

var polygonLanguages = []string{"javascript", "cpp23", "python314"}

func polygonTestlib(files map[string][]byte) (map[string][]byte, []string, string) {
	var p struct {
		Judging struct {
			Input   string           `xml:"input-file,attr"`
			Output  string           `xml:"output-file,attr"`
			Runs    int              `xml:"run-count,attr"`
			Testset []polygonTestset `xml:"testset"`
		} `xml:"judging"`
		Checker struct {
			Type string `xml:"type,attr"`
			File struct {
				Path string `xml:"path,attr"`
				Type string `xml:"type,attr"`
			} `xml:"source"`
		} `xml:"assets>checker"`
		Interactors []struct{} `xml:"assets>interactor"`
	}
	if err := xml.Unmarshal(files["problem.xml"], &p); err != nil || p.Checker.Type != "testlib" || !strings.HasPrefix(p.Checker.File.Type, "cpp.") {
		return nil, nil, "The package does not declare a supported C++ testlib checker."
	}
	j := p.Judging
	if len(j.Testset) != 1 || j.Input != "" || j.Output != "" || j.Runs > 1 || len(p.Interactors) != 0 {
		return nil, nil, "Testlib import requires one non-interactive stdin/stdout testset."
	}
	t := j.Testset[0]
	if t.Name == "" {
		t.Name = "tests"
	}
	if t.Count < 1 || t.Count > 64 || len(t.Tests) != t.Count || t.GroupCount != 0 || len(t.Groups) != 0 || t.Time == 0 || t.Memory == 0 {
		return nil, nil, "Unsupported Polygon test count, test groups or missing limits."
	}
	checker := p.Checker.File.Path
	header := path.Join(path.Dir(checker), "testlib.h")
	if files[header] == nil {
		header = "files/testlib.h"
	}
	out := map[string][]byte{"checker/checker.cpp": files[checker], "checker/testlib.h": files[header]}
	type test struct {
		ID     string `json:"id"`
		Group  string `json:"group"`
		Input  string `json:"input"`
		Answer string `json:"answer"`
		Points int    `json:"points_milli"`
	}
	tests := make([]test, 0, t.Count)
	for i, item := range t.Tests {
		if item.Group != "" || item.Points != "" {
			return nil, nil, "Polygon group and explicit test scoring cannot be converted."
		}
		in, okIn := polygonTestPath(t.Input, i+1)
		ans, okAns := polygonTestPath(t.Answer, i+1)
		if !okIn || !okAns || files[in] == nil || files[ans] == nil {
			return nil, nil, "The package is missing generated test inputs or answers."
		}
		id := fmt.Sprintf("%02d", i+1)
		inPath, ansPath := "tests/"+id+".in", "tests/"+id+".ans"
		out[inPath], out[ansPath] = files[in], files[ans]
		tests = append(tests, test{id, "main", inPath, ansPath, (i+1)*100000/t.Count - i*100000/t.Count})
	}
	out["standard.json"], _ = json.Marshal(map[string]any{
		"version": 1, "language": "javascript", "languages": polygonLanguages, "build": "package_source", "checker": "testlib", "testset": t.Name,
		"limits": map[string]uint64{"wall_ms": t.Time, "memory_bytes": t.Memory, "output_bytes": 32 << 20},
		"groups": []map[string]string{{"id": "main", "scoring": "sum"}}, "tests": tests,
	})
	_, err := validateStandardLanguages(out, polygonLanguages)
	if err != nil {
		return nil, nil, "The checker sources or test package exceed the supported Standard limits."
	}
	return out, polygonLanguages, ""
}

func polygonTestPath(pattern string, n int) (string, bool) {
	for _, token := range []string{"%d", "%02d", "%03d", "%04d"} {
		if strings.Count(pattern, "%") == 1 && strings.Contains(pattern, token) {
			name := strings.Replace(pattern, token, fmt.Sprintf(token, n), 1)
			return name, standardTestPath(name)
		}
	}
	return "", false
}
