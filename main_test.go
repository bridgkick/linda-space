package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
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

func TestConcurrentAtomicMutationsUnderLock(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "concurrent_space.json")
	space := tupleSpace{path: spacePath}

	const workers = 10
	const opsPerWorker = 20

	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func(workerID int) {
			for j := 0; j < opsPerWorker; j++ {
				tuple := json.RawMessage([]byte(`{"worker":` + string(rune('0'+workerID%10)) + `}`))
				if err := space.mutate(func(state *fileState) error {
					state.Tuples = append(state.Tuples, tuple)
					return nil
				}); err != nil {
					errCh <- err
					return
				}
			}
			errCh <- nil
		}(i)
	}

	for i := 0; i < workers; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("worker failed: %v", err)
		}
	}

	count, err := space.count(nil, nil)
	if err != nil {
		t.Fatalf("count failed: %v", err)
	}
	if count != workers*opsPerWorker {
		t.Fatalf("expected %d tuples, got %d", workers*opsPerWorker, count)
	}
}

func TestStoragePathResolution(t *testing.T) {
	origEnv, hadEnv := os.LookupEnv("SPACE_FILE")
	defer func() {
		if hadEnv {
			os.Setenv("SPACE_FILE", origEnv)
		} else {
			os.Unsetenv("SPACE_FILE")
		}
	}()

	t.Run("default fallback", func(t *testing.T) {
		os.Unsetenv("SPACE_FILE")
		var f commandFlags
		f.resolveSpace()
		if f.space != defaultSpacePath {
			t.Fatalf("expected default %q, got %q", defaultSpacePath, f.space)
		}
	})

	t.Run("SPACE_FILE env var used when no flag", func(t *testing.T) {
		os.Setenv("SPACE_FILE", "/env/path/space.json")
		var f commandFlags
		f.resolveSpace()
		if f.space != "/env/path/space.json" {
			t.Fatalf("expected env path %q, got %q", "/env/path/space.json", f.space)
		}
	})

	t.Run("flag takes priority over SPACE_FILE env var", func(t *testing.T) {
		os.Setenv("SPACE_FILE", "/env/path/space.json")
		f := commandFlags{space: "/flag/path/space.json"}
		f.resolveSpace()
		if f.space != "/flag/path/space.json" {
			t.Fatalf("expected flag path %q, got %q", "/flag/path/space.json", f.space)
		}
	})

	t.Run("flag takes priority over default", func(t *testing.T) {
		os.Unsetenv("SPACE_FILE")
		f := commandFlags{space: "/flag/path/space.json"}
		f.resolveSpace()
		if f.space != "/flag/path/space.json" {
			t.Fatalf("expected flag path %q, got %q", "/flag/path/space.json", f.space)
		}
	})
}

func TestVerboseOutput(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "verbose_space.json")

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	err := runOut([]string{"-v", "--space", spacePath, `{"msg":"hello"}`})
	w.Close()
	os.Stderr = oldStderr

	if err != nil {
		t.Fatalf("runOut failed: %v", err)
	}

	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])
	expected := "storage: " + spacePath + "\n"
	if output != expected {
		t.Fatalf("expected stderr output %q, got %q", expected, output)
	}

	// Also test --verbose long flag
	r2, w2, _ := os.Pipe()
	os.Stderr = w2

	err = runOut([]string{"--verbose", "--space", spacePath, `{"msg":"world"}`})
	w2.Close()
	os.Stderr = oldStderr

	if err != nil {
		t.Fatalf("runOut with --verbose failed: %v", err)
	}

	buf2 := make([]byte, 1024)
	n2, _ := r2.Read(buf2)
	output2 := string(buf2[:n2])
	if output2 != expected {
		t.Fatalf("expected stderr output %q, got %q", expected, output2)
	}
}

