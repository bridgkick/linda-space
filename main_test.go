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

func TestResolvePattern(t *testing.T) {
	t.Run("empty without flags or pattern rejected when not allowed", func(t *testing.T) {
		f := &commandFlags{}
		_, err := resolvePattern(nil, f, false)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("empty with key flags allowed even when allowEmpty is false", func(t *testing.T) {
		f := &commandFlags{}
		if err := f.keys.Set("foo=bar"); err != nil {
			t.Fatal(err)
		}
		pattern, err := resolvePattern(nil, f, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pattern != nil {
			t.Fatalf("expected nil pattern, got %s", pattern)
		}
	})

	t.Run("empty when allowEmpty is true", func(t *testing.T) {
		f := &commandFlags{}
		pattern, err := resolvePattern(nil, f, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pattern != nil {
			t.Fatalf("expected nil pattern, got %s", pattern)
		}
	})

	t.Run("positional json with key flags", func(t *testing.T) {
		f := &commandFlags{}
		if err := f.keys.Set("meta.team=infra"); err != nil {
			t.Fatal(err)
		}
		pattern, err := resolvePattern([]string{`{"type":"job"}`}, f, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(pattern) != `{"type":"job"}` {
			t.Fatalf("expected {\"type\":\"job\"}, got %s", pattern)
		}
	})

	t.Run("reject non-tuple pattern", func(t *testing.T) {
		f := &commandFlags{}
		_, err := resolvePattern([]string{`"scalar"`}, f, false)
		if !errors.Is(err, errBadPattern) {
			t.Fatalf("expected errBadPattern, got %v", err)
		}
	})
}

func TestTupleMatchesCombinedTemplateAndKeyFilters(t *testing.T) {
	var keys keyValueFilters
	if err := keys.Set("meta.team=infra"); err != nil {
		t.Fatal(err)
	}
	if err := keys.Set("priority=1"); err != nil {
		t.Fatal(err)
	}

	tuple := json.RawMessage(`{"type":"job","status":"pending","priority":1,"meta":{"team":"infra"}}`)

	// Both template and keys match
	ok, err := tupleMatches(json.RawMessage(`{"type":"job"}`), keys, tuple)
	if err != nil || !ok {
		t.Fatalf("expected match, got ok=%v err=%v", ok, err)
	}

	// Template mismatches
	ok, err = tupleMatches(json.RawMessage(`{"type":"other"}`), keys, tuple)
	if err != nil || ok {
		t.Fatalf("expected mismatch on template, got ok=%v err=%v", ok, err)
	}

	// Keys mismatch
	var mismatchKeys keyValueFilters
	mismatchKeys.Set("meta.team=backend")
	ok, err = tupleMatches(json.RawMessage(`{"type":"job"}`), mismatchKeys, tuple)
	if err != nil || ok {
		t.Fatalf("expected mismatch on keys, got ok=%v err=%v", ok, err)
	}

	// No template, only keys
	ok, err = tupleMatches(nil, keys, tuple)
	if err != nil || !ok {
		t.Fatalf("expected match with nil template, got ok=%v err=%v", ok, err)
	}
}

func TestSubcommandsEndToEnd(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "e2e_space.json")

	// 1. out tuples
	if err := runOut([]string{"--space", spacePath, `{"type":"task","status":"open","priority":1,"meta":{"env":"prod"}}`}); err != nil {
		t.Fatalf("out 1 failed: %v", err)
	}
	if err := runOut([]string{"--space", spacePath, `{"type":"task","status":"open","priority":2,"meta":{"env":"dev"}}`}); err != nil {
		t.Fatalf("out 2 failed: %v", err)
	}
	if err := runOut([]string{"--space", spacePath, `{"type":"task","status":"done","priority":1,"meta":{"env":"prod"}}`}); err != nil {
		t.Fatalf("out 3 failed: %v", err)
	}

	// 2. count with -k only
	space := tupleSpace{path: spacePath}
	var fKeyOpen keyValueFilters
	fKeyOpen.Set("status=open")
	count, err := space.count(nil, fKeyOpen)
	if err != nil || count != 2 {
		t.Fatalf("count open = %d, %v; want 2", count, err)
	}

	// count with -k and template
	count, err = space.count(json.RawMessage(`{"type":"task"}`), fKeyOpen)
	if err != nil || count != 2 {
		t.Fatalf("count open with template = %d, %v; want 2", count, err)
	}

	// 3. rd with -k only (no template argument)
	oldStdout := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	err = runTake([]string{"--space", spacePath, "-k", "meta.env=dev"}, false)
	w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatalf("rd with -k failed: %v", err)
	}

	// 4. in with -k only (no template argument)
	_, w, _ = os.Pipe()
	os.Stdout = w
	err = runTake([]string{"--space", spacePath, "-k", "meta.env=dev"}, true)
	w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatalf("in with -k failed: %v", err)
	}

	// Verify it was removed
	count, err = space.count(nil, nil)
	if err != nil || count != 2 {
		t.Fatalf("count after in = %d; want 2", count)
	}

	// 5. clip with -k only
	var fKeyProd keyValueFilters
	fKeyProd.Set("meta.env=prod")
	clipped, err := space.clip(nil, fKeyProd)
	if err != nil || clipped != 2 {
		t.Fatalf("clip = %d, %v; want 2", clipped, err)
	}

	// Verify space is empty
	count, err = space.count(nil, nil)
	if err != nil || count != 0 {
		t.Fatalf("count after clip = %d; want 0", count)
	}
}

