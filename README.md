# Rockets Service

A Go service that ingests rocket messages on `POST /messages` and serves each rocket's current state over a REST API. Messages are applied in order per rocket and exactly once, even though they arrive out of order, duplicated and concurrently. State is stored in SQLite and survives restarts.

- **Correct under the delivery guarantees:** ordering and deduplication by `(channel, messageNumber)`, verified with randomized shuffle-and-duplicate tests against real SQLite and an end-to-end test with injected failures.
- **Layered:** HTTP → service (all business logic) → storage interface ← SQLite implementation.
- **One dependency:** the pure-Go SQLite driver `modernc.org/sqlite` (no C compiler needed).

How AI was used to build this is logged step by step in [`AI_WORKFLOW.md`](AI_WORKFLOW.md).

## Quick start

Requires **Go 1.25+**.

```bash
make run                     # listens on :8088, stores state in ./rockets.db
```

In another terminal, run the provided test program with its defaults:

```bash
./rockets launch "http://localhost:8088/messages"
```

Then look at the rockets:

```bash
curl -s 'localhost:8088/rockets?sort=speed&order=desc'
```

Without `make`: `go run ./cmd/server`. Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8088` | HTTP listen address |
| `-db` | `rockets.db` | SQLite file, created if missing. Delete it to start from scratch |
| `-sync` | `normal` | `normal`: a reply survives a crash of the server process. `full`: it also survives power loss, about 100× slower (see [Durability](#durability)) |
| `-max-pending` | `10000` | Early messages buffered per rocket before replying 503 |
| `-log-level` | `info` | `debug` logs every message |

## Testing

| Command | What it runs | Time |
|---|---|---|
| `make test` | All unit tests under the race detector, against real SQLite files | about 1 min |
| `go test -short ./...` | The same with fewer random seeds | about 10 s |
| `make e2e` | The real `rockets` binary against the real server binary, checked against an independent reference | about 1 min |

`make e2e` looks for the test program at `~/Downloads/lunar-backend-engineer-challenge-ai/<os>_<arch>/rockets`. Point it elsewhere with `make e2e ROCKETS_BIN=/path/to/rockets`.

| Layer | Where | Proves |
|---|---|---|
| Rule tests | `internal/service` | Each message type and each edge-case rule; decoding and validation of every field |
| Storage tests | `internal/storage/sqlite` | Round trip including NULLs and time zones, buffer dedup and order, rollback on error, every sort field and direction, data survives reopening, SQLite settings really applied |
| Ordering property test | `internal/service` | 200 seeded random orderings of a 100-message stream, each message delivered 1 to 3 times, all reach the same state as a naive in-order fold |
| Concurrency test | `internal/service` | 16 writers and 2 readers over 8 rockets under `-race`; final state still matches |
| HTTP tests | `internal/api` | Every status code, JSON shape, 404s, every sort field and order |
| End to end | `e2e/` | **Defaults:** 100,000 messages in about 53 s; all 20 rockets match field by field. **Injected failures:** 10% of requests fail (half dropped, half with the reply lost), producing about 1,000 real duplicates and many gaps; state and buffers still match |

The tests were also checked to fail when the code is broken on purpose: skipping the duplicate check, applying buffered messages across a gap, or not deleting applied messages from `pending` each make the service tests fail, and removing deduplication makes the e2e failure scenario fail. The server was also killed with `kill -9` mid-run and restarted with its state intact. See `AI_WORKFLOW.md`.

## API

### `POST /messages`

Ingests one message in the format from the challenge README. The reply is sent only after the outcome is committed to the database.

| Response | When | Sender behaviour |
|---|---|---|
| `200 {"result":"applied"}` | Applied now (with any buffered successors) | Moves on |
| `200 {"result":"buffered"}` | Arrived early; held until the gap before it fills | Moves on |
| `200 {"result":"duplicate"}` | Already processed or already buffered; ignored | Moves on |
| `400 {"error":"..."}` | Malformed JSON, unknown `messageType`, missing or invalid field | Redelivery cannot help |
| `503` + `Retry-After: 1` | This rocket's buffer of early messages is full | Redelivers later (backpressure) |
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
  "launchedAt": "2022-02-02T18:39:05.86337Z",
  "updatedAt": "2022-02-02T18:41:12.1Z",
  "lastMessageNumber": 57
}
```

