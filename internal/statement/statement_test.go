package statement

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kasofsk/track-record/internal/key"
)

func inputs() key.Inputs {
	return key.Inputs{
		Namespace: "acme",
		Name:      "unit",
		Dir:       ".",
		Argv:      []string{"go", "test", "./..."},
		Files:     []key.File{{Path: "go.mod", Kind: key.Regular, SHA256: key.Digest([]byte("module x\n"))}},
		Tools:     []key.Tool{{Command: "go version", Output: "go1.24\n"}},
		Env:       []key.Var{{Name: "SECRET", Value: "hunter2", Set: true}},
	}
}

func TestNewParses(t *testing.T) {
	in := inputs()
	raw, err := json.Marshal(New(in))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(raw, in.Key())
	if err != nil {
		t.Fatalf("Parse(New()): %v\n%s", err, raw)
	}
	if s.Subject[0].Name != "unit" || len(s.Predicate.PassedTests) != 1 || s.Predicate.PassedTests[0] != "unit" {
		t.Errorf("subject or passedTests do not name the command: %s", raw)
	}
	sections := in.Sections()
	if len(s.Predicate.Configuration) != len(sections) {
		t.Fatalf("%d descriptors for %d sections", len(s.Predicate.Configuration), len(sections))
	}
	for i, rd := range s.Predicate.Configuration {
		if rd.Name != sections[i].Label || rd.Digest["sha256"] != key.Digest(sections[i].Bytes) {
			t.Errorf("descriptor %d = %+v, want section %q's digest", i, rd, sections[i].Label)
		}
	}
	for _, leak := range []string{"hunter2", "go1.24", "go.mod"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("statement contains %q: %s", leak, raw)
		}
	}
}

func TestParseRejects(t *testing.T) {
	in := inputs()
	k := in.Key()
	tests := []struct {
		name   string
		change func(s map[string]any)
	}{
		{"wrong _type", func(s map[string]any) { s["_type"] = "https://in-toto.io/Statement/v0.1" }},
		{"no subject", func(s map[string]any) { s["subject"] = []any{} }},
		{"two subjects", func(s map[string]any) {
			sub := s["subject"].([]any)
			s["subject"] = append(sub, sub[0])
		}},
		{"subject digest not the key", func(s map[string]any) {
			subject(s)["digest"] = map[string]any{"sha256": strings.Repeat("0", 64)}
		}},
		{"subject digest not sha256", func(s map[string]any) {
			subject(s)["digest"] = map[string]any{"sha512": k}
		}},
		{"unnamed subject", func(s map[string]any) { delete(subject(s), "name") }},
		{"wrong predicateType", func(s map[string]any) { s["predicateType"] = "https://slsa.dev/provenance/v1" }},
		{"failed", func(s map[string]any) { predicate(s)["result"] = "FAILED" }},
		{"warned", func(s map[string]any) { predicate(s)["result"] = "WARNED" }},
		{"no result", func(s map[string]any) { delete(predicate(s), "result") }},
		{"no configuration", func(s map[string]any) { delete(predicate(s), "configuration") }},
		{"empty descriptor", func(s map[string]any) {
			predicate(s)["configuration"] = []any{map[string]any{"name": "x"}}
		}},
		{"no predicate", func(s map[string]any) { delete(s, "predicate") }},
		{"wrong type", func(s map[string]any) { predicate(s)["result"] = 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := asMap(t, New(in))
			tt.change(s)
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(raw, k); err == nil {
				t.Errorf("accepted %s", raw)
			}
		})
	}
	for _, raw := range []string{"", "null", "[]", `"x"`, "{"} {
		if _, err := Parse([]byte(raw), k); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestParseIgnoresUnrecognisedFields(t *testing.T) {
	in := inputs()
	s := asMap(t, New(in))
	s["https://example.com/extension"] = true
	predicate(s)["warnedTests"] = []any{}
	predicate(s)["https://example.com/more"] = map[string]any{"x": 1}
	raw, _ := json.Marshal(s)
	if _, err := Parse(raw, in.Key()); err != nil {
		t.Error(err)
	}
}

func asMap(t *testing.T, s Statement) map[string]any {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func subject(s map[string]any) map[string]any {
	return s["subject"].([]any)[0].(map[string]any)
}

func predicate(s map[string]any) map[string]any {
	return s["predicate"].(map[string]any)
}