func TestSpaceFileEnvVarEndToEnd(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "env_space.json")
	origEnv, hadEnv := os.LookupEnv("SPACE_FILE")
	defer func() {
		if hadEnv {
			os.Setenv("SPACE_FILE", origEnv)
		} else {
			os.Unsetenv("SPACE_FILE")
		}
	}()
	os.Setenv("SPACE_FILE", spacePath)

	// runOut without --space flag should use SPACE_FILE
	if err := runOut([]string{`{"env":"tested"}`}); err != nil {
		t.Fatalf("runOut using SPACE_FILE failed: %v", err)
	}

	// Verify tuple space file was created at spacePath
	if _, err := os.Stat(spacePath); os.IsNotExist(err) {
		t.Fatalf("expected file to exist at %s, but does not", spacePath)
	}

	// runTake without --space flag should read from SPACE_FILE
	oldStdout := os.Stdout
	rOut, wOut, _ := os.Pipe()
	os.Stdout = wOut

	err := runTake([]string{"-k", "env=tested"}, false)
	wOut.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runTake failed: %v", err)
	}

	buf := make([]byte, 1024)
	n, _ := rOut.Read(buf)
	if string(buf[:n]) != `{"env":"tested"}`+"\n" {
		t.Fatalf("unexpected output: %s", string(buf[:n]))
	}

	// Explicit --space should override SPACE_FILE
	overridePath := filepath.Join(t.TempDir(), "override_space.json")
	if err := runOut([]string{"--space", overridePath, `{"override":true}`}); err != nil {
		t.Fatalf("runOut with override space failed: %v", err)
	}
	if _, err := os.Stat(overridePath); os.IsNotExist(err) {
		t.Fatalf("expected override file to exist at %s, but does not", overridePath)
	}
}

func captureOutput(fn func() error) (string, error) {
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	os.Stdout = w

	cmdErr := fn()
	w.Close()
	os.Stdout = oldStdout

	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return string(data), cmdErr
}

func TestEnvelopeWrappingAndShortHexID(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "env_space.json")
	hexRegex := regexp.MustCompile(`^sys-[0-9a-f]{8}$`)

	// 1. out with --envelope should print the envelope JSON
	outJSON, err := captureOutput(func() error {
		return runOut([]string{"--space", spacePath, "-e", `{"task":"compute","val":42}`})
	})
	if err != nil {
		t.Fatalf("runOut -e failed: %v", err)
	}

	var rec TupleRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(outJSON)), &rec); err != nil {
		t.Fatalf("failed to parse envelope output %q: %v", outJSON, err)
	}

	if !hexRegex.MatchString(rec.System.UUID) {
		t.Fatalf("UUID %q does not match expected format ^sys-[0-9a-f]{8}$", rec.System.UUID)
	}
	if rec.System.CreatedAt == "" {
		t.Fatal("expected non-empty created_at")
	}
	if rec.System.LeaseExpires != nil {
		t.Fatalf("expected nil lease_expires, got %v", *rec.System.LeaseExpires)
	}
	if rec.System.ExpiresAt != nil {
		t.Fatalf("expected nil expires_at, got %v", *rec.System.ExpiresAt)
	}
	if rec.System.Hidden {
		t.Fatal("expected hidden to be false")
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		t.Fatalf("failed to parse payload: %v", err)
	}
	if payload["task"] != "compute" || payload["val"] != float64(42) {
		t.Fatalf("unexpected payload: %v", payload)
	}

	// 2. Deposit multiple and verify unique UUIDs
	seenUUIDs := map[string]bool{rec.System.UUID: true}
	for i := 0; i < 20; i++ {
		outI, err := captureOutput(func() error {
			return runOut([]string{"--space", spacePath, "--envelope", fmt.Sprintf(`{"item":%d}`, i)})
		})
		if err != nil {
			t.Fatalf("runOut --envelope item %d failed: %v", i, err)
		}
		var r TupleRecord
		if err := json.Unmarshal([]byte(strings.TrimSpace(outI)), &r); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if !hexRegex.MatchString(r.System.UUID) {
			t.Fatalf("invalid UUID %q", r.System.UUID)
		}
		if seenUUIDs[r.System.UUID] {
			t.Fatalf("duplicate UUID generated: %s", r.System.UUID)
		}
		seenUUIDs[r.System.UUID] = true
	}
}

