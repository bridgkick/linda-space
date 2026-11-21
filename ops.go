package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// TargetSpec defines how an update or delete targets a tuple.
type TargetSpec struct {
	UUID        string
	Pattern     json.RawMessage
	Keys        keyValueFilters
	AllowHidden bool // When true, can match tuples with hidden=true (used for --uuid targeting)
}

// FindResult contains matching tuple information.
type FindResult struct {
	Record TupleRecord
	Index  int
}

// SweepResult reports statistics from an administrative sweep.
type SweepResult struct {
	PurgedExpired int `json:"purged_expired"`
	Reinflated    int `json:"reinflated"`
}

// deposit wraps payload into a TupleRecord with a new UUID and optional expiration.
func (s tupleSpace) deposit(payload json.RawMessage, expires string) (TupleRecord, error) {
	now := time.Now().UTC()
	id, err := generateSystemID()
	if err != nil {
		return TupleRecord{}, err
	}

	var expiresAt *string
	if expires != "" {
		expTime, err := parseDurationOrTimestamp(expires, now)
		if err != nil {
			return TupleRecord{}, fmt.Errorf("invalid --expires: %w", err)
		}
		expStr := formatTimestamp(expTime)
		expiresAt = &expStr
	}

	rec := TupleRecord{
		System: SystemEnvelope{
			UUID:         id,
			CreatedAt:    formatTimestamp(now),
			LeaseExpires: nil,
			ExpiresAt:    expiresAt,
			Hidden:       false,
		},
		Payload: cloneRaw(payload),
	}

	err = s.mutate(func(state *fileState) error {
		rawRec, err := marshalRecord(rec)
		if err != nil {
			return err
		}
		state.Tuples = append(state.Tuples, rawRec)
		return nil
	})
	return rec, err
}

// findOne locates the first matching tuple matching target criteria.
// If leaseDuration > 0, the matched tuple is claimed under lease (hidden=true, lease_expires set).
// If remove is true (and leaseDuration <= 0), the matched tuple is deleted permanently.
func (s tupleSpace) findOne(ctx context.Context, target TargetSpec, leaseDuration time.Duration, remove bool) (TupleRecord, bool, error) {
	var matchedRec TupleRecord
	var found bool

	var decodedPattern any
	if target.Pattern != nil {
		var err error
		decodedPattern, err = decodeValue(target.Pattern)
		if err != nil {
			return TupleRecord{}, false, fmt.Errorf("decode template: %w", err)
		}
	}

	exclusive := remove || (leaseDuration > 0)
	err := withLockedFile(s.path, exclusive, func(file *os.File, state *fileState) error {
		now := time.Now().UTC()
		for i, raw := range state.Tuples {
			rec, err := parseRecord(raw, now)
			if err != nil {
				return err
			}

			// Target matching
			if target.UUID != "" {
				if rec.System.UUID != target.UUID {
					continue
				}
				// When targeting by UUID, we allow matching even if hidden unless explicitly forbidden
				if !target.AllowHidden && !isRecordAvailable(&rec.System, now) {
					continue
				}
			} else {
				// Regular query: passive filtering ignores hidden or expired tuples
				if !isRecordAvailable(&rec.System, now) {
					continue
				}
				ok, err := matchCandidatePayload(decodedPattern, target.Keys, &rec)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
			}

			matchedRec = rec
			found = true

			if leaseDuration > 0 {
				// Leased claim: mark hidden and set lease_expires
				leaseEnd := now.Add(leaseDuration)
				leaseEndStr := formatTimestamp(leaseEnd)
				rec.System.Hidden = true
				rec.System.LeaseExpires = &leaseEndStr
				matchedRec = rec

				newRaw, err := marshalRecord(rec)
				if err != nil {
					return err
				}
				state.Tuples[i] = newRaw
				return writeState(file, state)
			} else if remove {
				// Permanent consumption / deletion
				state.Tuples = append(state.Tuples[:i], state.Tuples[i+1:]...)
				return writeState(file, state)
			}
			return nil
		}
		return nil
	})

	return matchedRec, found, err
}

