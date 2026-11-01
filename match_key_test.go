package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMatchKeyValues(t *testing.T) {
	var filters keyValueFilters
	if err := filters.Set("meta.team=infra"); err != nil {
		t.Fatal(err)
	}
	if err := filters.Set("status=raw"); err != nil {
		t.Fatal(err)
	}

	candidate := json.RawMessage(`{"type":"job","status":"raw","meta":{"team":"infra","priority":2}}`)
	ok, err := matchKeyValues(filters, candidate)
	if err != nil || !ok {
		t.Fatalf("matchKeyValues = %v, %v; want true, nil", ok, err)
	}

	filters2 := keyValueFilters{}
	filters2.Set("meta.team=frontend")
	ok, err = matchKeyValues(filters2, candidate)
	if err != nil || ok {
		t.Fatalf("matchKeyValues mismatch = %v, %v; want false", ok, err)
	}
}

func TestMatchKeyValuesScalarsAndTypes(t *testing.T) {
	candidate := json.RawMessage(`{
		"name": "worker-1",
		"count": 42,
		"ratio": 3.14,
		"active": true,
		"paused": false,
		"ref": null,
		"nested": {
			"deep": {
				"num": 100,
				"flag": true
			}
		}
	}`)

	tests := []struct {
		name    string
		filter  string
		wantOk  bool
		wantErr bool
	}{
		{"match integer", "count=42", true, false},
		{"mismatch integer", "count=43", false, false},
		{"match float", "ratio=3.14", true, false},
		{"match bool true", "active=true", true, false},
		{"mismatch bool true", "active=false", false, false},
		{"match bool false", "paused=false", true, false},
		{"match null", "ref=null", true, false},
		{"match raw string", "name=worker-1", true, false},
		{"match deep number", "nested.deep.num=100", true, false},
		{"match deep bool", "nested.deep.flag=true", true, false},
		{"non-existent path root", "missing=foo", false, false},
		{"non-existent nested path", "nested.missing=foo", false, false},
		{"path through non-map", "count.sub=foo", false, false},
		{"type distinction string vs number", "name=42", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f keyValueFilters
			if err := f.Set(tt.filter); err != nil {
				t.Fatalf("Set(%q) error: %v", tt.filter, err)
			}
			gotOk, err := matchKeyValues(f, candidate)
			if (err != nil) != tt.wantErr {
				t.Fatalf("matchKeyValues() error = %v, wantErr %v", err, tt.wantErr)
			}
			if gotOk != tt.wantOk {
				t.Fatalf("matchKeyValues(%q) = %v, want %v", tt.filter, gotOk, tt.wantOk)
			}
		})
	}
}

func TestKeyValueFiltersInvalidSyntax(t *testing.T) {
	invalid := []string{
		"no-equals-sign",
		"justkey",
		"",
	}
	for _, raw := range invalid {
		var f keyValueFilters
		if err := f.Set(raw); err == nil {
			t.Fatalf("expected error for Set(%q), got nil", raw)
		}
	}
}

func TestKeyValueFiltersStringRepresentation(t *testing.T) {
	var f keyValueFilters
	f.Set("a=1")
	f.Set("b=two")
	str := f.String()
	if !strings.Contains(str, "a=1") || !strings.Contains(str, "b=two") {
		t.Fatalf("unexpected String() representation: %q", str)
	}
}

