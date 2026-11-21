# Architectural Briefing: Transactions, Batching, and Session Semantics for Networked Linda-Space

**Author:** Systems & Architecture Engineering  
**Date:** November 2026  
**Status:** Architectural Specification & Design Analysis  
**Target Systems:** `linda-space`, HTTP Sidecar Daemon, Multi-Agent Swarms  

---

## Executive Summary

`linda-space` is currently implemented as an ultra-compact, file-backed Linda tuple space CLI in Go. Multi-process concurrency on a single node relies on POSIX advisory locks (`flock`) on a local JSON file (`space.json`). As agent swarms scale across distributed network nodes, containerized environments, and cloud instances, file-level locking becomes fragile and unviable over distributed filesystems (NFS/SMB).

The architectural evolution described in `projects/linda-coordination-space.md` introduces an in-memory, event-driven HTTP sidecar daemon providing non-blocking and long-polling primitives (`out`, `in`, `rd`, `count`, `clip`).

However, transitioning to a networked HTTP model exposes critical distributed coordination gaps:
1. **Network round-trip latency & TCP overhead:** Claiming or emitting multiple tuples individually creates severe head-of-line blocking and connection amplification.
2. **Partial failure during pipeline handoffs:** An agent that takes an item (`in`) and crashes before depositing the next state (`out`) drops the task into a black hole.
3. **Compound atomic coordination:** Classic Linda lacks multi-tuple coordination (e.g., claiming two complementary input tokens simultaneously or swapping state atomically).

This report evaluates how **server-side multi-op transactions**, **batch primitives**, and **session/lease-based registration transactions** should be architected for networked `linda-space`. We examine theoretical tuple space models, analyze failure modes in stateless HTTP architectures, and provide concrete API designs and Go implementation blueprints.

---

## 1. Networked HTTP Topology & The Granularity Problem

### 1.1 The File-Locked Baseline vs. Networked Daemon

In the local CLI model, atomic transactions across multiple operations can technically occur if a single CLI execution holds `flock` across consecutive mutations. In contrast, an HTTP REST/JSON API decouples operations into discrete HTTP request-response cycles:

```
[Agent A] ---- POST /in ----> [HTTP Sidecar Daemon] ---- flock / in-memory ----> [Tuple Store]
[Agent A] <--- 200 OK ------- [HTTP Sidecar Daemon]
[Agent A] (Process work / or crashes)
[Agent A] ---- POST /out ---> [HTTP Sidecar Daemon]
```

When an agent needs to:
- Claim 10 raw tasks (`in` $\times 10$),
- Read a configuration token and atomically advance a sequence counter, or
- Take an input tuple and emit two derived output tuples,

executing these as discrete HTTP requests over the network introduces:
- **Atomicity failure:** If the agent fails midway, state is partially mutated or lost.
- **Race conditions:** Another agent can interleave between step 1 and step 2, stealing correlated tuples.
- **Latency penalties:** Network round-trips ($N \times \text{RTT}$) dominate agent execution time.

---

## 2. Server-Side Multi-Op Transactions

### 2.1 The Classic Linda Atomicity Dilemma

In Gelernter’s classic Linda model, all operations are isolated single-tuple operations. When an agent requires two inputs to proceed (e.g., matching tuple `{"channel": 1}` and `{"key": 2}`), it must execute two consecutive `in` operations:
1. `in(templateA)`
2. `in(templateB)`

If Agent 1 grabs `A` and waits for `B`, while Agent 2 grabs `B` and waits for `A`, the system enters **classic dining cryptographers / circular deadlock**. In pure Linda, the only mitigation is a convention-based global lock tuple (e.g., `in({"lock": "system"})`), which serializes the entire space and destroys concurrency.

### 2.2 Atomic Multi-Op Bundles (`POST /tx/commit`)

To resolve this without maintaining distributed locks across network boundaries, `linda-space` can introduce **server-side atomic composite operations**. The client sends an array of declarative operations executed under an exclusive write lock in memory.

#### Proposed Request Payload
```json
POST /v1/space/tx
Content-Type: application/json

{
  "isolation": "serializable",
  "timeout_ms": 5000,
  "ops": [
    {
      "op": "in",
      "template": {"type": "job", "status": "raw", "stage": "ingest"},
      "bind": "claimed_job"
    },
    {
      "op": "rd",
      "template": {"type": "config", "pipeline": "main"},
      "bind": "cfg"
    },
    {
      "op": "out",
      "tuple": {
        "type": "audit_log",
        "action": "claim",
        "timestamp": 1793991200
      }
    }
  ]
}
```

