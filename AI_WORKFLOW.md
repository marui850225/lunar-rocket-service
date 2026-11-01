# AI Workflow

How I built this service with an AI coding agent (Claude Code): what I delegated, what I verified myself, where I overrode the AI, and how quality was kept. The structured part is first; the full step-by-step log, written as the work happened, is at the end.

## The workflow

1. **Understand before coding.** The AI read the challenge and interviewed me about the design, one question at a time, each with a recommendation; I made every decision. In parallel it ran the provided `rockets` program against a logging stub and analysed 100,000 real messages, so the design rests on measured behaviour, not on the README alone.
2. **Write the design down.** A design document (architecture, ordering, concurrency, persistence, API, limitations) before any code.
3. **Build in small, tested steps.** One layer per step, each a commit with its tests passing (29 commits). The AI drafted code and tests; I decided what each step should do and approved moving on.
4. **Verify beyond "tests pass".** Mutation checks (break the code on purpose, the tests must fail), the real program end to end, fault injection, a crash-and-replay test, and benchmarks on the real disk.
5. **Review.** An automated code review, then fixes with a test each. A linter (staticcheck, all checks).
6. **Redesign and simplify when the design is not right.** I replaced the in-memory store with SQLite and a service layer, then cut the code to a minimal version I can explain line by line. Removed features are documented with how to add them back.
7. **Understand every line.** I walked through the code file by file. When an answer did not convince me, the code changed.
8. **Keep a log as I go.** Every step below was written when it happened, including the AI's mistakes.

## Who did what

| | Me | The AI |
|---|---|---|
| Problem understanding | Decided how to handle out-of-order messages, duplicates, status codes, edge cases | Read the README, ran and measured the real program, proposed options with trade-offs |
| Design | Every decision; the SQLite redesign; the layers; naming | Design document, sketches, measurements (e.g. durability cost) |
| Code | Reviewed through questions and the line-by-line walkthrough; directed refactors | Wrote the code and the tests |
| Verification | Ran the demo myself; judged whether tests prove the right thing | Wrote and ran tests, mutation checks, e2e, crash test, review, lint |
| Documentation | Decided what goes in | Drafted README, design doc, this log |

## Where I overrode or redirected the AI

| AI's version | My decision |
|---|---|
| Compare bodies of duplicate messages while buffered | Simpler: the first copy wins, bodies are never compared |
| In-memory store with per-rocket locks | Relational SQLite store: a `rockets` table and a `pending` buffer table |
| Watermark column called `applied` | `last_message_number`, the publisher's own term; rocket model column stays `type` |
| Domain package with types and rules together | `model` holds types only; rules live in a `service` layer that I asked for |
| Storage interface `Tx` mixing two tables, reads on `Storage` | One interface per table; `Storage` only runs transactions; the `Tables` struct kept by my preference |
| Debug data type in the storage package | Moved to `model` |
| Many hardening features (graceful shutdown, buffer cap, flags, read pool, …) | Removed for a minimal, explainable version; documented as a backlog |
| README run command `./rockets launch …` | I found it does not work; replaced with `make publish` |

It also went the other way:

- My first SQLite sketch stored only message numbers in the buffer table. The AI pointed out that the body is needed to apply a message later, and that draining must loop over the whole contiguous run. I accepted both and decided the loop stays in Go, not SQL.
- I proposed keeping applied rows in the `pending` table (with an `applied` flag) for debugging. The AI showed it would be a partial, misleading history (in-order messages never enter that table), and I dropped the idea.

## How quality was kept

- **Decisions before code:** the design interview and the design document.
- **Small steps:** every commit builds and passes its tests.
- **Tests at every layer:** message rules, SQLite storage, the service, HTTP handlers.
- **Tests that hunt bugs:** a property test (100 random orderings with duplicates) against a deliberately naive oracle; a concurrency test under the race detector.
- **Real conditions:** e2e with the real `rockets` binary and the real server, including 10% injected failures (about 1,000 real duplicates per run) and a by-hand out-of-order and duplicate scenario.
- **Tests that can fail:** mutation checks for dedup, ordering and locking; each broken version made the tests fail.
- **Crash safety:** `kill -9` mid-run, restart, replay the whole stream: 0 differences from an uninterrupted run.
- **Review and lint:** an automated code review (10 findings, all fixed with tests) and staticcheck with all checks (0 issues).

