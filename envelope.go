package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SystemEnvelope wraps internal system metadata for each tuple in the space.
type SystemEnvelope struct {
	UUID         string  `json:"uuid"`
	CreatedAt    string  `json:"created_at"`
	LeaseExpires *string `json:"lease_expires"`
	ExpiresAt    *string `json:"expires_at"`
	Hidden       bool    `json:"hidden"`
}

// TupleRecord is the stored representation of a tuple with its system envelope.
type TupleRecord struct {
	System  SystemEnvelope  `json:"_system"`
	Payload json.RawMessage `json:"payload"`
}

// generateSystemID creates an 8-character lowercase hex string prefixed with "sys-",
// using 4 random bytes from crypto/rand.
func generateSystemID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate system id: %w", err)
	}
	return "sys-" + hex.EncodeToString(b), nil
}

// formatTimestamp returns an RFC3339 formatted UTC timestamp string.
func formatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// parseTimestamp parses an RFC3339 or ISO 8601 formatted string into UTC time.
func parseTimestamp(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// parseDurationOrTimestamp parses either a Go duration (e.g. "60s", "2h", "+60s")
// relative to now, or an absolute ISO-8601 timestamp.
func parseDurationOrTimestamp(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty duration/timestamp")
	}
	cleanDuration := strings.TrimPrefix(s, "+")
	if d, err := time.ParseDuration(cleanDuration); err == nil {
		return now.Add(d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid duration or timestamp %q", s)
}

// parseRecord parses a raw JSON tuple from the storage file into a TupleRecord.
// If the tuple was stored before envelope wrapping (legacy tuple), it wraps it automatically.
func parseRecord(raw json.RawMessage, now time.Time) (TupleRecord, error) {
	var rec TupleRecord
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err == nil && m != nil {
		_, hasSys := m["_system"]
		_, hasPayload := m["payload"]
		if hasSys && hasPayload && len(m) == 2 {
			if err := json.Unmarshal(raw, &rec); err == nil && rec.System.UUID != "" {
				return rec, nil
			}
		}
	}

	// Legacy tuple: wrap it
	id, err := generateSystemID()
	if err != nil {
		return TupleRecord{}, err
	}
	return TupleRecord{
		System: SystemEnvelope{
			UUID:         id,
			CreatedAt:    formatTimestamp(now),
			LeaseExpires: nil,
			ExpiresAt:    nil,
			Hidden:       false,
		},
		Payload: cloneRaw(raw),
	}, nil
}

// marshalRecord encodes the TupleRecord back to json.RawMessage.
func marshalRecord(rec TupleRecord) (json.RawMessage, error) {
	return json.Marshal(rec)
}

// isRecordAvailable checks if a record is active and matchable for regular in/rd/count/clip queries.
// It returns false if hidden is true, or if lease_expires or expires_at have passed.
func isRecordAvailable(sys *SystemEnvelope, now time.Time) bool {
	if sys.Hidden {
		return false
	}
	if sys.LeaseExpires != nil {
		t, err := parseTimestamp(*sys.LeaseExpires)
		if err == nil && !now.Before(t) { // now >= t: expired
			return false
		}
	}
	if sys.ExpiresAt != nil {
		t, err := parseTimestamp(*sys.ExpiresAt)
		if err == nil && !now.Before(t) { // now >= t: expired
			return false
		}
	}
	return true
}

// matchCandidatePayload matches a pattern and key filters against candidate's payload or candidate itself.
func matchCandidatePayload(pattern any, keys keyValueFilters, rec *TupleRecord) (bool, error) {
	payloadVal, err := decodeValue(rec.Payload)
	if err != nil {
		return false, fmt.Errorf("decode payload: %w", err)
	}

	if pattern != nil {
		if !subsetMatch(pattern, payloadVal) {
			return false, nil
		}
	}
	if len(keys) > 0 {
		if !matchParsedKeyValues(keys, payloadVal) {
			return false, nil
		}
	}
	return true, nil
}
