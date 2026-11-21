# Spec 007: Atomic Update / CAS by UUID or Pattern Match

## Overview
Provide a native `update` command to modify tuples or renew leases in a single atomic transaction under file lock.

## Requirements & Mini-Spec
1. **Targeting (Explicit UUID or Pattern Template)**:
   - `update` can target either an explicit system UUID (`--uuid sys-uuid`) OR a Linda pattern-matching template (`--template '{...}'`). 
   - Pattern matching updates matching tuples (or the first matching, configurable).
2. **Operations**:
   - Update user `payload` (`--json`) and/or system metadata (`--lease +60s`, `--expires ...`).
3. **CLI Syntax Example**:
   ```bash
   linda-space update --uuid sys-9b2c-4f81-a1e8 --lease +60s --json '{"status": "running"}'
   ```
