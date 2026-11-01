// Command linda-space provides a small, local, file-backed Linda tuple space.
//
// The command intentionally has no network daemon or dependencies: multiple
// processes coordinate through an advisory flock around one JSON file.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
)

const (
	defaultSpacePath = "linda-space.json"
	pollInterval     = 100 * time.Millisecond
)

var (
	errNoMatch    = errors.New("no matching tuple")
	errBadTuple   = errors.New("tuple must be a JSON object or array")
	errBadPattern = errors.New("template must be a JSON object or array")
)

type tupleSpace struct {
	path string
}

type fileState struct {
	Tuples []json.RawMessage `json:"tuples"`
}

type commandFlags struct {
	space     string
	input     string
	inputFile string
	wait      time.Duration
	fail      bool
	pretty    bool
	keys      keyValueFilters
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "--help" || os.Args[1] == "-h" {
		printUsage(os.Stdout)
		return
	}

	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "out":
		err = runOut(args)
	case "in":
		err = runTake(args, true)
	case "rd", "read":
		err = runTake(args, false)
	case "count":
		err = runCount(args)
	case "clip":
		err = runClip(args)
	case "help":
		printUsage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "linda-space: unknown command %q\n\n", cmd)
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "linda-space: %v\n", err)
		os.Exit(1)
	}
}

func runOut(args []string) error {
	fs := flag.NewFlagSet("out", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var f commandFlags
	addCommonFlags(fs, &f)
	if err := fs.Parse(args); err != nil {
		return err
	}
	payload, err := readJSONInput(fs.Args(), f.input, f.inputFile, true)
	if err != nil {
		return err
	}
	if !isTuple(payload) {
		return errBadTuple
	}
	space := tupleSpace{path: f.space}
	return space.mutate(func(state *fileState) error {
		state.Tuples = append(state.Tuples, cloneRaw(payload))
		return nil
	})
}

func runTake(args []string, remove bool) error {
	name := "rd"
	if remove {
		name = "in"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var f commandFlags
	addCommonFlags(fs, &f)
	fs.DurationVar(&f.wait, "wait", 0, "wait up to this duration for a match (0 means do not wait; use 0s for a non-blocking check)")
	fs.BoolVar(&f.fail, "fail-on-none", false, "exit 1 when no matching tuple is available before --wait expires")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pattern, err := resolvePattern(fs.Args(), &f, false)
	if err != nil {
		return err
	}
	space := tupleSpace{path: f.space}
	ctx := context.Background()
	if f.wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.wait)
		defer cancel()
	}
	for {
		result, found, err := space.find(ctx, pattern, f.keys, remove)
		if err != nil {
			return err
		}
		if found {
			return writeJSON(os.Stdout, result, f.pretty)
		}
		if f.wait <= 0 {
			if f.fail {
				return errNoMatch
			}
			return nil
		}
		if err := sleepPoll(ctx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				if f.fail {
					return errNoMatch
				}
				return nil
			}
			return err
		}
	}
}

func runCount(args []string) error {
	fs := flag.NewFlagSet("count", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var f commandFlags
	addCommonFlags(fs, &f)
	if err := fs.Parse(args); err != nil {
		return err
	}
	pattern, err := resolvePattern(fs.Args(), &f, true)
	if err != nil {
		return err
	}
	space := tupleSpace{path: f.space}
	count, err := space.count(pattern, f.keys)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "%d\n", count)
	return err
}

