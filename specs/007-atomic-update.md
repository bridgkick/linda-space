# Spec 007: Atomic Update / CAS by UUID or Pattern Match

## Overview
Provide a native `update` command to modify tuples, manage lease states, or renew heartbeats in a single atomic transaction under file lock.

## Requirements & Mini-Spec
1. **Targeting (Explicit UUID or Pattern Template)**:
   - `update` can target either an explicit system UUID (`--uuid sys-a1b2c3d4`) OR a Linda pattern-matching template (`--template '{...}'`). 
   - Pattern matching updates matching tuples (or the first matching, configurable).
2. **Operations & Payload Modification**:
   - Updates user `payload` via `--json`. Replaces the payload atomically in a single CAS transaction.
3. **Lease State Management Flags**:
   - `--lease +60s` / `--lease <duration>`: Heartbeat action. Extends `_system.lease_expires` deadline and keeps `_system.hidden = true` (retains active lease lock).
   - `--release`: Early release action. Swaps the payload and returns the item to the active queue by setting `_system.hidden = false` and clearing `_system.lease_expires = null`.
   - **Default Behavior (No Lease Flag):** If neither `--lease` nor `--release` is passed during an `update`, the engine **preserves** the existing active lease state (`_system.hidden = true` and current `lease_expires`) so routine payload swaps don't accidentally drop the worker's lock.
4. **CLI Syntax Examples**:
   - *Heartbeat / Extend:*
     ```bash
     linda-space update --uuid sys-a1b2c3d4 --lease +60s
     ```
   - *Modify & Retain Lease:*
     ```bash
     linda-space update --uuid sys-a1b2c3d4 --json '{"task": "compile", "status": "running", "progress": 50}'
     ```
   - *Modify & Release Back to Queue:*
     ```bash
     linda-space update --uuid sys-a1b2c3d4 --release --json '{"task": "compile", "status": "reviewing"}'
     ```
