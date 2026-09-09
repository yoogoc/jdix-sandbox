// Package openapicheck compares an OpenAPI document with the routes a server
// actually registers.
//
// The specs under api/openapi are the contract the SDKs are generated from, so
// they have to stay true. Two things go wrong quietly: a route the server
// serves but the document omits is an undocumented API, and a documented route
// the server does not serve is a generated client that 404s. Neither shows up
// in any other test, because nothing else reads both.
//
// net/http's ServeMux does not expose what was registered, which is why each
// server keeps its routes as a table for this to read.
package openapicheck

import (
	"fmt"
	"os"
	"sort"
	"testing"

	"sigs.k8s.io/yaml"
)

// Doc is as much of an OpenAPI document as these checks need.
//
// A path item mixes operations, keyed by verb, with path-level keys such as
// `parameters`, which is an array — so values stay untyped and each operation
// is asserted where it is used.
type Doc struct {
	OpenAPI string                    `json:"openapi"`
	Paths   map[string]map[string]any `json:"paths"`
}

var httpVerbs = map[string]bool{
	"get": true, "put": true, "post": true,
	"delete": true, "patch": true, "head": true, "options": true,
}

// Load reads and parses a spec, failing the test if it is unusable.
func Load(t *testing.T, path string) Doc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var doc Doc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v", path, err)
	}
	if doc.OpenAPI == "" {
		t.Fatalf("%s has no openapi version", path)
	}
	if len(doc.Paths) == 0 {
		t.Fatalf("%s describes no paths", path)
	}
	return doc
}

// Each walks the real operations, skipping path-level keys.
func (d Doc) Each(fn func(verb, path string, op map[string]any)) {
	for path, item := range d.Paths {
		for key, raw := range item {
			if !httpVerbs[key] {
				continue
			}
			if op, ok := raw.(map[string]any); ok {
				fn(key, path, op)
			}
		}
	}
}

// Operations returns "METHOD /path" for every operation in the document.
func (d Doc) Operations() map[string]bool {
	out := map[string]bool{}
	d.Each(func(verb, path string, _ map[string]any) {
		out[fmt.Sprintf("%s %s", upper(verb), path)] = true
	})
	return out
}

// SameSurface asserts the document and the server describe the same API.
// served is "METHOD /path" for each registered route.
func SameSurface(t *testing.T, d Doc, served []string) {
	t.Helper()
	inSpec := d.Operations()
	inCode := map[string]bool{}
	for _, op := range served {
		inCode[op] = true
	}

	var undocumented, phantom []string
	for op := range inCode {
		if !inSpec[op] {
			undocumented = append(undocumented, op)
		}
	}
	for op := range inSpec {
		if !inCode[op] {
			phantom = append(phantom, op)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(phantom)

	for _, op := range undocumented {
		t.Errorf("the server serves %q but the spec does not describe it", op)
	}
	for _, op := range phantom {
		t.Errorf("the spec describes %q but the server does not serve it; a generated client would 404", op)
	}
}

// Named asserts every operation has a unique operationId. It becomes the method
// name in every generated client; without one the generator invents something
// from the path, and the SDKs end up disagreeing about what a call is called.
func Named(t *testing.T, d Doc) {
	t.Helper()
	seen := map[string]string{}
	d.Each(func(verb, path string, op map[string]any) {
		id, _ := op["operationId"].(string)
		if id == "" {
			t.Errorf("%s %s has no operationId", upper(verb), path)
			return
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("operationId %q is used by both %s and %s %s", id, prev, upper(verb), path)
		}
		seen[id] = upper(verb) + " " + path
	})
}

// TypedSuccess asserts success responses carry a schema. An untyped response
// produces a client method that hands back nothing useful, which is much the
// same as having no client.
func TypedSuccess(t *testing.T, d Doc) {
	t.Helper()
	d.Each(func(verb, path string, op map[string]any) {
		responses, _ := op["responses"].(map[string]any)
		for code, body := range responses {
			if code == "" || code[0] != '2' {
				continue
			}
			// 204 and 101 legitimately carry nothing.
			if code == "204" || code == "101" {
				continue
			}
			if b, _ := body.(map[string]any); b["content"] == nil {
				t.Errorf("%s %s: response %s has no content schema", upper(verb), path, code)
			}
		}
	})
}

func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 32
		}
	}
	return string(b)
}
