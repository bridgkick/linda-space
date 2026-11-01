package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// keyValueFilter stores a parsed dot-path filter (e.g. -k meta.team=infra).
//
// Target values (rawVal) are pre-parsed once at flag parsing time into parsedVal
// (using json.Decoder with UseNumber) to avoid re-decoding inside inner matching loops
// over candidate tuples.
//
// Coercion Behavior:
// JSON scalar value coercion (parsing target values as JSON numbers/bools/nulls with
// string fallback) is intentional and left as-is per user preference. For instance,
// -k priority=2 parses as json.Number("2"), -k active=true parses as bool true,
// -k ref=null parses as nil, while unquoted strings like -k status=raw fail JSON decoding
// and fall back cleanly to raw strings.
type keyValueFilter struct {
	path      string
	rawVal    string
	parsedVal any
	hasParsed bool
}

type keyValueFilters []keyValueFilter

func (k *keyValueFilters) String() string {
	var parts []string
	for _, f := range *k {
		parts = append(parts, f.path+"="+f.rawVal)
	}
	return strings.Join(parts, ", ")
}

// Set parses a key=value filter string. Target values are decoded once here as JSON scalars
// (number, boolean, null, object, array) with fallback to raw string, caching the result
// in parsedVal so candidate matching doesn't repeat JSON decoding.
func (k *keyValueFilters) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid key-value filter %q, expected key=value", value)
	}
	rawVal := strings.TrimSpace(parts[1])
	var parsedVal any
	dec := json.NewDecoder(strings.NewReader(rawVal))
	dec.UseNumber()
	hasParsed := dec.Decode(&parsedVal) == nil

	*k = append(*k, keyValueFilter{
		path:      strings.TrimSpace(parts[0]),
		rawVal:    rawVal,
		parsedVal: parsedVal,
		hasParsed: hasParsed,
	})
	return nil
}

// matchKeyValues checks if a candidate tuple satisfies all dot-path key-value filters.
// Filter target values are pre-parsed once during flag parsing, avoiding re-decoding
// inside this candidate evaluation loop.
func matchKeyValues(filters keyValueFilters, candidate json.RawMessage) (bool, error) {
	if len(filters) == 0 {
		return true, nil
	}
	var c any
	dec := json.NewDecoder(strings.NewReader(string(candidate)))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return false, fmt.Errorf("decode candidate: %w", err)
	}

	for _, f := range filters {
		val, found := getNestedValue(c, f.path)
		if !found {
			return false, nil
		}
		targetVal := f.parsedVal
		if !f.hasParsed {
			targetVal = f.rawVal
		}
		if !jsonValueEqual(targetVal, val) {
			return false, nil
		}
	}
	return true, nil
}

// getNestedValue traverses nested maps using dot-separated keys.
//
// NOTE: Dot-path key traversal does not yet support escaped dots ('.').
// Literal dot escaping in key names will be implemented later via slash-escaping ('\.').
func getNestedValue(obj any, dotPath string) (any, bool) {
	parts := strings.Split(dotPath, ".")
	curr := obj
	for _, p := range parts {
		m, ok := curr.(map[string]any)
		if !ok {
			return nil, false
		}
		next, exists := m[p]
		if !exists {
			return nil, false
		}
		curr = next
	}
	return curr, true
}