func TestEnvelopeFlagAndPayloadOutput(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "flag_space.json")

	// out without -e should output nothing
	outNoEnv, err := captureOutput(func() error {
		return runOut([]string{"--space", spacePath, `{"msg":"hello","code":100}`})
	})
	if err != nil {
		t.Fatalf("runOut failed: %v", err)
	}
	if strings.TrimSpace(outNoEnv) != "" {
		t.Fatalf("expected empty output for out without -e, got %q", outNoEnv)
	}

	// rd without -e should return only inner payload
	rdPayload, err := captureOutput(func() error {
		return runTake([]string{"--space", spacePath, `{"msg":"hello"}`}, false)
	})
	if err != nil {
		t.Fatalf("rd failed: %v", err)
	}
	var mRd map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(rdPayload)), &mRd); err != nil {
		t.Fatalf("failed to unmarshal rd payload: %v", err)
	}
	if mRd["msg"] != "hello" || mRd["code"] != float64(100) {
		t.Fatalf("unexpected rd payload: %v", mRd)
	}

	// rd with -e should return full envelope
	rdEnv, err := captureOutput(func() error {
		return runTake([]string{"--space", spacePath, "-e", `{"msg":"hello"}`}, false)
	})
	if err != nil {
		t.Fatalf("rd -e failed: %v", err)
	}
	var rec TupleRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(rdEnv)), &rec); err != nil {
		t.Fatalf("failed to unmarshal envelope from rd: %v", err)
	}
	if rec.System.UUID == "" || !strings.HasPrefix(rec.System.UUID, "sys-") {
		t.Fatalf("expected sys- uuid, got %q", rec.System.UUID)
	}

	// in without -e should return only payload and remove tuple
	inPayload, err := captureOutput(func() error {
		return runTake([]string{"--space", spacePath, `{"msg":"hello"}`}, true)
	})
	if err != nil {
		t.Fatalf("in failed: %v", err)
	}
	var mIn map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(inPayload)), &mIn); err != nil {
		t.Fatalf("failed to unmarshal in payload: %v", err)
	}
	if mIn["msg"] != "hello" || mIn["code"] != float64(100) {
		t.Fatalf("unexpected in payload: %v", mIn)
	}

	// Space should now be empty
	space := tupleSpace{path: spacePath}
	count, err := space.count(nil, nil)
	if err != nil || count != 0 {
		t.Fatalf("expected count 0 after in, got %d, err %v", count, err)
	}
}

func TestPassiveReadFilteringAndExpiration(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "expire_space.json")

	// Deposit a tuple expiring in 1s
	if err := runOut([]string{"--space", spacePath, "--expires", "1s", `{"temp":"short"}`}); err != nil {
		t.Fatalf("out with expires failed: %v", err)
	}
	// Deposit a permanent tuple
	if err := runOut([]string{"--space", spacePath, `{"temp":"permanent"}`}); err != nil {
		t.Fatalf("out permanent failed: %v", err)
	}

	space := tupleSpace{path: spacePath}
	// Immediately both are available
	count, err := space.count(nil, nil)
	if err != nil || count != 2 {
		t.Fatalf("initial count = %d, want 2", count)
	}

	// Wait for expiration
	time.Sleep(1100 * time.Millisecond)

	// count should now passively ignore the expired tuple (returns 1)
	count, err = space.count(nil, nil)
	if err != nil || count != 1 {
		t.Fatalf("count after expiry = %d, want 1", count)
	}

	// rd for the expired tuple should not find it
	_, err = captureOutput(func() error {
		return runTake([]string{"--space", spacePath, "--fail-on-none", `{"temp":"short"}`}, false)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("expected errNoMatch reading expired tuple, got %v", err)
	}

	// in for the expired tuple should also not find it
	_, err = captureOutput(func() error {
		return runTake([]string{"--space", spacePath, "--fail-on-none", `{"temp":"short"}`}, true)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("expected errNoMatch taking expired tuple, got %v", err)
	}

	// Crucial check: verify file still contains 2 records because reads NEVER mutate inline!
	fileState, err := space.readFileState()
	if err != nil {
		t.Fatalf("readFileState failed: %v", err)
	}
	if len(fileState.Tuples) != 2 {
		t.Fatalf("expected 2 tuples in raw file (no inline mutation), got %d", len(fileState.Tuples))
	}

	// Run sweep to explicitly clean up expired records
	sweepOut, err := captureOutput(func() error {
		return runSweep([]string{"--space", spacePath})
	})
	if err != nil {
		t.Fatalf("sweep failed: %v", err)
	}
	var res SweepResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(sweepOut)), &res); err != nil {
		t.Fatalf("unmarshal sweep result failed: %v", err)
	}
	if res.PurgedExpired != 1 || res.Reinflated != 0 {
		t.Fatalf("unexpected sweep result: %+v, want purged=1, reinflated=0", res)
	}

	// Now raw file should have only 1 tuple
	fileState, err = space.readFileState()
	if err != nil {
		t.Fatalf("readFileState failed: %v", err)
	}
	if len(fileState.Tuples) != 1 {
		t.Fatalf("expected 1 tuple in raw file after sweep, got %d", len(fileState.Tuples))
	}
}