func runClip(args []string) error {
	fs := flag.NewFlagSet("clip", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var f commandFlags
	addCommonFlags(fs, &f)
	fs.BoolVar(&f.fail, "fail-on-none", false, "exit 1 when no matching tuple is available")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pattern, err := resolvePattern(fs.Args(), &f, true)
	if err != nil {
		return err
	}
	space := tupleSpace{path: f.space}
	count, err := space.clip(pattern, f.keys)
	if err != nil {
		return err
	}
	if count == 0 && f.fail {
		return errNoMatch
	}
	_, err = fmt.Fprintf(os.Stdout, "%d\n", count)
	return err
}

func addCommonFlags(fs *flag.FlagSet, f *commandFlags) {
	fs.StringVar(&f.space, "space", defaultSpacePath, "path to the shared JSON tuple-space file")
	fs.StringVar(&f.input, "json", "", "JSON tuple/template; alternatively provide it as the final argument or on stdin")
	fs.StringVar(&f.inputFile, "file", "", "read the JSON tuple/template from this file")
	fs.BoolVar(&f.pretty, "pretty", false, "pretty-print JSON output")
	fs.Var(&f.keys, "k", "dot-path key-value filter (e.g. -k status=raw -k meta.team=infra); repeatable")
	fs.Var(&f.keys, "key", "dot-path key-value filter (e.g. --key status=raw); repeatable")
}

func resolvePattern(args []string, f *commandFlags, allowEmpty bool) (json.RawMessage, error) {
	if len(args) == 0 && f.input == "" && f.inputFile == "" {
		if allowEmpty && len(f.keys) > 0 {
			return nil, nil
		}
		if allowEmpty {
			return nil, nil
		}
	}
	pattern, err := readJSONInput(args, f.input, f.inputFile, false)
	if err != nil {
		if allowEmpty && len(f.keys) > 0 && (len(args) == 0 && f.input == "" && f.inputFile == "") {
			return nil, nil
		}
		return nil, err
	}
	if pattern != nil && !isTuple(pattern) {
		return nil, errBadPattern
	}
	return pattern, nil
}

func readJSONInput(args []string, inline, filename string, allowStdin bool) (json.RawMessage, error) {
	if inline != "" && (len(args) > 0 || filename != "") {
		return nil, errors.New("use only one of positional JSON, --json, or --file")
	}
	if filename != "" {
		data, err := os.ReadFile(filename)
		if err != nil {
			return nil, fmt.Errorf("read JSON file: %w", err)
		}
		return normalizeJSON(data)
	}
	if inline != "" {
		return normalizeJSON([]byte(inline))
	}
	if len(args) > 1 {
		return nil, errors.New("expected one JSON tuple/template")
	}
	if len(args) == 1 {
		return normalizeJSON([]byte(args[0]))
	}
	if !allowStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			return nil, errors.New("missing JSON tuple/template (use positional JSON, --json, --file, or stdin)")
		}
		return normalizeJSON(data)
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, errors.New("missing JSON tuple (use positional JSON, --json, --file, or stdin)")
	}
	return normalizeJSON(data)
}

func normalizeJSON(data []byte) (json.RawMessage, error) {
	var value any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode JSON: %w", err)
	}
	return canonical, nil
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("expected exactly one JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func isTuple(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch value.(type) {
	case map[string]any, []any:
		return true
	default:
		return false
	}
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

func (s tupleSpace) mutate(fn func(*fileState) error) error {
	return withLockedFile(s.path, true, func(file *os.File, state *fileState) error {
		if err := fn(state); err != nil {
			return err
		}
		return writeState(file, state)
	})
}

func (s tupleSpace) find(ctx context.Context, pattern json.RawMessage, keys keyValueFilters, remove bool) (json.RawMessage, bool, error) {
	var result json.RawMessage
	var found bool
	err := withLockedFile(s.path, remove, func(file *os.File, state *fileState) error {
		for i, candidate := range state.Tuples {
			ok, err := tupleMatches(pattern, keys, candidate)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			result = cloneRaw(candidate)
			found = true
			if remove {
				state.Tuples = append(state.Tuples[:i], state.Tuples[i+1:]...)
				return writeState(file, state)
			}
			return nil
		}
		return nil
	})
	return result, found, err
}

func (s tupleSpace) count(pattern json.RawMessage, keys keyValueFilters) (int, error) {
	count := 0
	err := withLockedFile(s.path, false, func(_ *os.File, state *fileState) error {
		for _, candidate := range state.Tuples {
			ok, err := tupleMatches(pattern, keys, candidate)
			if err != nil {
				return err
			}
			if ok {
				count++
			}
		}
		return nil
	})
	return count, err
}

func (s tupleSpace) clip(pattern json.RawMessage, keys keyValueFilters) (int, error) {
	removed := 0
	err := withLockedFile(s.path, true, func(file *os.File, state *fileState) error {
		kept := state.Tuples[:0]
		for _, candidate := range state.Tuples {
			ok, err := tupleMatches(pattern, keys, candidate)
			if err != nil {
				return err
			}
			if ok {
				removed++
				continue
			}
			kept = append(kept, candidate)
		}
		state.Tuples = kept
		if removed > 0 {
			return writeState(file, state)
		}
		return nil
	})
	return removed, err
}