#### Semantics and Guarantees
1. **All-or-Nothing Execution:** Either all `in` and `rd` templates match and all `out` tuples are deposited, or the transaction aborts with zero mutations.
2. **Read-Your-Own-Writes / Variable Binding:** Operations inside the transaction can reference attributes matched by previous operations within the same bundle via variable binding (e.g., using `claimed_job.id` in the `out` tuple).
3. **No Intermediate Visibility:** Other readers cannot observe the space while an atomic bundle is executing. Waiters waiting on `rd` or `in` are notified only after the entire batch successfully commits.
4. **Deterministic Deadlock Prevention:** For multi-`in` operations, the server sorts required matching patterns by a canonical hash/order before evaluating, preventing multi-tuple resource contention deadlocks.

---

## 3. High-Throughput Batch Primitives

Transactions guarantee ACID-like atomicity and consistency, but high-throughput data pipelines (e.g., an OpenClaw scraper ingesting 500 URLs or an LLM embedding worker draining a batch of text chunks) primarily suffer from **network serialization and lock acquisition thrashing**.

### 3.1 Batch Operations Suite

We define three native batch primitives tailored for associative tuple matching:

| Endpoint | Primitive | Description |
| :--- | :--- | :--- |
| `POST /v1/space/batch/out` | `out_many` | Atomically appends $N$ tuples to the space and notifies registered waiters in a single pass. |
| `POST /v1/space/batch/in` | `in_many` | Atomically matches, removes, and returns up to `limit` tuples satisfying a template. |
| `POST /v1/space/batch/rd` | `rd_many` | Non-destructively reads and returns up to `limit` matching tuples. |

### 3.2 Performance and Lock Contention Characteristics

In the file-locked CLI or a naive mutex-locked HTTP daemon, executing 100 `out` operations requires:
$$\text{Cost} = 100 \times (\text{HTTP Header Parsing} + \text{Acquire Lock} + \text{Append} + \text{Journal Flush} + \text{Release Lock} + \text{Notify Waiters})$$

With `batch/out`:
$$\text{Cost} = 1 \times \text{HTTP Overhead} + 1 \times \text{Lock} + (N \times \text{In-Memory Insert}) + 1 \times \text{Journal Sync} + 1 \times \text{Lock Release}$$

For journaled storage (fsync to SSD), batching achieves an immediate **$20\times$ to $50\times$ throughput multiplier**.

---

## 4. Session & Registration Transactions: Architectural Fit Analysis

A recurring question in distributed tuple systems (such as JavaSpaces, IBM TSpaces, and GigaSpaces) is whether to implement **stateful multi-request sessions** (e.g., `tx_begin` $\rightarrow$ ID $\rightarrow$ multi-request $\rightarrow$ `tx_commit`).

### 4.1 Do Stateful Sessions Make Sense for Linda?

In standard relational databases, client-held transactions (`BEGIN` ... `COMMIT`) are common because database connections are long-lived pooled TCP sockets with transaction state tracked per connection. In an HTTP-based LLM agent swarm, interactive client-held sessions present severe anti-patterns:

1. **The Zombie Holder Problem:** An LLM agent calls `BEGIN_SESSION`, claims a critical work tuple, begins an external API call or LLM inference (which can take 15–90 seconds), and crashes, runs out of context tokens, or gets killed by an orchestrator. The held tuples remain locked indefinitely or until a server lease expires.
2. **Head-of-Line Server Blocking:** If the server holds tuple locks on behalf of client session tokens, other workers querying matching templates are blocked, degrading swarm throughput.
3. **HTTP Statelessness Violation:** Load balancers, API gateways, and sidecar failovers require complex state replication or sticky routing if transactions span multiple HTTP requests.

### 4.2 The Superior Alternative: Lease-Based Visibility & Reservation ("Tombstone & Ack")

Instead of conversational `BEGIN/COMMIT` blocks, tuple spaces excel with **Time-to-Live (TTL) Leases with Heartbeats**, inspired by the Saga and Dead Letter Queue patterns:

```
[Worker Agent] ---- POST /v1/space/claim (template, lease=30s) ----> [Linda Server]
                                                                        |
                                                                  [Moves tuple to]
                                                                  [RESERVED state]
[Worker Agent] <--- 200 OK (tuple, token: "lease_89fa") -------------- |
      |
      |-- (Processing task: 12 seconds)
      |
[Worker Agent] ---- POST /v1/space/ack (token: "lease_89fa") --------> [Linda Server]
                                                                        |
                                                                  [Tuple permanently]
                                                                  [removed from space]
```

#### Lease Semantics
- **Atomic Claim with Lease:** `claim` atomically hides the tuple from other `in` or `rd` requests.
- **Heartbeat Extension:** Long-running agents can issue `POST /v1/space/renew {"token": "..."}` to extend the lease.
- **Automatic Orphan Recovery:** If the agent dies without acknowledging within the lease duration, the server automatically transitions the tuple back to `AVAILABLE` status.
- **Poison-Pill / Redelivery Counter:** Each tuple tracks `delivery_attempts`. If a tuple is claimed and timed out 3 times, it is routed to an internal dead-letter bag `{"_status": "dead_letter"}`.

