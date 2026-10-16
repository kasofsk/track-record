// Package statement builds and checks the statement a record holds: an
// in-toto Statement v1 with the Test Result predicate, whose single subject
// is the key.
package statement

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/kasofsk/track-record/internal/key"
)

const (
	Type          = "https://in-toto.io/Statement/v1"
	PredicateType = "https://in-toto.io/attestation/test-result/v0.1"
	Passed        = "PASSED"
)

// Statement is an in-toto Statement v1 carrying a Test Result predicate.
type Statement struct {
	Type          string               `json:"_type"`
	Subject       []ResourceDescriptor `json:"subject"`
	PredicateType string               `json:"predicateType"`
	Predicate     TestResult           `json:"predicate"`
}

// ResourceDescriptor is an in-toto ResourceDescriptor v1.
type ResourceDescriptor struct {
	Name             string            `json:"name,omitempty"`
	URI              string            `json:"uri,omitempty"`
	Digest           map[string]string `json:"digest,omitempty"`
	Content          []byte            `json:"content,omitempty"`
	DownloadLocation string            `json:"downloadLocation,omitempty"`
	MediaType        string            `json:"mediaType,omitempty"`
	Annotations      map[string]any    `json:"annotations,omitempty"`
}

// TestResult is the Test Result predicate, v0.1.
type TestResult struct {
	Result        string               `json:"result"`
	Configuration []ResourceDescriptor `json:"configuration"`
	URL           string               `json:"url,omitempty"`
	PassedTests   []string             `json:"passedTests,omitempty"`
	WarnedTests   []string             `json:"warnedTests,omitempty"`
	FailedTests   []string             `json:"failedTests,omitempty"`
}

// New returns the statement that the command in describes passed. Its
// configuration has one descriptor per section of the key's preimage: the
// section's digest, and a description that holds the command line, counts and
// names but never file contents, tool output or variable values.
func New(in key.Inputs) Statement {
	in = in.Canonical()
	sections := in.Sections()
	config := make([]ResourceDescriptor, 0, len(sections))
	for _, s := range sections {
		config = append(config, ResourceDescriptor{
			Name:        s.Label,
			Digest:      map[string]string{"sha256": key.Digest(s.Bytes)},
			Annotations: describe(in, s.Label),
		})
	}
	return Statement{
		Type:          Type,
		Subject:       []ResourceDescriptor{{Name: in.Name, Digest: map[string]string{"sha256": in.Key()}}},
		PredicateType: PredicateType,
		Predicate: TestResult{
			Result:        Passed,
			Configuration: config,
			PassedTests:   []string{in.Name},
		},
	}
}

func describe(in key.Inputs, label string) map[string]any {
	switch label {
	case "namespace":
		return map[string]any{"namespace": in.Namespace}
	case "name":
		return map[string]any{"name": in.Name}
	case "dir":
		return map[string]any{"dir": in.Dir}
	case "argv":
		return map[string]any{"argv": in.Argv}
	case "files":
		return map[string]any{"count": len(in.Files)}
	case "untracked":
		return map[string]any{"count": len(in.Untracked)}
	case "tools":
		commands := make([]string, 0, len(in.Tools))
		for _, t := range in.Tools {
			commands = append(commands, t.Command)
		}
		return map[string]any{"commands": commands}
	case "env":
		names := make([]string, 0, len(in.Env))
		for _, v := range in.Env {
			names = append(names, v.Name)
		}
		return map[string]any{"names": names}
	case "platform":
		if in.Platform != "" {
			return map[string]any{"platform": in.Platform}
		}
	}
	return nil
}

// Parse decodes raw and checks that it is a statement of a pass: an in-toto
// Statement v1 whose single, named subject has k as its sha256 digest, with a
// Test Result predicate whose result is PASSED. Unrecognised fields are
// ignored, as the in-toto parsing rules require.
func Parse(raw []byte, k string) (Statement, error) {
	var s Statement
	if err := json.Unmarshal(raw, &s); err != nil {
		return Statement{}, fmt.Errorf("not a statement: %w", err)
	}
	switch {
	case s.Type != Type:
		return Statement{}, fmt.Errorf("_type is %q, not %q", s.Type, Type)
	case len(s.Subject) != 1:
		return Statement{}, fmt.Errorf("%d subjects, not one", len(s.Subject))
	case s.Subject[0].Digest["sha256"] != k:
		return Statement{}, errors.New("the subject's sha256 digest is not the key")
	case s.Subject[0].Name == "":
		return Statement{}, errors.New("the subject has no name")
	case s.PredicateType != PredicateType:
		return Statement{}, fmt.Errorf("predicateType is %q, not %q", s.PredicateType, PredicateType)
	case s.Predicate.Result != Passed:
		return Statement{}, fmt.Errorf("result is %q, not %q", s.Predicate.Result, Passed)
	case s.Predicate.Configuration == nil:
		return Statement{}, errors.New("the predicate has no configuration")
	}
	for _, rd := range slices.Concat(s.Subject, s.Predicate.Configuration) {
		if rd.URI == "" && len(rd.Digest) == 0 && len(rd.Content) == 0 {
			return Statement{}, errors.New("a resource descriptor has no uri, digest or content")
		}
	}
	return s, nil
}