`type` is the rocket model. `status` is `ACTIVE` or `EXPLODED`; when exploded, `explosionReason` holds the reason. `launchedAt` and `updatedAt` are the `messageTime` of the launch and of the last processed message, **in UTC**. `lastMessageNumber` is the publisher's `messageNumber` of the last processed message. Returns `404` for an unknown rocket, and for a rocket whose `RocketLaunched` has not been processed yet.

### `GET /rockets?sort=<field>&order=<asc|desc>`

Lists all launched rockets. `sort` is one of `id`, `type`, `speed`, `mission`, `status`, `launchedAt` or `updatedAt`. The default is `launchedAt` `asc`. Ties are always broken by `id`, so the order is stable. An unknown value returns `400`.

### `GET /debug/store`

A read-only view of the bookkeeping: per channel, `lastMessageNumber`, the number of buffered messages, and the lowest 100 buffered message numbers. It includes channels that have not launched. In production this would sit behind auth or on an admin port.

### `GET /healthz`

Returns `200 ok`.

## Design

```
cmd/server                 wiring: flags, HTTP server, graceful shutdown
internal/api               HTTP only: read request → call service → status code and JSON
internal/service           business logic: decode and validate, apply rules, ordering/buffer, get, list
internal/storage           storage interface (no SQL, no rules) + ListOptions, SortField, ChannelInfo
internal/storage/sqlite    SQLite implementation: schema, queries, transactions
internal/model             shared types only: Message and payloads, Rocket, Status
e2e                        end-to-end test with the real rockets binary
```

Dependencies point one way: `api → service → storage ← storage/sqlite`. The service never sees SQL, and the SQLite code never decides anything about ordering.

### Storage

```sql
CREATE TABLE rockets (
  channel             TEXT PRIMARY KEY,
  type                TEXT,               -- rocket model, e.g. Falcon-9
  speed               INTEGER NOT NULL DEFAULT 0,
  mission             TEXT,
  status              TEXT,               -- NULL until RocketLaunched is processed
  explosion_reason    TEXT,
  launched_at         TEXT,               -- UTC, fixed width: text order = time order
  updated_at          TEXT,
  last_message_number INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE pending (                    -- messages that arrived early
  channel        TEXT    NOT NULL,
  message_number INTEGER NOT NULL,
  message_type   TEXT    NOT NULL,
  message_time   TEXT    NOT NULL,
  body           TEXT    NOT NULL,        -- the payload, needed to apply it later
  PRIMARY KEY (channel, message_number)
);
```

Times are stored in UTC with fixed-width nanoseconds so that SQL `ORDER BY` on the text matches time order. Mixed offsets or trimmed nanoseconds would sort wrongly as text.

### Ordering and deduplication

Each message is processed in **one transaction**, and the loop that applies buffered messages runs in Go (`service.Ingest`):

| Arriving message `n` | Action |
|---|---|
| `n <= last_message_number` | Duplicate, ignored |
| `n > last_message_number + 1` | Inserted into `pending`. Already there: duplicate. Buffer over the cap: rolled back, `503` |
| `n == last_message_number + 1` | Applied, then the contiguous run that follows it in `pending`; the applied rows are deleted and the rocket row is saved |

Example, arrivals `1, 7, 5, 4, 3, 2`:

```
1       → apply 1                          last_message_number=1  pending=[]
7       → buffer                           last_message_number=1  pending=[7]
5, 4, 3 → buffer                           last_message_number=1  pending=[3 4 5 7]
2       → apply 2, 3, 4, 5; 6 is missing   last_message_number=5  pending=[7]
```