// updateModOptions contains payload and lease state modifications.
type updateModOptions struct {
	NewPayload json.RawMessage
	HasPayload bool
	LeaseExt   string // e.g. "+60s" or "60s"
	HasLease   bool
	Release    bool
}

// update atomically modifies a targeted tuple in the space under lock.
func (s tupleSpace) update(target TargetSpec, mods updateModOptions) (TupleRecord, error) {
	var updatedRec TupleRecord
	var found bool

	var decodedPattern any
	if target.Pattern != nil {
		var err error
		decodedPattern, err = decodeValue(target.Pattern)
		if err != nil {
			return TupleRecord{}, fmt.Errorf("decode template: %w", err)
		}
	}

	err := withLockedFile(s.path, true, func(file *os.File, state *fileState) error {
		now := time.Now().UTC()
		for i, raw := range state.Tuples {
			rec, err := parseRecord(raw, now)
			if err != nil {
				return err
			}

			if target.UUID != "" {
				if rec.System.UUID != target.UUID {
					continue
				}
				// If lease is expired and it was hidden, can it be updated?
				// Spec: "locks the space, verifies the tuple exists and is still active under this lease"
				// If lease expired, it is no longer active under this lease.
				if rec.System.LeaseExpires != nil {
					exp, err := parseTimestamp(*rec.System.LeaseExpires)
					if err == nil && !now.Before(exp) {
						// Expired lease
						return fmt.Errorf("lease for tuple %s has expired", target.UUID)
					}
				}
			} else {
				if !isRecordAvailable(&rec.System, now) {
					continue
				}
				ok, err := matchCandidatePayload(decodedPattern, target.Keys, &rec)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
			}

			found = true

			// Apply payload update if provided
			if mods.HasPayload {
				rec.Payload = cloneRaw(mods.NewPayload)
			}

			// Apply lease modifications
			if mods.Release {
				// Early release back to queue
				rec.System.Hidden = false
				rec.System.LeaseExpires = nil
			} else if mods.HasLease {
				// Heartbeat / extend lease
				extTime, err := parseDurationOrTimestamp(mods.LeaseExt, now)
				if err != nil {
					return fmt.Errorf("invalid --lease: %w", err)
				}
				extStr := formatTimestamp(extTime)
				rec.System.LeaseExpires = &extStr
				rec.System.Hidden = true
			}
			// Default behavior: if neither --lease nor --release is passed,
			// active lease state (hidden and current lease_expires) is preserved.

			updatedRec = rec
			newRaw, err := marshalRecord(rec)
			if err != nil {
				return err
			}
			state.Tuples[i] = newRaw
			return writeState(file, state)
		}

		if !found {
			return errNoMatch
		}
		return nil
	})

	return updatedRec, err
}

// sweep performs administrative maintenance under exclusive file lock:
// 1. Permanently purges hard-expired records (expires_at has passed).
// 2. Reinflates expired leases (hidden=true and lease_expires has passed) by setting hidden=false, lease_expires=nil.
func (s tupleSpace) sweep() (SweepResult, error) {
	var res SweepResult
	err := withLockedFile(s.path, true, func(file *os.File, state *fileState) error {
		now := time.Now().UTC()
		var kept []json.RawMessage
		dirty := false

		for _, raw := range state.Tuples {
			rec, err := parseRecord(raw, now)
			if err != nil {
				return err
			}

			// 1. Check absolute expiration
			if rec.System.ExpiresAt != nil {
				exp, err := parseTimestamp(*rec.System.ExpiresAt)
				if err == nil && !now.Before(exp) {
					res.PurgedExpired++
					dirty = true
					continue // purge
				}
			}

			// 2. Check lease expiration reinflation
			if rec.System.Hidden && rec.System.LeaseExpires != nil {
				leaseExp, err := parseTimestamp(*rec.System.LeaseExpires)
				if err == nil && !now.Before(leaseExp) {
					rec.System.Hidden = false
					rec.System.LeaseExpires = nil
					res.Reinflated++
					dirty = true
				}
			}

			newRaw, err := marshalRecord(rec)
			if err != nil {
				return err
			}
			kept = append(kept, newRaw)
		}

		state.Tuples = kept
		if dirty {
			return writeState(file, state)
		}
		return nil
	})
	return res, err
}
