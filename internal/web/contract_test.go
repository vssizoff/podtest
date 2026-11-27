package web

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

func TestPublicRouteContract(t *testing.T) {
	data, err := os.ReadFile("../../openapi/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]yaml.Node
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	ids := map[string]bool{}
	for path, operations := range spec.Paths {
		for method, node := range operations {
			switch method {
			case "get", "post", "patch", "put", "delete", "head", "options", "trace":
				var op struct {
					OperationID string `yaml:"operationId"`
				}
				if err := node.Decode(&op); err != nil {
					t.Fatal(err)
				}
				if op.OperationID == "" || ids[op.OperationID] {
					t.Errorf("missing or duplicate operationId for %s %s", method, path)
				}
				ids[op.OperationID] = true
				documented[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	err = chi.Walk(Router(nil, nil, nil, nil).(chi.Routes), func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if relative, public := strings.CutPrefix(path, "/api/v1/"); public {
			key := method + " /" + relative
			if !documented[key] {
				t.Errorf("undocumented public route: %s", key)
			}
			delete(documented, key)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for key := range documented {
		t.Errorf("OpenAPI operation has no route: %s", key)
	}
}
