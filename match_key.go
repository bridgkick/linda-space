package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type keyValueFilter struct {
	path       string
	rawVal     string
	parsedVal  any
	hasParsed  bool
}

type keyValueFilters []keyValueFilter

func (k *keyValueFilters) String() string {
	var parts []string
	for _, f := range *k {
		parts = append(parts, f.path+"="+f.rawVal)
	}
	return strings.Join(parts, ", ")
}

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
