# Spec 008: Leased Claims (lin) & TTL / Expiration Reinflation

## Overview
Implement leased claims where claimed tasks are temporarily hidden from normal `in`/`rd` queries with automatic TTL reinflation if workers crash, along with precise mechanics for extending leases, early release, absolute expiration, and finalization.

## Requirements & Mini-Spec

### 1. Claiming with a Lease (`in --lease <duration>`)
- **Action:** An agent claims a task matching a template:
  ```bash
  linda-space in --envelope --lease 60s '{"task": "compile", "status": "pending"}'
  ```
- **Engine Behavior:**
  1. Atomically locks the space and locates the first matching tuple.
  2. Sets `_system.hidden = true` (making it invisible to subsequent standard `in`/`rd` calls).
  3. Sets `_system.lease_expires = now() + 60s`.
  4. Returns the full envelope containing the assigned system UUID (`sys-a1b2c3d4`).

### 2. Managing Leases via `update`
Workers can manage lease states during task execution using three distinct flag patterns on `update`:
- **Heartbeat / Extend:**
  ```bash
  linda-space update --uuid sys-a1b2c3d4 --lease +60s
  ```
  *(Pushes `_system.lease_expires` forward; keeps tuple hidden).*
- **Modify & Retain Lease:**
  ```bash
  linda-space update --uuid sys-a1b2c3d4 --json '{"task": "compile", "status": "running"}'
  ```
  *(Swaps user payload; preserves existing active lease and hidden status).*
- **Modify & Release Back to Queue (Early Release):**
  ```bash
  linda-space update --uuid sys-a1b2c3d4 --release --json '{"task": "compile", "status": "reviewing"}'
  ```
  *(Swaps payload, un-hides the tuple by setting `_system.hidden = false`, and clears `_system.lease_expires = null` so another worker can claim it).*

### 3. Finalization & Deletion (Task Completion)
When a worker finishes processing an item completely, it retires the task:
- **Permanent Deletion (Consume & Retire):**
  ```bash
  linda-space delete --uuid sys-a1b2c3d4
  ```
  *(Note: `delete --uuid` is syntactic sugar for an unqualified `in` filtered strictly by `--uuid sys-a1b2c3d4`, permanently removing the record from space and journal).*

### 4. Crash Recovery & Absolute Expiration (Lazy Sweeping)
- **Mechanism:** No background ticker threads (SQLite style). 
- **Execution:** When any incoming `in` or `rd` command runs, the engine inspects candidate tuples:
  1. **Absolute Expiration (`expires_at`):** If `now() > expires_at`, the engine permanently drops/retires the tuple from the space (ignoring it completely).
  2. **Lease Expiration (`lease_expires`):** If `now() > lease_expires` (and `expires_at` has not passed), the engine treats the task as abandoned by a crashed worker, automatically clears the lease, sets `_system.hidden = false`, and reinflates it back into active visibility for other workers to claim.
