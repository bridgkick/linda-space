# Spec 006: Internal System Envelope (_system)

## Overview
Wrap incoming tuples in an internal system envelope (`_system`) while keeping user payloads clean, introduce an `--envelope` flag, and support absolute expiration.

## Requirements & Mini-Spec
1. **The System Envelope Structure**:
   - Every tuple deposited via `out` is wrapped in:
     ```json
     {
       "_system": {
         "uuid": "sys-a1b2c3d4",
         "created_at": "ISO-8601",
         "lease_expires": null,
         "expires_at": null,
         "hidden": false
       },
       "payload": { ... }
     }
     ```
2. **System ID Generation Details (Option A - Short 8-char Random Hex)**:
   - Generated using Go's built-in `crypto/rand` to generate **4 random bytes**, encoded as an 8-character lowercase hexadecimal string with a `sys-` prefix (e.g., `sys-a1b2c3d4`).
   - **Collision Safety:** With 32 bits of entropy ($4.3 \text{ billion}$ combinations), collision probability for a max active pool of 10,000 tuples is roughly 1 in 86,000. Perfect for lean local agent swarms while keeping terminal logs and JSONL journals clean and human-readable with zero external dependencies.
   - **Execution:** Generated atomically under the space file lock upon every `out()` operation.
3. **Absolute Expiration (`--expires`)**:
   - `out` supports an optional `--expires <duration>` (e.g., `--expires 2h`) or absolute ISO timestamp.
   - Stamps `_system.expires_at`. When the explicit `sweep` command runs, any tuple where `now() > expires_at` is permanently dropped/retired from the space.
4. **Passive Read Filtering (No Inline Sweeps)**:
   - `in` and `rd` perform **passive filtering**: they simply ignore tuples where `hidden: true`, `lease_expires` has passed, or `expires_at` has passed. They never perform inline write mutations during reads, preventing shared read-lock (`LOCK_SH`) write deadlocks.
   - **Explicit Cleanup via `sweep`:** A dedicated maintenance command (`linda-space sweep`) acquires an exclusive lock (`LOCK_EX`) to reinflate expired leases and purge hard-expired records. This is designed to be run periodically via a system cron job or background worker.
5. **The `--envelope` Flag**:
   - By default, commands output only the inner `payload` for backward compatibility.
   - When `--envelope` (`-e`) is passed, the complete JSON envelope including `_system` metadata is returned to `stdout`.