---

## 5. Architectural Blueprint & Go Implementation for Server Mode

Below is the concrete software design for incorporating transactions, batching, and leased reservations into `projects/linda-cli`.

### 5.1 In-Memory Data Structures

```go
package space

import (
	"encoding/json"
	"sync"
	"time"
)

type TupleState int

const (
	Available TupleState = iota
	Reserved
	Committed
)

type StoredTuple struct {
	ID        string          `json:"id"`
	Payload   json.RawMessage `json:"payload"`
	State     TupleState      `json:"state"`
	Owner     string          `json:"owner,omitempty"`
	LeaseExp  time.Time       `json:"lease_exp,omitempty"`
	Attempts  int             `json:"attempts"`
	CreatedAt time.Time       `json:"created_at"`
}

type MemorySpace struct {
	mu      sync.RWMutex
	tuples  map[string]*StoredTuple
	waiters []*Waiter
	journal JournalWriter
}

type Waiter struct {
	ID       string
	Template any
	Keys     keyValueFilters
	Remove   bool
	Ch       chan json.RawMessage
}
```

### 5.2 Server-Side Atomic Multi-Op Handler

```go
type TxOp struct {
	Type     string          `json:"op"` // "in", "rd", "out"
	Template json.RawMessage `json:"template,omitempty"`
	Tuple    json.RawMessage `json:"tuple,omitempty"`
	Bind     string          `json:"bind,omitempty"`
}

type TxRequest struct {
	Ops []TxOp `json:"ops"`
}

func (s *MemorySpace) ExecuteTx(req TxRequest) ([]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Stage 1: Validate all preconditions and read/in matches without mutating
	type matchRecord struct {
		opIdx   int
		tupleID string
		data    json.RawMessage
	}
	var matches []matchRecord
	claimedIDs := make(map[string]bool)

	for i, op := range req.Ops {
		switch op.Type {
		case "in", "rd":
			decodedTpl, _ := decodeValue(op.Template)
			foundID := ""
			var foundTuple json.RawMessage

			for id, st := range s.tuples {
				if st.State != Available || claimedIDs[id] {
					continue
				}
				if matchTemplate(decodedTpl, st.Payload) {
					foundID = id
					foundTuple = st.Payload
					break
				}
			}

			if foundID == "" {
				return nil, fmt.Errorf("transaction aborted: op %d (%s) failed to match", i, op.Type)
			}

			if op.Type == "in" {
				claimedIDs[foundID] = true
			}
			matches = append(matches, matchRecord{opIdx: i, tupleID: foundID, data: foundTuple})

		case "out":
			if !isTuple(op.Tuple) {
				return nil, fmt.Errorf("transaction aborted: op %d invalid tuple", i)
			}
		}
	}

	// Stage 2: Commit all mutations atomically
	results := make([]json.RawMessage, len(req.Ops))
	for _, m := range matches {
		results[m.opIdx] = m.data
		if req.Ops[m.opIdx].Type == "in" {
			delete(s.tuples, m.tupleID)
			s.journal.AppendDelete(m.tupleID)
		}
	}

	for i, op := range req.Ops {
		if op.Type == "out" {
			id := newUUID()
			st := &StoredTuple{
				ID:        id,
				Payload:   op.Tuple,
				State:     Available,
				CreatedAt: time.Now(),
			}
			s.tuples[id] = st
			s.journal.AppendInsert(st)
			results[i] = op.Tuple
			s.notifyWaitersUnderLock(st)
		}
	}

	return results, nil
}
```

---

## 6. Recommendations & Roadmap for Linda-Space

1. **Prioritize Batch Primitives First:**
   Immediate implementation of `POST /v1/space/batch/out` and `POST /v1/space/batch/in` provides the highest operational ROI, solving network round-trip overhead and bulk ingestion.
2. **Implement Single-Request Atomic Transactions (`/v1/space/tx`):**
   Support atomic multi-op bundles within a single HTTP request. This completely eliminates race conditions and deadlocks without client session complexity.
3. **Reject Conversational Multi-Request Sessions in Favor of Leases:**
   Avoid stateful `tx_begin` / `tx_commit` across HTTP. Instead, adopt the **Leased Reservation (`claim` + `ack`)** pattern to guarantee crash recovery for autonomous LLM agents.
4. **Preserve CLI & File-Locked Parity:**
   Expose batch and atomic primitives in the local CLI (`linda-space tx --file tx.json`, `linda-space out --batch-file items.jsonl`) so local script execution remains identical in semantics to the HTTP sidecar server.

---