Message numbers are dense, so `n <= last_message_number` identifies every processed duplicate and no message history is kept. The duplicate check runs before the buffer cap, so a redelivered buffered message is never refused as full, and the next message never enters the buffer, so a full buffer can always drain.

**Why buffer instead of rejecting early messages?** Rejecting with non-2xx would push ordering onto the sender's retry loop: a flat 500 ms delay, observed from the binary. Worse, the binary **drops retries still pending when it finishes**, so a rejection near the end loses the message for good.

### Concurrency

SQLite allows one writer at a time, so the service holds no locks of its own:

- **Writes** go through a single connection and start with `BEGIN IMMEDIATE`. Concurrent requests queue in Go instead of failing with `SQLITE_BUSY`, and each transaction reads, decides and commits atomically.
- **Reads** use a separate read-only pool. In WAL mode they see the last committed state and never wait for the writer.
- **Atomicity:** any error, including "buffer full", rolls the whole transaction back, so the rocket row and `pending` never disagree.

### Durability

The reply is sent after commit. How durable a commit is depends on `-sync`, measured on an NVMe SSD:

| `-sync` | Per message | 100,000-message default run | A `200` survives |
|---|---|---|---|
| `normal` (default) | 0.065 ms | about 53 s | a crash or `kill -9` of the server (tested) |
| `full` | 6.4 ms | about 11 min | also an OS crash or power loss |

With `normal`, an OS crash or power loss can lose the last few commits; the database is never corrupted. `normal` is the default so that the graders' default run stays fast. Strict durability without the slowdown would need several requests per fsync (group commit), which Postgres does.

### Listing

The service validates `sort` and `order` and applies the defaults; storage runs `ORDER BY <column> <dir>, channel ASC`, with the column taken from a fixed map so that user input never reaches the SQL.

### Domain rules and edge cases

There is one rule for anything that does not fit the lifecycle: **skip it, log a warning, and still advance `last_message_number`**, so later messages are never blocked behind it. None of these cases occur with the default test program.

| Case | Behaviour |
|---|---|
| Speed goes below 0 | Applied as given, with a warning. The messages are the source of truth |
| Message after `RocketExploded` | State stays frozen |
| `RocketLaunched` that is not message 1, or message 1 that is not `RocketLaunched` | Skipped; in the second case the rocket never becomes visible |
| Same `(channel, messageNumber)` with a different body | The first copy wins; bodies are not compared |
| Negative `by` | Rejected with `400` |

### What the test program actually does

Measured by running the binary against a logging stub before designing anything. With its defaults (`--concurrency-level=3`, no delay, 100,000 messages, `--seed=444`) it runs at about 2,500 requests/s over 20 rockets. It sends **no duplicates and no gaps**, and reorders by one position at most. Duplicates and real reordering only appear when the service returns errors, which is why correctness is proven with our own randomized and fault-injecting tests.

## Limitations

| Limitation | Impact | Next step |
|---|---|---|
| One SQLite writer | Writes for all rockets are serialized; not a bottleneck at this load with `-sync normal` | Postgres with `SELECT … FOR UPDATE` per rocket row, or partition channels across instances |
| Durability depends on `-sync` | `normal` can lose the last few commits on power loss | `-sync full`, or Postgres with group commit |
| Single instance | No horizontal scaling | Partition by channel (consistent hashing, or Kafka keyed by channel) so each rocket has one owner |
| A permanently lost message blocks its rocket | State stops at the gap; once the buffer cap is reached, further early messages get 503 | A gap-repair request to the sender, or a gap timeout with an explicit `STALE` status |
| Conflicting duplicates are not detected | A resend with a different body is silently ignored | Store a body hash per message |
| No pagination, filtering, metrics or auth | Fine for 20 rockets and a local demo | `LIMIT` + cursor, `?status=` filter, Prometheus counters, auth middleware |