func (s tupleSpace) readFileState() (*fileState, error) {
	var state *fileState
	err := withLockedFile(s.path, false, func(f *os.File, st *fileState) error {
		state = st
		return nil
	})
	return state, err
}

func TestLeasedClaimsIn(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "lease_space.json")

	// Deposit tuple
	if err := runOut([]string{"--space", spacePath, `{"task":"compile","status":"pending"}`}); err != nil {
		t.Fatalf("out failed: %v", err)
	}

	// Claim with in --envelope --lease 60s
	claimOut, err := captureOutput(func() error {
		return runTake([]string{"--space", spacePath, "--envelope", "--lease", "60s", `{"task":"compile"}`}, true)
	})
	if err != nil {
		t.Fatalf("in --lease failed: %v", err)
	}

	var rec TupleRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(claimOut)), &rec); err != nil {
		t.Fatalf("failed to unmarshal claim envelope: %v", err)
	}
	if !rec.System.Hidden {
		t.Fatal("expected claimed tuple to have hidden = true")
	}
	if rec.System.LeaseExpires == nil {
		t.Fatal("expected claimed tuple to have lease_expires set")
	}

	// Tuple is now hidden, count should be 0
	space := tupleSpace{path: spacePath}
	count, err := space.count(nil, nil)
	if err != nil || count != 0 {
		t.Fatalf("count of leased item = %d, want 0", count)
	}

	// rd should not find it
	_, err = captureOutput(func() error {
		return runTake([]string{"--space", spacePath, "--fail-on-none", `{"task":"compile"}`}, false)
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("expected errNoMatch for leased tuple, got %v", err)
	}

	// File still contains the tuple
	st, err := space.readFileState()
	if err != nil || len(st.Tuples) != 1 {
		t.Fatalf("expected 1 tuple in storage file, got %d, err %v", len(st.Tuples), err)
	}
}

func TestUpdateLeaseHeartbeatAndPayload(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "update_space.json")

	// Deposit and claim
	outEnv, err := captureOutput(func() error {
		return runOut([]string{"--space", spacePath, "-e", `{"task":"compile","status":"pending"}`})
	})
	if err != nil {
		t.Fatalf("out failed: %v", err)
	}
	var initRec TupleRecord
	json.Unmarshal([]byte(strings.TrimSpace(outEnv)), &initRec)
	uuid := initRec.System.UUID

	// Claim with lease 30s
	_, err = captureOutput(func() error {
		return runTake([]string{"--space", spacePath, "--lease", "30s", "--uuid", uuid}, true)
	})
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// 1. Heartbeat / extend lease
	hbOut, err := captureOutput(func() error {
		return runUpdate([]string{"--space", spacePath, "-e", "--uuid", uuid, "--lease", "+120s"})
	})
	if err != nil {
		t.Fatalf("update --lease failed: %v", err)
	}
	var hbRec TupleRecord
	json.Unmarshal([]byte(strings.TrimSpace(hbOut)), &hbRec)
	if !hbRec.System.Hidden {
		t.Fatal("expected hidden=true after heartbeat")
	}
	if hbRec.System.LeaseExpires == nil {
		t.Fatal("expected lease_expires set after heartbeat")
	}

	// 2. Modify payload & retain lease
	modOut, err := captureOutput(func() error {
		return runUpdate([]string{"--space", spacePath, "-e", "--uuid", uuid, "--json", `{"task":"compile","status":"running","progress":50}`})
	})
	if err != nil {
		t.Fatalf("update --json failed: %v", err)
	}
	var modRec TupleRecord
	json.Unmarshal([]byte(strings.TrimSpace(modOut)), &modRec)
	if !modRec.System.Hidden {
		t.Fatal("expected hidden=true retained during payload modification")
	}
	if modRec.System.LeaseExpires == nil || *modRec.System.LeaseExpires != *hbRec.System.LeaseExpires {
		t.Fatalf("expected lease_expires preserved, got %v", modRec.System.LeaseExpires)
	}
	var p map[string]any
	json.Unmarshal(modRec.Payload, &p)
	if p["status"] != "running" || p["progress"] != float64(50) {
		t.Fatalf("unexpected payload: %v", p)
	}

	// 3. Early release back to queue
	relOut, err := captureOutput(func() error {
		return runUpdate([]string{"--space", spacePath, "-e", "--uuid", uuid, "--release", "--json", `{"task":"compile","status":"reviewing"}`})
	})
	if err != nil {
		t.Fatalf("update --release failed: %v", err)
	}
	var relRec TupleRecord
	json.Unmarshal([]byte(strings.TrimSpace(relOut)), &relRec)
	if relRec.System.Hidden {
		t.Fatal("expected hidden=false after release")
	}
	if relRec.System.LeaseExpires != nil {
		t.Fatalf("expected lease_expires=nil after release, got %v", *relRec.System.LeaseExpires)
	}

	// Now it should be visible to regular count and rd
	space := tupleSpace{path: spacePath}
	count, err := space.count(nil, nil)
	if err != nil || count != 1 {
		t.Fatalf("expected count 1 after release, got %d, err %v", count, err)
	}

	rdOut, err := captureOutput(func() error {
		return runTake([]string{"--space", spacePath, `{"status":"reviewing"}`}, false)
	})
	if err != nil {
		t.Fatalf("rd failed: %v", err)
	}
	if !strings.Contains(rdOut, `"reviewing"`) {
		t.Fatalf("unexpected rd output: %s", rdOut)
	}
}

