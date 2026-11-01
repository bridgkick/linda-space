package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type keyValueFilter struct {
	path  string
	value string
}

type keyValueFilters []keyValueFilter

func (k *keyValueFilters) String() string {
	return fmt.Sprint(*k)
}

func (k *keyValueFilters) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid key-value filter %q, expected key=value", value)
	}
	*k = append(*k, keyValueFilter{
		path:  strings.TrimSpace(parts[0]),
		value: strings.TrimSpace(parts[1]),
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
		var targetVal any
		dec2 := json.NewDecoder(strings.NewReader(f.value))
		dec2.UseNumber()
		if err := dec2.Decode(&targetVal); err != nil {
			targetVal = f.value
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
