package main

import (
	"encoding/json"
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
