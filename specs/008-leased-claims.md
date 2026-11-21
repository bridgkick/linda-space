# Spec 008: Leased Claims (lin) & TTL / Expiration Reinflation

## Overview
Implement leased claims where claimed tasks are temporarily hidden from normal `in`/`rd` queries with automatic TTL reinflation if workers crash, along with precise mechanics for extending leases and finalization.

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

### 2. Extending the Lease (Heartbeat / Progress)
- **Action:** If a task takes longer than expected, the worker extends its lease via the `update` command using its system UUID:
  ```bash
  linda-space update --uuid sys-a1b2c3d4 --lease +60s
  ```
- **Engine Behavior:**
  - Locks the space, verifies the tuple exists and is still active under this lease (i.e. not already expired or reclaimed), and pushes `_system.lease_expires` forward by the specified duration.

### 3. Finalization & Deletion (Task Completion)
When the worker finishes processing the item, it disposes of or transitions the task using one of two methods:
- **Option A: Mark Complete / Transition State (CAS Update)**
  ```bash
  linda-space update --uuid sys-a1b2c3d4 --json '{"task": "compile", "status": "complete"}'
  ```
  *(Atomically updates the user payload and clears or archives the lease/hidden state).*
- **Option B: Permanent Deletion (Consume & Retire)**
  ```bash
  linda-space delete --uuid sys-a1b2c3d4
  ```
  *(Permanently removes the tuple record from the space and journal).*

### 4. Crash Recovery (Lazy Reinflation)
- **Mechanism:** No background ticker threads (SQLite style). 
- **Execution:** When any incoming `in` or `rd` command runs, the engine inspects candidate tuples. If a tuple's `_system.lease_expires` timestamp has passed (`now() > lease_expires`), the engine treats it as abandoned, automatically clears the lease, sets `_system.hidden = false`, and reinflates it back into active visibility for other workers to claim.
