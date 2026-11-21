# Spec 008: Leased Claims (lin) & TTL / Expiration Reinflation

## Overview
Implement leased claims where claimed tasks are temporarily hidden from normal `in`/`rd` queries with automatic TTL reinflation if workers crash, along with precise mechanics for extending leases, early release, and finalization.

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
  *(Permanently removes the tuple record from the space and journal).*

### 4. Crash Recovery (Lazy Reinflation)
- **Mechanism:** No background ticker threads (SQLite style). 
- **Execution:** When any incoming `in` or `rd` command runs, the engine inspects candidate tuples. If a tuple's `_system.lease_expires` timestamp has passed (`now() > lease_expires`), the engine treats it as abandoned, automatically clears the lease, sets `_system.hidden = false`, and reinflates it back into active visibility for other workers to claim.
