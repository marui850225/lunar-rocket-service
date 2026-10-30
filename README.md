# Rockets Service

A Go service that ingests rocket messages on `POST /messages` and serves each rocket's current state over a REST API. Messages are applied in order per rocket and exactly once, even though they arrive out of order, duplicated and concurrently.

- **Correct under the delivery guarantees:** ordering and deduplication by `(channel, messageNumber)`, verified with 1,000 randomized shuffle-and-duplicate runs and an end-to-end test with injected failures.
- **Standard library only:** no third-party dependencies.
- **Known trade-off:** state is in memory, so a restart loses it (see [Limitations](#limitations)).

How AI was used to build this is logged step by step in [`AI_WORKFLOW.md`](AI_WORKFLOW.md).

## Quick start

Requires **Go 1.25+**.

```bash
make run                     # listens on :8088
```

In another terminal, run the provided test program with its defaults:

```bash
./rockets launch "http://localhost:8088/messages"
```

Then look at the rockets:

```bash
curl -s 'localhost:8088/rockets?sort=speed&order=desc'
```

Without `make`: `go run ./cmd/server` (flags: `-addr :8088`, `-max-pending 10000`, `-log-level info|debug|warn|error`).

## Testing

| Command | What it runs | Time |
|---|---|---|
| `make test` | All unit tests under the race detector | a few seconds |
| `make e2e` | The real `rockets` binary against the real server binary, checked against an independent reference | ~1 min |

`make e2e` looks for the test program at `~/Downloads/lunar-backend-engineer-challenge-ai/<os>_<arch>/rockets`. Point it elsewhere with `make e2e ROCKETS_BIN=/path/to/rockets`.

What the tests prove:

| Layer | Where | Proves |
|---|---|---|
| Rule tests | `internal/rocket` | Each message type and each edge-case rule; decoding and validation of every field |
| Ordering property test | `internal/store` | 1,000 seeded random orderings of a 200-message stream, each message delivered 1 to 3 times, all reach the same state as a naive in-order fold |
| Concurrency test | `internal/store` | 16 writers and 2 readers over 8 rockets under `-race`; final state still matches |
| HTTP tests | `internal/api` | Every status code, JSON shape, 404s, every sort field and order |
| End to end | `e2e/` | **Defaults:** 100,000 messages; all 20 rockets match field by field. **Injected failures:** 10% of requests fail (half dropped, half with the reply lost), which produces about 1,000 real duplicates and many gaps; state and buffers still match |

The tests were also checked to fail when the code is broken. Removing deduplication, draining the buffer out of order, or removing the per-rocket lock each make the suite fail (see `AI_WORKFLOW.md`).

## API

### `POST /messages`

Ingests one message in the format from the challenge README.

| Response | When | Sender behaviour |
|---|---|---|
| `200 {"result":"applied"}` | Applied now (with any buffered successors) | Moves on |
| `200 {"result":"buffered"}` | Arrived early; held until the gap before it fills | Moves on |
| `200 {"result":"duplicate"}` | Already applied or already buffered; ignored | Moves on |
| `400 {"error":"..."}` | Malformed JSON, unknown `messageType`, missing or invalid field | Redelivery cannot help |
| `503` + `Retry-After: 1` | This rocket's buffer of early messages is full (10,000) | Redelivers later (backpressure) |
| `500` | Unexpected internal error | Redelivers |

### `GET /rockets/{channel}`

```json
{
  "id": "193270a9-c9cf-404a-8f83-838e71d9ae67",
  "type": "Falcon-9",
  "speed": 3500,
  "mission": "ARTEMIS",
  "status": "ACTIVE",
  "explosionReason": null,
  "launchedAt": "2022-02-02T19:39:05.86337+01:00",
  "updatedAt": "2022-02-02T19:41:12.1+01:00",
  "lastMessageNumber": 57
}
```

`status` is `ACTIVE` or `EXPLODED`. When the rocket exploded, `explosionReason` holds the reason. `launchedAt` and `updatedAt` are the `messageTime` of the launch and of the last applied message. Returns `404` for an unknown rocket, and for a rocket whose `RocketLaunched` has not been applied yet.

### `GET /rockets?sort=<field>&order=<asc|desc>`

Lists all launched rockets. `sort` is one of `id`, `type`, `speed`, `mission`, `status`, `launchedAt` or `updatedAt`. The default is `launchedAt` `asc`. Ties are always broken by `id`, so the order is stable. An unknown value returns `400`.

### `GET /debug/store`

A read-only view of the internal bookkeeping: per channel, the `applied` watermark, the number of buffered messages, and the lowest 100 buffered message numbers. It includes channels that have not launched. In production this would sit behind auth or on an admin port.

### `GET /healthz`

Returns `200 ok`.

## Design

```
cmd/server        wiring: flags, HTTP server, graceful shutdown
internal/api      HTTP only: decode → store → status code; sorting; JSON
internal/store    ordering, dedup, buffering, locking
internal/rocket   pure domain: message decoding/validation and Apply rules
e2e               end-to-end test with the real rockets binary
```

### Ordering and deduplication

Each rocket keeps a watermark `applied` (every message up to this number has been applied) and a `pending` map of messages that arrived early.

- `n <= applied`, or `n` already in `pending`: a duplicate, ignored.
- Otherwise the message goes into `pending`, then the store drains it while `pending[applied+1]` exists.

Example, arrivals `1, 3, 2, 2, 5, 4`:

```
1 → apply 1            applied=1 pending={}
3 → wait for 2         applied=1 pending={3}
2 → apply 2, apply 3   applied=3 pending={}
2 → duplicate          applied=3 pending={}
5 → wait for 4         applied=3 pending={5}
4 → apply 4, apply 5   applied=5 pending={}
```

Message numbers are dense, so the watermark alone identifies applied duplicates, and no history is stored. Memory per rocket is proportional to the current gap, not to the number of messages seen. A map is enough because draining only ever asks "is the next number here?".

The buffer is **capped** at 10,000 messages per rocket. Beyond that an early message gets `503`, and the sender's own redelivery acts as backpressure. The message that fills the gap is always accepted, so a full buffer can always drain.

**Why buffer instead of rejecting early messages?** Rejecting with non-2xx would push ordering onto the sender's retry loop: a flat 500 ms delay, observed from the binary. Worse, the binary **drops retries still pending when it finishes**, so a rejection near the end loses the message for good. Buffering handles any arrival order in one pass.

### Concurrency

A `sync.RWMutex` guards the map of rockets, and each rocket has its own `sync.Mutex`. Messages for different rockets are processed in parallel. Messages for the same rocket are serialized, which is the boundary that ordering needs anyway. Reads copy a rocket's state under its lock, so handlers never touch shared state unlocked. Lock order is always the map before the rocket.

Rejected alternatives: one global mutex (unrelated rockets would block each other), and one goroutine per rocket (lifecycle and shutdown complexity with no benefit at this scale).

### Domain rules and edge cases

There is one rule for anything that does not fit the lifecycle: **skip it, log a warning, and still advance the watermark**, so later messages are never blocked behind it. None of these cases occur with the default test program; they are defensive choices.

| Case | Behaviour |
|---|---|
| Speed goes below 0 | Applied as given, with a warning. The messages are the source of truth; clamping would make every later update wrong |
| Message after `RocketExploded` | State stays frozen (a destroyed rocket must not accelerate on a dashboard) |
| `RocketLaunched` that is not message 1, or message 1 that is not `RocketLaunched` | Skipped; in the second case the rocket never becomes visible |
| Same `(channel, messageNumber)` with a different body | The first copy wins; bodies are not compared |
| Negative `by` | Rejected with `400` (a "decrease by -5" is malformed) |

### What the test program actually does

Measured by running the binary against a logging stub before designing anything. With its defaults (`--concurrency-level=3`, no delay, 100,000 messages, `--seed=444`) it runs at about 2,500 requests/s over 20 rockets. It sends **no duplicates and no gaps**, and only reorders by one position (from its 3 concurrent senders). Duplicates and real reordering only appear when the service returns errors. That is why correctness is proven with our own randomized and fault-injecting tests rather than by the default run alone.

## Limitations

Each of these was chosen to keep the design simple and verifiable within the time budget.

| Limitation | Impact | Next step |
|---|---|---|
| In-memory state | A restart loses everything, including acknowledged messages | SQLite or Postgres behind the store: `UNIQUE (channel, message_number)`, apply and advance the watermark in one transaction, reply only after commit |
| Single instance | No horizontal scaling | Partition by channel (consistent hashing, or a log keyed by channel such as Kafka) so each rocket has one owner |
| A permanently lost message blocks its rocket | State stops at the gap; once the buffer cap is reached, further early messages get 503 | A gap-repair request to the sender, or a gap timeout with an explicit `STALE` status |
| Conflicting duplicates are not detected | A resend with a different body is silently ignored | Store a body hash per message |
| No pagination, filtering, metrics or auth | Fine for 20 rockets and a local demo | `limit`/`cursor`, `?status=` filters, Prometheus counters, auth middleware |
