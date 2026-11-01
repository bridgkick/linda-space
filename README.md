# linda-space

`linda-space` is a standalone Go CLI for a local, file-backed Linda tuple space. It is designed for shell scripts and LLM agents that need a tiny coordination primitive without a database or daemon.

The shared state is one JSON file. Every operation takes an OS-level `flock`, so separate CLI processes can safely use the same space. `in` removes a match while holding the exclusive lock, making task claiming atomic.

## Install

Requires Go 1.23 or newer:

```sh
go install github.com/bridgkick/linda-space@latest
```

Or build from a checkout:

```sh
go build -o linda-space .
```

## Operations

```sh
# Add objects or arrays.
linda-space out --space /tmp/jobs.json '{"type":"job","status":"raw","id":42}'

# `in` claims and removes the first matching tuple.
linda-space in --space /tmp/jobs.json '{"type":"job","status":"raw"}'

# `rd` reads without removing.
linda-space rd --space /tmp/jobs.json '{"type":"job"}'

# Count all tuples or only matching tuples.
linda-space count --space /tmp/jobs.json
linda-space count --space /tmp/jobs.json '{"status":"raw"}'

# Remove all matches; print the number removed.
linda-space clip --space /tmp/jobs.json '{"status":"done"}'
```

JSON can be passed as the final argument, with `--json`, with `--file`, or through stdin:

```sh
printf '%s\n' '{"type":"signal"}' | linda-space out --space /tmp/jobs.json
linda-space in --space /tmp/jobs.json --file template.json
```

## Matching

Templates recursively match subsets of JSON objects: every key in a template must be present with the same value in the candidate, while extra candidate keys are allowed. Nested objects use the same rule. Arrays must have the same length and match positionally. Top-level tuples can be JSON objects or arrays; scalar top-level values are rejected.

### Key-Value Filtering (`-k` / `--key`)

Tuples can also be filtered using dot-path key-value flags (`-k <path>=<value>` or `--key <path>=<value>`). Key filters can be repeated and used on their own (without a JSON template) or combined with a template:

```sh
# Read or take tuples solely using dot-path filters
linda-space rd --space /tmp/jobs.json -k status=raw -k meta.team=infra
linda-space in --space /tmp/jobs.json -k status=raw

# Count and clip with key filters
linda-space count --space /tmp/jobs.json -k status=done
linda-space clip --space /tmp/jobs.json -k status=cancelled
```

Scalar values in filter targets (e.g. numbers, booleans, null) are parsed as JSON scalars with fallback to raw strings (e.g., `-k priority=2`, `-k active=true`, `-k ref=null`, `-k status=raw`).

> **Note on dot escaping:** Dot-path key traversal does not yet support escaped dots (`.`). Traversal splits paths on literal `.` characters. Literal dot escaping in key names will be implemented later via slash-escaping (`\.`).


## Blocking and exit status

`in` and `rd` do an immediate check by default. Add `--wait` to long-poll until a matching tuple arrives:

```sh
linda-space in --space /tmp/jobs.json --wait 30s --fail-on-none '{"status":"raw"}'
```

Without `--fail-on-none`, a timeout or immediate miss exits 0 and prints nothing. With it, a miss exits 1. Invalid input and I/O failures also exit 1; unknown commands or flag syntax errors exit 2.

Run `linda-space --help` for complete, agent-friendly command documentation.

## Storage format

The file contains an object with a `tuples` array:

```json
{"tuples":[{"type":"job","status":"raw"},["event",{"kind":"created"}]]}
```

Do not edit the file while `linda-space` is operating on it. The format is intentionally simple and human-inspectable, but writers should use the CLI so locking and atomic `in` semantics are preserved.