func TestUpdateByTemplate(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "tmpl_space.json")

	if err := runOut([]string{"--space", spacePath, `{"job":"email","status":"queued"}`}); err != nil {
		t.Fatalf("out failed: %v", err)
	}

	upOut, err := captureOutput(func() error {
		return runUpdate([]string{"--space", spacePath, "--template", `{"job":"email"}`, "--json", `{"job":"email","status":"sending"}`})
	})
	if err != nil {
		t.Fatalf("update by template failed: %v", err)
	}
	if strings.TrimSpace(upOut) != `{"job":"email","status":"sending"}` {
		t.Fatalf("unexpected update output: %q", upOut)
	}

	// Verify via rd
	rdOut, err := captureOutput(func() error {
		return runTake([]string{"--space", spacePath, `{"status":"sending"}`}, false)
	})
	if err != nil {
		t.Fatalf("rd failed: %v", err)
	}
	if strings.TrimSpace(rdOut) != `{"job":"email","status":"sending"}` {
		t.Fatalf("unexpected rd: %q", rdOut)
	}
}

func TestDeleteByUUID(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "del_space.json")

	outEnv, err := captureOutput(func() error {
		return runOut([]string{"--space", spacePath, "-e", `{"task":"cleanup","status":"done"}`})
	})
	if err != nil {
		t.Fatalf("out failed: %v", err)
	}
	var rec TupleRecord
	json.Unmarshal([]byte(strings.TrimSpace(outEnv)), &rec)
	uuid := rec.System.UUID

	// Lease it first (making it hidden)
	_, err = captureOutput(func() error {
		return runTake([]string{"--space", spacePath, "--lease", "60s", "--uuid", uuid}, true)
	})
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// delete --uuid should permanently remove even hidden leased tuple
	delOut, err := captureOutput(func() error {
		return runDelete([]string{"--space", spacePath, "--uuid", uuid})
	})
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	var mDel map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(delOut)), &mDel); err != nil {
		t.Fatalf("failed to unmarshal delete output: %v", err)
	}
	if mDel["task"] != "cleanup" || mDel["status"] != "done" {
		t.Fatalf("unexpected delete output: %v", mDel)
	}

	// Verify space is completely empty
	space := tupleSpace{path: spacePath}
	st, err := space.readFileState()
	if err != nil || len(st.Tuples) != 0 {
		t.Fatalf("expected 0 tuples after delete, got %d, err %v", len(st.Tuples), err)
	}

	// Second delete on same UUID should return errNoMatch
	_, err = captureOutput(func() error {
		return runDelete([]string{"--space", spacePath, "--uuid", uuid})
	})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("expected errNoMatch deleting non-existent tuple, got %v", err)
	}
}

