# Spec 006: Internal System Envelope (_system)

## Overview
Wrap incoming tuples in an internal system envelope (`_system`) while keeping user payloads clean, and introduce an `--envelope` flag.

## Requirements & Mini-Spec
1. **The System Envelope Structure**:
   - Every tuple deposited via `out` is wrapped in:
     ```json
     {
       "_system": {
         "uuid": "sys-a1b2-c3d4",
         "created_at": "ISO-8601",
         "lease_expires": null,
         "expires_at": null,
         "hidden": false
       },
       "payload": { ... }
     }
     ```
2. **System ID Generation Details (Option A - Short Random Hex with Hyphen)**:
   - Generated using Go's built-in `crypto/rand` to generate **4 random bytes**, encoded as an 8-character lowercase hexadecimal string formatted with a hyphen (`sys-XXXX-XXXX`, e.g., `sys-f47c-9b21`).
   - **Collision Safety:** With 32 bits of entropy, this easily scales to thousands of active tuples while keeping terminal logs and JSONL journals clean and human-readable without any external dependency bloat.
   - **Execution:** Generated atomically under the space file lock upon every `out()` operation.
3. **Visibility & Lazy Sweeping (SQLite Style)**:
   - `in` and `rd` **ignore** tuples where `hidden: true` or where `lease_expires` / `expires_at` are in the past.
   - **No background processes / daemon tickers**: Expiration and lease sweeps happen **lazily** on the fly during the next incoming `in` or `rd` scan when candidate tuples are evaluated.
4. **The `--envelope` Flag**:
   - By default, commands output only the inner `payload` for backward compatibility.
   - When `--envelope` (`-e`) is passed, the complete JSON envelope including `_system` metadata is returned to `stdout`.
