# Spec 006: Internal System Envelope (_system)

## Overview
Wrap incoming tuples in an internal system envelope (`_system`) while keeping user payloads clean, and introduce an `--envelope` flag.

## Requirements & Mini-Spec
1. **The System Envelope Structure**:
   - Every tuple deposited via `out` is wrapped in:
     ```json
     {
       "_system": {
         "uuid": "sys-uuid-v4",
         "created_at": "ISO-8601",
         "lease_expires": null,
         "expires_at": null,
         "hidden": false
       },
       "payload": { ... }
     }
     ```
2. **UUID Generation Details**:
   - Generated using Go's standard cryptographic/pseudo-random number generator (`crypto/rand`) following the **UUIDv4 specification** (RFC 4122).
   - Format: 36-character hexadecimal string with hyphens (`8-4-4-4-12`), e.g., `f47ac10b-58cc-4372-a567-0e02b2c3d479`.
   - Version bits: The 13th hex character must be `4` (indicating UUIDv4), and the 17th hex character must be `8`, `9`, `a`, or `b` (indicating RFC 4122 variant).
   - Uniqueness guarantee: Generated on every `out()` invocation under the file lock, ensuring zero collision probability across concurrent agent workers.
3. **Visibility & Lazy Sweeping (SQLite Style)**:
   - `in` and `rd` **ignore** tuples where `hidden: true` or where `lease_expires` / `expires_at` are in the past.
   - **No background processes / daemon tickers**: Expiration and lease sweeps happen **lazily** on the fly during the next incoming `in` or `rd` scan when candidate tuples are evaluated.
4. **The `--envelope` Flag**:
   - By default, commands output only the inner `payload` for backward compatibility.
   - When `--envelope` (`-e`) is passed, the complete JSON envelope including `_system` metadata is returned to `stdout`.
