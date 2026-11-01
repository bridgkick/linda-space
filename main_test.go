package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSubsetMatchRecursiveObjectsAndArrays(t *testing.T) {
	tests := []struct {
		name      string
		pattern   string
		candidate string
		want      bool
	}{
		{"object subset", `{"type":"job","meta":{"team":"infra"}}`, `{"type":"job","status":"raw","meta":{"team":"infra","priority":2}}`, true},
		{"missing nested key", `{"meta":{"team":"infra"}}`, `{"meta":{"priority":2}}`, false},
		{"array exact", `["job",{"status":"raw"}]`, `["job",{"status":"raw","n":1}]`, true},
		{"array length matters", `["job"]`, `["job","extra"]`, false},
		{"different scalar types", `{"value":"1"}`, `{"value":1}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := matches(json.RawMessage(test.pattern), json.RawMessage(test.candidate))
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("matches() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSpaceOperations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "space.json")
	space := tupleSpace{path: path}
	for _, tuple := range []string{
		`{"type":"job","status":"raw","meta":{"team":"infra"}}`,
		`{"type":"job","status":"done"}`,
		`["event",{"kind":"created"}]`,
	} {
		if err := space.mutate(func(state *fileState) error {
			state.Tuples = append(state.Tuples, json.RawMessage(tuple))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	count, err := space.count(json.RawMessage(`{"type":"job"}`), nil)
	if err != nil || count != 2 {
		t.Fatalf("count = %d, %v; want 2", count, err)
	}
	got, found, err := space.find(context.Background(), json.RawMessage(`{"status":"raw"}`), nil, false)
	if err != nil || !found {
		t.Fatalf("rd = %s, found=%v, err=%v", got, found, err)
	}
	if string(got) != `{"type":"job","status":"raw","meta":{"team":"infra"}}` {
		t.Fatalf("rd returned %s", got)
	}
	got, found, err = space.find(context.Background(), json.RawMessage(`{"status":"raw"}`), nil, true)
	if err != nil || !found {
		t.Fatalf("in = %s, found=%v, err=%v", got, found, err)
	}
	count, err = space.count(nil, nil)
	if err != nil || count != 2 {
		t.Fatalf("count after in = %d, %v; want 2", count, err)
	}
	removed, err := space.clip(json.RawMessage(`{"type":"job"}`), nil)
	if err != nil || removed != 1 {
		t.Fatalf("clip = %d, %v; want 1", removed, err)
	}
	count, err = space.count(nil, nil)
	if err != nil || count != 1 {
		t.Fatalf("count after clip = %d, %v; want 1", count, err)
	}
}

func TestBlockingFindSeesTupleWrittenByAnotherOperation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "space.json")
	space := tupleSpace{path: path}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan struct {
		raw   json.RawMessage
		found bool
		err   error
	}, 1)
	go func() {
		for {
			raw, found, err := space.find(ctx, json.RawMessage(`{"kind":"ready"}`), nil, true)
			if err != nil || found {
				result <- struct {
					raw   json.RawMessage
					found bool
					err   error
				}{raw, found, err}
				return
			}
			if err := sleepPoll(ctx); err != nil {
				result <- struct {
					raw   json.RawMessage
					found bool
					err   error
				}{nil, false, err}
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if err := space.mutate(func(state *fileState) error {
		state.Tuples = append(state.Tuples, json.RawMessage(`{"kind":"ready","id":7}`))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || !got.found {
			t.Fatalf("blocking find: found=%v err=%v", got.found, got.err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for tuple")
	}
}

func TestReadStateRejectsNonTuple(t *testing.T) {
	path := filepath.Join(t.TempDir(), "space.json")
	if err := os.WriteFile(path, []byte(`{"tuples":[1]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := tupleSpace{path: path}.count(nil, nil)
	if err == nil {
		t.Fatal("expected invalid tuple error")
	}
}

func TestJsonValueEqualKeepsTypesDistinct(t *testing.T) {
	if jsonValueEqual("true", true) {
		t.Fatal("string and bool should not compare equal")
	}
	if !jsonValueEqual(json.Number("1"), json.Number("1")) {
		t.Fatal("equal JSON numbers should compare equal")
	}
	if reflect.DeepEqual("1", 1) {
		t.Fatal("test setup: reflect types should differ")
	}
}

func TestNoMatchErrorIsSentinel(t *testing.T) {
	if !errors.Is(errNoMatch, errNoMatch) {
		t.Fatal("errNoMatch should be a sentinel")
	}
}