func TestSweepMaintenance(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "sweep_space.json")

	// 1. Tuple with short expiration (1s)
	if err := runOut([]string{"--space", spacePath, "--expires", "1s", `{"kind":"expires_soon"}`}); err != nil {
		t.Fatalf("out 1 failed: %v", err)
	}

	// 2. Tuple claimed with short lease (1s)
	out2, err := captureOutput(func() error {
		return runOut([]string{"--space", spacePath, "-e", `{"kind":"leased_then_abandoned"}`})
	})
	if err != nil {
		t.Fatalf("out 2 failed: %v", err)
	}
	var r2 TupleRecord
	json.Unmarshal([]byte(strings.TrimSpace(out2)), &r2)
	if _, err := captureOutput(func() error {
		return runTake([]string{"--space", spacePath, "--lease", "1s", "--uuid", r2.System.UUID}, true)
	}); err != nil {
		t.Fatalf("claim 2 failed: %v", err)
	}

	// 3. Normal tuple that stays
	if err := runOut([]string{"--space", spacePath, `{"kind":"permanent"}`}); err != nil {
		t.Fatalf("out 3 failed: %v", err)
	}

	// Wait for both lease and absolute expiration to lapse
	time.Sleep(1100 * time.Millisecond)

	// Run sweep
	sweepOut, err := captureOutput(func() error {
		return runSweep([]string{"--space", spacePath})
	})
	if err != nil {
		t.Fatalf("sweep failed: %v", err)
	}

	var res SweepResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(sweepOut)), &res); err != nil {
		t.Fatalf("unmarshal sweep failed: %v", err)
	}
	if res.PurgedExpired != 1 || res.Reinflated != 1 {
		t.Fatalf("expected purged=1, reinflated=1; got %+v", res)
	}

	// Reinflated tuple should now be available again to count (now 2 available: reinflated + permanent)
	space := tupleSpace{path: spacePath}
	count, err := space.count(nil, nil)
	if err != nil || count != 2 {
		t.Fatalf("count after sweep = %d, want 2", count)
	}

	// We can read the reinflated tuple
	rdOut, err := captureOutput(func() error {
		return runTake([]string{"--space", spacePath, `{"kind":"leased_then_abandoned"}`}, false)
	})
	if err != nil {
		t.Fatalf("rd reinflated tuple failed: %v", err)
	}
	if !strings.Contains(rdOut, "leased_then_abandoned") {
		t.Fatalf("unexpected rd: %s", rdOut)
	}
}

func TestClipWithPassiveFiltering(t *testing.T) {
	spacePath := filepath.Join(t.TempDir(), "clip_space.json")

	// 1. Active available tuple
	if err := runOut([]string{"--space", spacePath, `{"log":"sys","id":1}`}); err != nil {
		t.Fatalf("out 1 failed: %v", err)
	}
	// 2. Active tuple that gets leased
	out2, _ := captureOutput(func() error {
		return runOut([]string{"--space", spacePath, "-e", `{"log":"sys","id":2}`})
	})
	var r2 TupleRecord
	json.Unmarshal([]byte(strings.TrimSpace(out2)), &r2)
	captureOutput(func() error {
		return runTake([]string{"--space", spacePath, "--lease", "60s", "--uuid", r2.System.UUID}, true)
	})
	// 3. Expired tuple (1s)
	runOut([]string{"--space", spacePath, "--expires", "1s", `{"log":"sys","id":3}`})

	time.Sleep(1100 * time.Millisecond)

	// Clip should only remove matching AVAILABLE tuples (id:1)
	// It must NOT remove leased (hidden) id:2 or passively expired id:3
	clipOut, err := captureOutput(func() error {
		return runClip([]string{"--space", spacePath, `{"log":"sys"}`})
	})
	if err != nil {
		t.Fatalf("clip failed: %v", err)
	}
	if strings.TrimSpace(clipOut) != "1" {
		t.Fatalf("expected clip to remove 1 tuple, got %q", clipOut)
	}

	// Verify id:2 is still in the file (still leased)
	space := tupleSpace{path: spacePath}
	st, err := space.readFileState()
	if err != nil {
		t.Fatalf("readFileState failed: %v", err)
	}
	if len(st.Tuples) != 2 {
		t.Fatalf("expected 2 tuples remaining in file, got %d", len(st.Tuples))
	}
}