## AI mistakes that were caught

| Mistake | How it was caught |
|---|---|
| Logged "listening" before the port was bound, so a port conflict looked like success | Seen while demoing; fixed |
| First e2e test never produced a real duplicate | AI noticed 0 duplicates in its own results; added "reply lost" failures |
| A benchmark on `/tmp` (a RAM disk) hid a 100× durability cost | Re-measured on the real disk before wiring it in |
| A README claim about mutation tests outlived the code it described | Re-ran the checks on the new code before keeping the claim |
| 10 issues, including a possible permanent freeze after a panic | Code review |
| Launched a maximum-effort review on top of a running one, using up my session limit | Logged; lesson: ask before expensive operations |
| One unexplained e2e failure | Could not reproduce in 6 runs; the test now prints diagnostics |
| Its check server failed to start and its test traffic went into my database | Reported immediately with a clean-up command; lesson: confirm a server is up before using it |
| Old header of this file claimed "no AI code committed unread" | Not accurate: I read the code line by line after implementation; corrected here |

## Step-by-step log

Written as the work happened, oldest first. "Human" is me.

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

### 7. End-to-end test

- Plan said `scripts/e2e.sh` (bash + Python reference). AI proposed, human accepted, a Go test (`e2e/`, build tag `e2e`, `make e2e`) instead: graders need only Go, and it is cross-platform.
- Setup: real `rockets` binary → recording proxy → real server binary. The reference is a from-scratch fold of the messages the server accepted (no shared code), compared with `GET /rockets` and `GET /debug/store`.
- Scenarios: defaults (exactly how graders run it), and 10% injected failures.
- AI caught a flaw in its own first version: injected failures only dropped requests, so the server never saw a duplicate (0 in the results). Added a "reply lost" mode (server processes the message, client gets 500) which produces ~1,000 real at-least-once duplicates per run.
- Verified the e2e can fail: with dedup removed from the store, `TestInjectedFailures` fails.

### 8. README

- AI drafted the README from the design doc and the implementation; commands and example outputs re-run before committing (quick start against the real binary, sort error message, test timing).
- AI lowered `go.mod` from `go 1.27.3` (the local toolchain) to `go 1.25`, the oldest version that has every feature used (`sync.WaitGroup.Go`), so graders aren't forced onto the newest release.

### 9. Redesign: SQLite + service layer (human-led)

Human decisions after reviewing the in-memory design:
- Replace the in-memory map with a relational SQLite store: a `rockets` table (one column per field) and a `pending` buffer table. AI pointed out two gaps in the first sketch, both accepted: the buffer must store the message body (not only its number), and draining must loop over the contiguous run. Human decided the loop stays in Go, not SQL.
- Human renamed the watermark: `applied` described our action; the column is `last_message_number`, after the publisher's `messageNumber` (and matching the API's `lastMessageNumber`). The rocket model column stays `type`, the publisher's name.
- Human asked for layers: `model` (types only; human chose to move `Decode`/`Apply` out of it), `service` (all business logic: ingest/buffer, get, list sort/order/defaults), `storage` (interface), `storage/sqlite` (implementation). List sorting moves into SQL `ORDER BY`, chosen via a fixed field→column map.

Step 1 — `model`, `storage` interface, `storage/sqlite`:
- AI: `modernc.org/sqlite` (pure Go, no cgo); pinned to v1.59.0 because the latest release raised the minimum Go to 1.26.
- AI: one writer connection with `BEGIN IMMEDIATE` (serializes writes in Go instead of `SQLITE_BUSY`), separate read-only pool, WAL, `synchronous=FULL` so a 200 means on disk.
- AI decision to flag: times are stored in UTC with fixed-width nanoseconds so text order equals time order (mixed offsets and trimmed nanos would sort wrong as text); the API will therefore return UTC timestamps (same instants).
- Tests: round trip incl. NULLs and time zones, pending dedup/order/delete, rollback on error, every sort field and direction with tiebreak and a mixed-timezone case, SQL-injection-shaped sort rejected, reopen persistence, and a test that the pragmas really applied (driver ignores unknown DSN params silently).