func tupleMatches(pattern json.RawMessage, keys keyValueFilters, candidate json.RawMessage) (bool, error) {
	if pattern != nil {
		ok, err := matches(pattern, candidate)
		if err != nil || !ok {
			return false, err
		}
	}
	if len(keys) > 0 {
		ok, err := matchKeyValues(keys, candidate)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func withLockedFile(path string, exclusive bool, fn func(*os.File, *fileState) error) error {
	if path == "" {
		return errors.New("space path cannot be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create space directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open space: %w", err)
	}
	defer file.Close()
	lock := syscall.LOCK_SH
	if exclusive {
		lock = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(file.Fd()), lock); err != nil {
		return fmt.Errorf("lock space: %w", err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	state, err := readState(file)
	if err != nil {
		return err
	}
	return fn(file, state)
}

func readState(file *os.File) (*fileState, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek space: %w", err)
	}
	var state fileState
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read space: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return &state, nil
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode space: %w", err)
	}
	for i, tuple := range state.Tuples {
		if !isTuple(tuple) {
			return nil, fmt.Errorf("space contains invalid tuple at index %d", i)
		}
	}
	return &state, nil
}

func writeState(file *os.File, state *fileState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode space: %w", err)
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate space: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek space for write: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write space: %w", err)
	}
	return file.Sync()
}

func matches(pattern, candidate json.RawMessage) (bool, error) {
	var p, c any
	dec := json.NewDecoder(strings.NewReader(string(pattern)))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return false, fmt.Errorf("decode template: %w", err)
	}
	dec = json.NewDecoder(strings.NewReader(string(candidate)))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return false, fmt.Errorf("decode tuple: %w", err)
	}
	return subsetMatch(p, c), nil
}

func subsetMatch(pattern, candidate any) bool {
	switch p := pattern.(type) {
	case map[string]any:
		c, ok := candidate.(map[string]any)
		if !ok {
			return false
		}
		for key, want := range p {
			have, ok := c[key]
			if !ok || !subsetMatch(want, have) {
				return false
			}
		}
		return true
	case []any:
		c, ok := candidate.([]any)
		if !ok || len(p) != len(c) {
			return false
		}
		for i := range p {
			if !subsetMatch(p[i], c[i]) {
				return false
			}
		}
		return true
	default:
		return jsonValueEqual(p, candidate)
	}
}

func jsonValueEqual(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

func sleepPoll(ctx context.Context) error {
	timer := time.NewTimer(pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func writeJSON(w io.Writer, raw json.RawMessage, pretty bool) error {
	if pretty {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s\n", data)
		return err
	}
	_, err := fmt.Fprintf(w, "%s\n", raw)
	return err
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `linda-space — a local, file-backed Linda tuple space

Usage:
  linda-space <command> [options] [JSON]

Commands:
  out      Add one JSON object or array tuple to the space.
  in       Remove and print the first tuple matching a JSON template / key flags.
  rd       Read (without removing) the first matching tuple. Alias: read.
  count    Print the number of tuples, optionally matching a template / key flags.
  clip     Remove all tuples matching a template / key flags and print the number removed.

Matching & Key Flags:
  Templates are recursive subsets. Alternatively or additionally, use repeatable
  -k or --key <dot.path>=<value> flags to match nested fields without JSON templates.
  Example: -k status=raw -k meta.team=infra

Input:
  JSON may be the final argument, --json VALUE, --file PATH, or stdin.
  A tuple is a top-level JSON object or array. The default space is
  ./linda-space.json; use --space PATH to share a different file.

Blocking:
  in and rd check immediately by default. --wait DURATION enables long-polling
  until a matching tuple arrives or the duration expires (for example 30s or
  5m). --fail-on-none makes a timeout/miss exit 1; without it, a miss exits 0
  and prints nothing. in is atomic under the file lock.

Examples:
  linda-space out --space /tmp/jobs.json '{"type":"job","status":"raw"}'
  linda-space in --space /tmp/jobs.json -k status=raw -k meta.team=infra
  linda-space rd --space /tmp/jobs.json --wait 30s --fail-on-none '{"type":"job"}'
  linda-space count --space /tmp/jobs.json -k status=raw
  linda-space clip --space /tmp/jobs.json -k status=done

Exit status:
  0 success (including a non-matching in/rd unless --fail-on-none is set)
  1 invalid input, I/O failure, or --fail-on-none with no match
  2 unknown command or flag syntax error
`)
}
