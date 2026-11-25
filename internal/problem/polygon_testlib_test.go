package problem

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestPolygonTestlibConversion(t *testing.T) {
	const manifest = `<problem><judging><testset name="tests">
<time-limit>2000</time-limit><memory-limit>134217728</memory-limit><test-count>3</test-count>
<input-path-pattern>tests/%02d</input-path-pattern><answer-path-pattern>tests/%02d.a</answer-path-pattern>
<tests><test/><test/><test/></tests></testset></judging>
<assets><checker type="testlib"><source type="cpp.g++17" path="check.cpp"/></checker></assets></problem>`
	files := map[string][]byte{
		"problem.xml": []byte(manifest), "check.cpp": []byte("#include \"testlib.h\""),
		"files/testlib.h": []byte("// fixture"),
		"tests/01":        {}, "tests/02": {}, "tests/03": {},
		"tests/01.a": make([]byte, 17<<20), "tests/02.a": {}, "tests/03.a": {},
	}
	bundle, language, reason := polygonTestlib(files)
	if reason != "" || !slices.Equal(language, []string{"javascript", "cpp23", "python314"}) || len(bundle["tests/01.ans"]) != 17<<20 || len(bundle["checker/testlib.h"]) == 0 {
		t.Fatalf("conversion: language=%q reason=%q", language, reason)
	}
	var task struct {
		Checker string
		Tests   []struct {
			Points uint32 `json:"points_milli"`
		}
	}
	if err := json.Unmarshal(bundle["standard.json"], &task); err != nil || task.Checker != "testlib" || len(task.Tests) != 3 || task.Tests[0].Points+task.Tests[1].Points+task.Tests[2].Points != 100000 {
		t.Fatalf("normalized task: %+v, %v", task, err)
	}
	for _, tc := range []struct{ before, after, reason string }{
		{`<judging>`, `<judging input-file="data.in">`, "non-interactive stdin/stdout"},
		{`<test/>`, `<test points="30"/>`, "explicit test scoring"},
		{`tests/%02d.a`, `tests/%02d.missing`, "missing generated"},
		{`<assets>`, `<assets><interactor/>`, "non-interactive stdin/stdout"},
	} {
		files["problem.xml"] = []byte(strings.Replace(manifest, tc.before, tc.after, 1))
		if out, _, why := polygonTestlib(files); out != nil || !strings.Contains(why, tc.reason) {
			t.Errorf("%s: reason=%q", tc.after, why)
		}
	}
}
