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
