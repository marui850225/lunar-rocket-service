# AI Workflow Log

How AI (Claude Code) was used on this challenge: what was delegated, what was verified by hand, and where AI output was overridden. Entries are added as work happens, newest last.

## Split of ownership

- **Human:** design decisions; writes or reviews line by line `internal/rocket` and `internal/store`.
- **AI:** investigation, scaffolding, HTTP layer, scripts, docs drafts, adversarial tests.
- **Rule:** no AI-written code is committed unread.

## Log

### 1. Design grilling (AI-led interview, human decisions)

- AI read the README and ran the provided `rockets` binary against a logging stub, then analysed 100,000 captured messages.
- Finding: with default flags (seed 444) there are no duplicates or gaps, and reordering is only off by one. Duplicates appear only when the service returns non-2xx; redelivery is a flat 500 ms; pending retries are dropped at shutdown.
- Consequence decided by human: treat the binary as a smoke/load test and prove ordering/dedup with our own randomized tests.
- Human decided each design point (watermark + pending map, per-rocket locks, status codes, edge-case rules, in-memory store, API shape).
- Human override: duplicate-with-different-body handling simplified to "first wins, no body comparison" (AI had proposed comparing bodies while still pending).

### 2. Repo setup

- AI scaffolded layout, Makefile, README stub, placeholder `main.go`.

### 3. `internal/rocket` (core domain)

- AI drafted `Decode` (validation) and `Apply` (pure state rules) plus table tests, following the design doc.
- Verified: unit tests under `-race` (95.6% coverage); all 100,000 captured real messages decode and fold in order to 20 rockets / 6 exploded / 0 skipped (throwaway test, not committed).
- AI-proposed decisions not covered in the design session, accepted by human: negative `by` is invalid (400); empty `reason` on `RocketExploded` is allowed, missing is not.

### 4. `internal/store` (ordering, dedup, concurrency)

- AI drafted the store (watermark + pending map, pending cap, map RWMutex + per-rocket mutex) and its tests.
- Tests: worked example from the design doc, duplicate-of-pending, hidden-until-launched, buffer-full/backpressure, 1,000-seed shuffle+duplicate property test against a naive in-order oracle, 16-writer + 2-reader concurrency test under `-race`.
- Verified the tests can fail (mutation testing, by hand): removing applied-dedup, draining pending out of order, and removing the per-rocket lock each make the suite fail (the last via the race detector and a fatal concurrent map write).

### 5. `internal/api` + `main.go`

- AI drafted handlers (status codes per design: 200 applied/buffered/duplicate, 400 invalid, 503 + Retry-After when the buffer is full, 500 without leaking internals), sorting with id tiebreak, JSON shape, and `main.go` with timeouts and graceful shutdown on SIGINT/SIGTERM.
- Tests: `httptest` for every status code, a fake store for the 500 path, exact JSON shape, 404 for unknown/unlaunched, every sort field and order, 400 for bad sort/order, 405 on wrong method.
- Verified against the real binary (defaults, 100,000 messages, 45 s): no errors on either side; final state of all 20 rockets matches an independent Python fold of an earlier capture (same seed) field by field.

### 6. Debug endpoint (human request)

- Human asked how to inspect the in-memory store; AI proposed and added read-only `GET /debug/store` (per-channel watermark, pending count, lowest 100 pending numbers, including unlaunched channels).
- While demoing it, AI found a real bug in its own `main.go`: "listening" was logged before the bind, so a port conflict looked like a successful start. Fixed by binding with `net.Listen` first, then logging.
