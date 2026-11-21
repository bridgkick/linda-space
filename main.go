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
	spaceEnvVar      = "SPACE_FILE"
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
	verbose   bool
	input     string
	inputFile string
	wait      time.Duration
	fail      bool
	pretty    bool
	keys      keyValueFilters
	envelope  bool
	expires   string
	lease     string
	release   bool
	uuid      string
	template  string
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
	case "update":
		err = runUpdate(args)
	case "delete":
		err = runDelete(args)
	case "sweep":
		err = runSweep(args)
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
	fs.StringVar(&f.expires, "expires", "", "tuple expiration duration (e.g. 2h, 30m) or absolute ISO timestamp")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f.resolveSpace()
	f.logVerbose()
	payload, err := readJSONInput(fs.Args(), f.input, f.inputFile, true)
	if err != nil {
		return err
	}
	if !isTuple(payload) {
		return errBadTuple
	}
	space := tupleSpace{path: f.space}
	rec, err := space.deposit(payload, f.expires)
	if err != nil {
		return err
	}
	if f.envelope {
		raw, err := marshalRecord(rec)
		if err != nil {
			return err
		}
		return writeJSON(os.Stdout, raw, f.pretty)
	}
	return nil
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
	fs.StringVar(&f.template, "template", "", "JSON pattern template to match")
	fs.StringVar(&f.uuid, "uuid", "", "system UUID to match")
	if remove {
		fs.StringVar(&f.lease, "lease", "", "claim tuple with lease duration (e.g. 60s, +60s)")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	f.resolveSpace()
	f.logVerbose()
	pattern, err := resolvePattern(fs.Args(), &f, true)
	if err != nil {
		return err
	}
	var leaseDur time.Duration
	if remove && f.lease != "" {
		cleanLease := strings.TrimPrefix(f.lease, "+")
		dur, err := time.ParseDuration(cleanLease)
		if err != nil {
			return fmt.Errorf("invalid --lease duration: %w", err)
		}
		if dur <= 0 {
			return errors.New("--lease duration must be positive")
		}
		leaseDur = dur
	}
	space := tupleSpace{path: f.space}
	ctx := context.Background()
	if f.wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.wait)
		defer cancel()
	}
	target := TargetSpec{
		UUID:        f.uuid,
		Pattern:     pattern,
		Keys:        f.keys,
		AllowHidden: (f.uuid != "" && remove && leaseDur == 0),
	}
	for {
		rec, found, err := space.findOne(ctx, target, leaseDur, remove)
		if err != nil {
			return err
		}
		if found {
			if f.envelope {
				raw, err := marshalRecord(rec)
				if err != nil {
					return err
				}
				return writeJSON(os.Stdout, raw, f.pretty)
			}
			return writeJSON(os.Stdout, rec.Payload, f.pretty)
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

func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var f commandFlags
	addCommonFlags(fs, &f)
	fs.StringVar(&f.uuid, "uuid", "", "target tuple system UUID")
	fs.StringVar(&f.template, "template", "", "target pattern template")
	fs.StringVar(&f.lease, "lease", "", "extend lease duration (e.g. +60s or 60s)")
	fs.BoolVar(&f.release, "release", false, "release leased tuple back to active queue")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f.resolveSpace()
	f.logVerbose()

	if f.release && f.lease != "" {
		return errors.New("cannot specify both --lease and --release")
	}

	var target TargetSpec
	target.UUID = f.uuid
	target.Keys = f.keys

	posArgs := fs.Args()
	if f.template != "" {
		pat, err := normalizeJSON([]byte(f.template))
		if err != nil {
			return fmt.Errorf("invalid --template: %w", err)
		}
		if !isTuple(pat) {
			return errBadPattern
		}
		target.Pattern = pat
	} else if f.uuid == "" && len(posArgs) > 0 {
		pat, err := normalizeJSON([]byte(posArgs[0]))
		if err != nil {
			return fmt.Errorf("invalid template argument: %w", err)
		}
		if !isTuple(pat) {
			return errBadPattern
		}
		target.Pattern = pat
		posArgs = posArgs[1:]
	}

	if target.UUID == "" && target.Pattern == nil && len(target.Keys) == 0 {
		return errors.New("update requires --uuid or a pattern template/key filter")
	}

	var mods updateModOptions
	if f.release {
		mods.Release = true
	} else if f.lease != "" {
		mods.HasLease = true
		mods.LeaseExt = f.lease
	}

	var newPayload json.RawMessage
	if f.input != "" {
		p, err := normalizeJSON([]byte(f.input))
		if err != nil {
			return fmt.Errorf("invalid --json: %w", err)
		}
		newPayload = p
	} else if f.inputFile != "" {
		data, err := os.ReadFile(f.inputFile)
		if err != nil {
			return fmt.Errorf("read JSON file: %w", err)
		}
		p, err := normalizeJSON(data)
		if err != nil {
			return err
		}
		newPayload = p
	} else if f.uuid != "" && len(posArgs) == 1 {
		p, err := normalizeJSON([]byte(posArgs[0]))
		if err != nil {
			return fmt.Errorf("invalid payload argument: %w", err)
		}
		newPayload = p
		posArgs = nil
	} else if len(posArgs) > 0 {
		return errors.New("too many positional arguments for update")
	}

	if newPayload != nil {
		if !isTuple(newPayload) {
			return errBadTuple
		}
		mods.NewPayload = newPayload
		mods.HasPayload = true
	}

	if !mods.HasPayload && !mods.HasLease && !mods.Release {
		return errors.New("update requires at least one of --json, --lease, or --release")
	}

	space := tupleSpace{path: f.space}
	updatedRec, err := space.update(target, mods)
	if err != nil {
		return err
	}

	if f.envelope {
		raw, err := marshalRecord(updatedRec)
		if err != nil {
			return err
		}
		return writeJSON(os.Stdout, raw, f.pretty)
	}
	return writeJSON(os.Stdout, updatedRec.Payload, f.pretty)
}

func runDelete(args []string) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var f commandFlags
	addCommonFlags(fs, &f)
	fs.StringVar(&f.uuid, "uuid", "", "target tuple system UUID to delete")
	fs.StringVar(&f.template, "template", "", "target pattern template to delete")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f.resolveSpace()
	f.logVerbose()

	var target TargetSpec
	if f.uuid != "" {
		target = TargetSpec{
			UUID:        f.uuid,
			AllowHidden: true,
		}
	} else {
		pattern, err := resolvePattern(fs.Args(), &f, false)
		if err != nil {
			return err
		}
		if pattern == nil && len(f.keys) == 0 {
			return errors.New("delete requires --uuid or a template/key filter")
		}
		target = TargetSpec{
			Pattern: pattern,
			Keys:    f.keys,
		}
	}

	space := tupleSpace{path: f.space}
	rec, found, err := space.findOne(context.Background(), target, 0, true)
	if err != nil {
		return err
	}
	if !found {
		return errNoMatch
	}

	if f.envelope {
		raw, err := marshalRecord(rec)
		if err != nil {
			return err
		}
		return writeJSON(os.Stdout, raw, f.pretty)
	}
	return writeJSON(os.Stdout, rec.Payload, f.pretty)
}