Step 2 — `service`:
- AI moved `Decode`/`Apply` into `service` (decode.go, apply.go; old tests ported mechanically, unchanged assertions) and wrote `Ingest` (one storage transaction per message: watermark check, buffer with cap checked after the duplicate check, apply the contiguous run, delete it), `GetRocket` (hides unlaunched), `ListRockets` (sort/order validation and defaults → `storage.ListOptions`).
- Tests against real SQLite: the 1,7,5,4,3,2 example step by step, duplicate-of-pending keeps first copy, hidden until launched, buffer full incl. rollback of the rejected insert, invalid input never touches storage, 200-seed shuffle+duplicate property test, 16-writer + 2-reader concurrency test under `-race`.
- AI measured a problem before wiring it in: with `synchronous=FULL` one ingest costs 6.4 ms on NVMe (~155 msg/s, ~11 min for the graders' default run) vs 0.065 ms with `NORMAL`. A first benchmark on `/tmp` looked fine only because `/tmp` is tmpfs here. Decision handed to the human.

Step R3 — wiring and cleanup:
- Human chose `synchronous=NORMAL` as the default after the measurement was explained; AI added `-sync normal|full` (validated; a test checks both modes really set the pragma).
- `api` now calls only `service` through a small interface (errors mapped: ErrInvalid→400, ErrNotFound→404, ErrInvalidSort→400, ErrBufferFull→503, other→500 without details). `main.go` gets `-db` and `-sync`; DB closed after the HTTP server drains. `internal/rocket` and `internal/store` deleted.
- Evidence the refactor kept behaviour: the API tests passed with only setup, UTC timestamps and the debug field name changed; `TestListRockets` (now sorted by SQL) passed unchanged.
- `make e2e` on the real disk: defaults 100,000 messages in 53 s (in-memory was 52 s), all 20 rockets match; injected failures: 1,061 real duplicates, all rockets and buffers match.
- Crash test: server killed with `kill -9` mid-run (after ~14,700 messages), restarted on the same file: all 20 rockets and their state were there (WAL replayed), nothing left pending.

Step R4 — docs:
- README rewritten for the SQLite design (flags table, schema, transaction flow, durability table, UTC timestamps).
- AI caught itself about to carry over a stale claim: the mutation tests had been run on the deleted in-memory store. Re-ran them on the new service before keeping the claim: skipping the duplicate check, applying across a gap, and not deleting applied rows from `pending` each fail the service tests.

### 10. Strict code review and fixes

- Human ran `/code-review`; it returned 10 findings. AI then launched a second, maximum-effort review on top without asking; its ~9 parallel agents exhausted the session limit and all stopped early. Lesson recorded: don't stack expensive reviews, ask first. The first review's findings were checked against the code by hand (all 10 accurate; line numbers corrected).
- Human: "fix everything". Fixes, each with a test where it made sense:
  1. Transaction rolled back on panic too (before: the single writer connection could stay inside an open transaction and freeze all writes). Test confirmed failing without the fix.
  2. Drain reads buffered successors one at a time and stops at the first gap (before: every in-order message loaded and decoded everything buffered above it). Benchmark with 9,000 messages behind a gap: 0.08 ms per message (review measured 16.7 ms before). Keeps the human's rule that the loop is in Go, not SQL.
  3. Errors carry channel and message number; per-message debug log restored (the README claimed it; the refactor had dropped it).
  4. Shutdown waits for running handlers before closing the DB, cancelling them after 10 s. Tested with SIGTERM under full load: clean exit, no "database is closed".
  5. A cancelled client request is a 503 logged at info, not an ERROR "ingest failed".
  6. One range DELETE instead of one per message.
  7. Buffered payloads use a fixed, tagged storage format; unknown or missing fields are errors, not zero values.
  8. Overflow-safe speed arithmetic; an overflowing change is skipped and logged.
  9. Debug view reads one snapshot in two queries instead of N+1 queries.
  10. Message type is derived from the payload (`Payload.Type()`); the duplicated type switches are gone.
- Verified: `make test` and `make e2e` pass (defaults 100,000 messages, all 20 rockets match; injected failures 1,064 duplicates). The default e2e run took 65 s vs 53 s earlier; AI benchmarked old vs new code side by side before reporting it: same speed (machine slower today), not a regression.

### 11. Second test round (fresh clone)

- Human asked for another round of testing. AI ran it on a fresh clone from GitHub, as a grader would: `gofmt`, `go vet` (incl. e2e), `make test`, `make e2e` all pass (defaults: 20 rockets match field by field; injected failures: 1,064 duplicates, all rockets and buffers match).
- New crash-and-replay test: server killed with `kill -9` 20 s into a run, restarted on the same database (20 rockets, ~33,900 messages processed), then the whole test program replayed from message 1. Final state compared with a separate uninterrupted run: 0 differences in type, speed, mission, status, reason and lastMessageNumber; 6/6 exploded; nothing pending; no errors or panics in any server log.

### 12. Test cleanup (human request: remove unnecessary tests)

- AI reviewed all six test files and removed only cases already proven elsewhere, then checked with a coverage diff that nothing lost coverage (api 89.1%, service 87.7%, sqlite 84.2% before and after; 0 code blocks covered before and not after).
- Removed: `TestLaunched` (covered by the apply table), `TestDecodeRealSample` (e2e decodes 100,000 real messages), sqlite `TestEmptyList` and `TestChannels` (the API tests run the same paths against real SQLite), two of three 400 cases in the API status-code table (validation is tested in `decode`; one case proves the 400 mapping), four per-field sort cases in the API list test (every field and direction is tested at the SQLite layer).
- Folded the two overflow cases into the apply table instead of an awkward `append`.
- Random-order property test: 200 → 100 seeds. `make test` went from ~100 s to 53 s.

### 13. Simplification to a minimal version (human request)

- Human wants to explain every line, so asked for a minimal working version. AI reviewed all source files itself (no parallel agents) and split candidates into "simpler code, same behaviour" (done directly) and "remove a feature" (human chose).
- Same behaviour, simpler code: no graceful shutdown in `main.go` (safe because WAL + idempotent redelivery protect data; tested with `kill -9`); `defer tx.Rollback()` idiom instead of a committed flag (still panic-safe, test kept); buffered payloads stored with JSON tags on the model instead of a separate strict format; no special case for cancelled requests; API uses `*service.Service` directly instead of an interface; `Result` is a string type; `/healthz` removed (e2e waits on `GET /rockets`).
- Features removed by human choice: `-sync` (hard-coded NORMAL), `-log-level` (with the debug and negative-speed logs), `-max-pending` and the buffer cap with its 503, the separate read-only pool (one SQLite connection for everything), the int64 overflow check. Kept by human choice: `/debug/store`, the lifecycle rules.
- Each removal is listed in the README's Limitations table with what it would take to add back.
- Verified: `go vet`, `make test` (race) and `make e2e` (defaults: all 20 rockets match; injected failures: 1,062 duplicates, all match) pass.

### 14. Clearer storage interface (human request)

- While walking through the code, the human pointed out that the `Tx` interface was confusing: named after a database transaction, but holding data operations on two different tables (no Commit/Rollback in it). AI agreed and offered three options; human chose one interface per table.
- `Tx` replaced by `Tables{Rocket RocketTable; Pending PendingTable}`: `RocketTable` has `Get`/`Save`, `PendingTable` has `Add`/`Get`/`Delete`. `InChannel` stays the transaction. The service now reads `tables.Pending.Add(m)`, `tables.Rocket.Save(r)`. Pure rename and regroup; `make test` passes.
- Follow-up (human questions): the `Tables` struct was only a bundle, so `InChannel` now passes the two tables as two parameters (`func(rockets RocketTable, pending PendingTable) error`); the service calls `rockets.Get()`, `pending.Add(m)`. `ChannelInfo` moved from `storage` to `model`: it is data that travels through every layer, while `storage` keeps only the database contract and query options. `make test` passes.
- Follow-up (human idea): `GetRocket` and `ListRockets` only touch the rockets table, so they moved onto `RocketTable` (`Get`, `Save`, `List`); the debug view moved onto `PendingTable` as `Summary`. `Storage` now only runs transactions (`InTx` + `Close`). Tables are no longer bound to one channel (the channel is an argument), and reads also go through `InTx`, which costs nothing with one SQLite connection. Removed the duplicate `Get`. `make test` and `make e2e` pass.
- Follow-up (human preference): brought the `Tables` struct back, so `InTx(ctx, fn func(Tables) error)`; fields `Rockets` and `Pending`, read as `tables.Rockets.Get(id)`, `tables.Pending.Add(m)`. `make test` passes.

### 15. Comment review and lint (human request)

- AI reviewed every comment in source and tests: removed stale terms from older versions ("watermark", "snapshots", "single writer connection", "before the fix"), an inaccurate claim ("model contains no behaviour" though it has `Type()`/`Launched()`), and shortened wordy ones (`ingest`, `Open`, the drain loop).
- Lint: downloading staticcheck was blocked by the permission system (external code), so AI ran `go vet`, `gofmt -s` and scripted checks of common staticcheck/golint rules. Fixed: doc comments for the five exported payload types; two error strings started with a capital letter (ST1005). Left as is, with reason: test `t.Errorf` messages (not error strings), interface method implementations (documented on the interface), `Close`.
- Human installed staticcheck; AI ran it: 0 issues with the default checks and with `-checks all`, including the e2e package. Added `make lint`.

### 16. Readable e2e output (human request)

- The e2e test now reports five numbered steps per scenario: the scenario and its settings; the server it started; what the publisher sent (requests, injected failures split into dropped vs reply lost, forwarded, the server's applied/buffered/duplicate counts); what the reference expects (unique messages, channels, launched/exploded, pending, and how many published messages never reached the server); and the comparison (rockets, channels and fields compared, mismatches). One SUMMARY line per scenario.
- One run of the failure scenario failed during publishing and could not be explained: the test only printed the path of the rockets log, which lives in a temp dir deleted after the test. Six reruns passed. AI did not hide it: the test now prints the last 20 lines of the rockets log on that failure (verified with a fake rockets that exits with an error), so a recurrence will show its cause. Root cause still unknown.

### 17. README demo section (human request)

- AI added a "Demo" section: start the server, run the real program, read and sort rockets, send out-of-order and duplicate messages by hand, error answers, restart survival. Explained why 20 rockets is correct (the program's default seed always uses 20 channels) and why `pending` is empty after a run (rows are deleted as soon as they are applied).
- AI verified every demo command by extracting it from the README and running it; found and fixed one bug (`grep -A5` cut off the pending numbers).
- AI mistake: its own check server failed to start because the human's `make run` already held port 8088, and AI did not check before sending; the demo traffic (one `rockets` run and two test rockets) went into the human's database. Reported immediately with a clean-up command. Lesson: start check servers on a free port and confirm they are up.
- Human found the README's `./rockets launch …` failed (the program lives in the challenge download, not the repo) and asked for exact commands. AI added `make publish` (uses the same `ROCKETS_BIN` default as `make e2e`), documented the exact full command and how the platform folder is chosen; tested `make publish` against its own server on port 8099 (confirmed started; 20 rockets, 100,000 messages processed).
- The by-hand out-of-order and duplicate demo removed from the README became an e2e scenario, `TestOutOfOrderAndDuplicates`, against the real server binary: buffered, buffered, 404, pending [2 3], applied, speed 650, duplicate, unchanged. Verified it fails with dedup broken (3 WRONG lines). `make e2e` passes all three scenarios.
