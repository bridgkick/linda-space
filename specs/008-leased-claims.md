# Spec 008: Leased Claims (lin) & TTL / Expiration Reinflation

## Overview
Implement leased claims where claimed tasks are temporarily hidden from normal `in`/`rd` queries with automatic TTL reinflation if workers crash.

## Requirements & Mini-Spec
1. **Leased Claim (`in --lease <duration>` or `lin`)**:
   - Atomically claims a matching tuple, sets `_system.hidden = true` and `_system.lease_expires = now() + duration`, and returns the envelope containing the `uuid`.
2. **Hard Expiration (`expires_at`)**:
   - Support an optional absolute expiration time (`--expires <duration/timestamp>`) for time-sensitive tasks that should never be retried after a certain window. Swept lazily.
3. **Crash Recovery (Lazy Reinflation)**:
   - When a worker dies, its lease expires. On the next incoming `in` or `rd`, any tuple whose `lease_expires` has passed is automatically reinflated (`hidden = false`, lease cleared) and made available to other workers.