func runSweep(args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var f commandFlags
	addCommonFlags(fs, &f)
	if err := fs.Parse(args); err != nil {
		return err
	}
	f.resolveSpace()
	f.logVerbose()

	space := tupleSpace{path: f.space}
	res, err := space.sweep()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, raw, f.pretty)
}

func runCount(args []string) error {
	fs := flag.NewFlagSet("count", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var f commandFlags
	addCommonFlags(fs, &f)
	fs.StringVar(&f.template, "template", "", "JSON pattern template to match")
	fs.StringVar(&f.uuid, "uuid", "", "system UUID to match")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f.resolveSpace()
	f.logVerbose()
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
	fs.StringVar(&f.template, "template", "", "JSON pattern template to match")
	fs.StringVar(&f.uuid, "uuid", "", "system UUID to match")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f.resolveSpace()
	f.logVerbose()
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
	fs.StringVar(&f.space, "space", "", "path to the shared JSON tuple-space file (defaults to SPACE_FILE env var or ./linda-space.json)")
	fs.BoolVar(&f.verbose, "v", false, "print resolved storage path to stderr")
	fs.BoolVar(&f.verbose, "verbose", false, "print resolved storage path to stderr")
	fs.StringVar(&f.input, "json", "", "JSON tuple/template/payload; alternatively provide it as the final argument or on stdin")
	fs.StringVar(&f.inputFile, "file", "", "read the JSON tuple/template from this file")
	fs.BoolVar(&f.pretty, "pretty", false, "pretty-print JSON output")
	fs.Var(&f.keys, "k", "dot-path key-value filter (e.g. -k status=raw -k meta.team=infra); repeatable")
	fs.Var(&f.keys, "key", "dot-path key-value filter (e.g. --key status=raw); repeatable")
	fs.BoolVar(&f.envelope, "e", false, "output complete JSON envelope including _system metadata")
	fs.BoolVar(&f.envelope, "envelope", false, "output complete JSON envelope including _system metadata")
}

func (f *commandFlags) resolveSpace() {
	if f.space != "" {
		return
	}
	if env := os.Getenv(spaceEnvVar); env != "" {
		f.space = env
		return
	}
	f.space = defaultSpacePath
}

func (f *commandFlags) logVerbose() {
	if f.verbose {
		fmt.Fprintf(os.Stderr, "storage: %s\n", f.space)
	}
}

func resolvePattern(args []string, f *commandFlags, allowEmpty bool) (json.RawMessage, error) {
	hasKeyFilters := len(f.keys) > 0
	if len(args) == 0 && f.input == "" && f.inputFile == "" && f.template == "" {
		if allowEmpty || hasKeyFilters || f.uuid != "" {
			return nil, nil
		}
	}
	if f.template != "" {
		if f.input != "" || len(args) > 0 || f.inputFile != "" {
			return nil, errors.New("use only one of --template, positional JSON, --json, or --file")
		}
		pat, err := normalizeJSON([]byte(f.template))
		if err != nil {
			return nil, err
		}
		if !isTuple(pat) {
			return nil, errBadPattern
		}
		return pat, nil
	}
	pattern, err := readJSONInput(args, f.input, f.inputFile, false)
	if err != nil {
		if (allowEmpty || hasKeyFilters || f.uuid != "") && (len(args) == 0 && f.input == "" && f.inputFile == "") {
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

// decodeValue decodes JSON raw bytes using UseNumber.
func decodeValue(raw json.RawMessage) (any, error) {
	var val any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&val); err != nil {
		return nil, err
	}
	return val, nil
}

func (s tupleSpace) find(ctx context.Context, pattern json.RawMessage, keys keyValueFilters, remove bool) (json.RawMessage, bool, error) {
	rec, found, err := s.findOne(ctx, TargetSpec{Pattern: pattern, Keys: keys}, 0, remove)
	if err != nil || !found {
		return nil, found, err
	}
	return rec.Payload, true, nil
}

func (s tupleSpace) count(pattern json.RawMessage, keys keyValueFilters) (int, error) {
	count := 0
	var decodedPattern any
	if pattern != nil {
		var err error
		decodedPattern, err = decodeValue(pattern)
		if err != nil {
			return 0, fmt.Errorf("decode template: %w", err)
		}
	}
	err := withLockedFile(s.path, false, func(_ *os.File, state *fileState) error {
		now := time.Now().UTC()
		for _, raw := range state.Tuples {
			rec, err := parseRecord(raw, now)
			if err != nil {
				return err
			}
			if !isRecordAvailable(&rec.System, now) {
				continue
			}
			ok, err := matchCandidatePayload(decodedPattern, keys, &rec)
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
	var decodedPattern any
	if pattern != nil {
		var err error
		decodedPattern, err = decodeValue(pattern)
		if err != nil {
			return 0, fmt.Errorf("decode template: %w", err)
		}
	}
	err := withLockedFile(s.path, true, func(file *os.File, state *fileState) error {
		now := time.Now().UTC()
		kept := state.Tuples[:0]
		for _, raw := range state.Tuples {
			rec, err := parseRecord(raw, now)
			if err != nil {
				return err
			}
			if !isRecordAvailable(&rec.System, now) {
				kept = append(kept, raw)
				continue
			}
			ok, err := matchCandidatePayload(decodedPattern, keys, &rec)
			if err != nil {
				return err
			}
			if ok {
				removed++
				continue
			}
			kept = append(kept, raw)
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
	var decodedPattern any
	if pattern != nil {
		var err error
		decodedPattern, err = decodeValue(pattern)
		if err != nil {
			return false, fmt.Errorf("decode template: %w", err)
		}
	}
	return tupleMatchesDecoded(decodedPattern, keys, candidate)
}

func tupleMatchesDecoded(pattern any, keys keyValueFilters, candidate json.RawMessage) (bool, error) {
	if pattern == nil && len(keys) == 0 {
		return true, nil
	}
	c, err := decodeValue(candidate)
	if err != nil {
		return false, fmt.Errorf("decode tuple: %w", err)
	}
	if pattern != nil {
		if !subsetMatch(pattern, c) {
			return false, nil
		}
	}
	if len(keys) > 0 {
		if !matchParsedKeyValues(keys, c) {
			return false, nil
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
	lock := syscall.LOCK_SH
	if exclusive {
		lock = syscall.LOCK_EX
	}
	for {
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return fmt.Errorf("open space: %w", err)
		}
		if err := syscall.Flock(int(file.Fd()), lock); err != nil {
			file.Close()
			return fmt.Errorf("lock space: %w", err)
		}
		fiFile, err := file.Stat()
		if err != nil {
			syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			file.Close()
			return fmt.Errorf("stat space file descriptor: %w", err)
		}
		fiPath, err := os.Stat(path)
		if err != nil {
			// If the file was replaced or removed concurrently right before stat, retry.
			syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			file.Close()
			continue
		}
		if os.SameFile(fiFile, fiPath) {
			defer file.Close()
			defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			state, err := readState(file)
			if err != nil {
				return err
			}
			return fn(file, state)
		}
		// File was atomically replaced before lock was acquired; retry with the new file.
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}
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
	destPath := file.Name()
	dir := filepath.Dir(destPath)
	tempFile, err := os.CreateTemp(dir, filepath.Base(destPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp space file: %w", err)
	}
	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			tempFile.Close()
			os.Remove(tempPath)
		}
	}()

	if _, err := tempFile.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write temp space: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		return fmt.Errorf("fsync temp space: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp space: %w", err)
	}
	if err := os.Rename(tempPath, destPath); err != nil {
		return fmt.Errorf("rename temp space: %w", err)
	}
	cleanup = false
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func matches(pattern, candidate json.RawMessage) (bool, error) {
	p, err := decodeValue(pattern)
	if err != nil {
		return false, fmt.Errorf("decode template: %w", err)
	}
	c, err := decodeValue(candidate)
	if err != nil {
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
  out      Add one JSON object or array tuple to the space (supports --expires and -e/--envelope).
  in       Remove and print the first tuple matching a JSON template / key flags (supports --lease and -e/--envelope).
  rd       Read (without removing) the first matching tuple. Alias: read.
  update   Atomically modify a tuple's payload and/or lease state (--uuid or --template).
  delete   Permanently remove a tuple by system UUID (--uuid) or pattern.
  sweep    Administrative maintenance: purge hard-expired tuples and reinflate expired leases.
  count    Print the number of available tuples, optionally matching a template / key flags.
  clip     Remove all matching available tuples and print the number removed.

Flags:
  -e, --envelope   Output complete JSON envelope including _system metadata.
  --expires DURATION/TIME  Set absolute expiration for deposited tuple (out).
  --lease DURATION         Claim with lease (in) or extend lease (update).
  --release                Release leased tuple back to active queue (update).
  --uuid UUID              Target tuple by system UUID (update, delete, in, rd).
  --template JSON          Specify JSON template pattern for matching.
  --json JSON              Input tuple, payload, or template.
  --wait DURATION          Wait up to this duration for a match (in, rd).
  --fail-on-none           Exit 1 when no matching tuple is found (in, rd, clip).
  -k, --key PATH=VALUE     Dot-path key-value filter (repeatable).

Matching & Key Flags:
  Templates are recursive subsets. Alternatively or additionally, use repeatable
  -k or --key <dot.path>=<value> flags to match nested fields without JSON templates.
  Example: -k status=raw -k meta.team=infra

Input:
  JSON may be the final argument, --json VALUE, --file PATH, or stdin.
  A tuple is a top-level JSON object or array. The default space is
  ./linda-space.json, unless overridden by the SPACE_FILE environment variable
  or the --space PATH flag. Use -v or --verbose to inspect the resolved storage
  path on stderr.

Blocking:
  in and rd check immediately by default. --wait DURATION enables long-polling
  until a matching tuple arrives or the duration expires (for example 30s or
  5m). --fail-on-none makes a timeout/miss exit 1; without it, a miss exits 0
  and prints nothing. in is atomic under the file lock.

Examples:
  linda-space out --space /tmp/jobs.json '{"type":"job","status":"raw"}'
  linda-space in --space /tmp/jobs.json --envelope --lease 60s -k status=raw
  linda-space update --space /tmp/jobs.json --uuid sys-a1b2c3d4 --lease +60s
  linda-space update --space /tmp/jobs.json --uuid sys-a1b2c3d4 --json '{"status":"done"}'
  linda-space update --space /tmp/jobs.json --uuid sys-a1b2c3d4 --release --json '{"status":"open"}'
  linda-space delete --space /tmp/jobs.json --uuid sys-a1b2c3d4
  linda-space sweep --space /tmp/jobs.json
  linda-space rd --space /tmp/jobs.json --wait 30s --fail-on-none '{"type":"job"}'
  linda-space count --space /tmp/jobs.json -k status=raw
  linda-space clip --space /tmp/jobs.json -k status=done

Exit status:
  0 success (including a non-matching in/rd unless --fail-on-none is set)
  1 invalid input, I/O failure, or --fail-on-none with no match
  2 unknown command or flag syntax error
`)
}
