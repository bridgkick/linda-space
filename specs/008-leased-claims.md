# Spec 008: Leased Claims (lin) & TTL / Expiration Reinflation

## Overview
Implement leased claims where claimed tasks are temporarily hidden from normal `in`/`rd` queries, along with precise mechanics for extending leases, early release, absolute expiration, explicit administrative sweeping, and finalization.

## Requirements & Mini-Spec

### 1. Claiming vs. Consuming (`in`)
- **Standard Consumption (`in` without lease):** 
  ```bash
  linda-space in '{"task": "compile"}'
  ```
  - Permanently deletes and removes the matched tuple from the space and journal.
- **Leased Claim (`in --lease <duration>`):**
  ```bash
  linda-space in --envelope --lease 60s '{"task": "compile", "status": "pending"}'
  ```
  - Atomically locates the matching tuple, sets `_system.hidden = true` (hiding it from subsequent standard `in`/`rd` calls), sets `_system.lease_expires = now() + duration`, and returns the full envelope with the system UUID (`sys-a1b2c3d4`).

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
- **Permanent Deletion (`delete --uuid`):**
  ```bash
  linda-space delete --uuid sys-a1b2c3d4
  ```
  *(Clean syntactic sugar for an unqualified `in` filtered strictly by `--uuid sys-a1b2c3d4`, permanently removing the record from space and journal).*

### 4. Passive Filtering & Explicit Maintenance (`linda-space sweep`)
- **Passive Read/Take Queries (`in`/`rd`):** Never perform inline writes or cleanup during reads (preventing shared read-lock deadlock). They simply skip over hidden or expired candidate records.
- **Administrative Sweep Command (`linda-space sweep`):**
  - Acquired under an exclusive file lock (`LOCK_EX`).
  - Scans all stored tuples:
    1. **Absolute Expiration (`expires_at`):** Permanently drops/retires any tuple where `now() > expires_at`.
    2. **Lease Expiration (`lease_expires`):** For any tuple where `now() > lease_expires` and `hidden == true`, it clears the lease and resets `_system.hidden = false`, reinflating the abandoned task back into active queue visibility.
  - Designed to be executed periodically via a cron job or scheduled worker.
