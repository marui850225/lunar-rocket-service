# AI Workflow

How I built this service with an AI coding agent (Claude Code): what I delegated, what I verified myself, where I overrode the AI, and how quality was kept. The structured part is first; the full step-by-step log, written as the work happened, is at the end.

## The workflow

1. **Understand before coding.** The AI read the challenge and interviewed me about the design, one question at a time; I made every decision. In parallel it ran the provided `rockets` program against a logging stub and analysed 100,000 real messages, so the design rests on measured behaviour, not on the README alone.
2. **Write the design down.** A design document (architecture, ordering, concurrency, persistence, API, limitations) before any code.
3. **Build in small, tested steps.** One layer per step, each a commit with its tests passing (29 commits). The AI drafted code and tests; I decided what each step should do and approved moving on.
4. **Verify beyond "tests pass".** Mutation checks (break the code on purpose, the tests must fail), the real program end to end, fault injection, a crash-and-replay test, and benchmarks on the real disk.
5. **Review.** An automated code review, then fixes with a test each. A linter (staticcheck, all checks).
6. **Redesign and simplify when the design is not right.** I replaced the in-memory store with SQLite and a service layer, then cut the code to a simplified version.
7. **Understand every line.** I walked through the code file by file. When an answer did not convince me, the code changed.
8. **Keep a log as I go.** Every step below was written when it happened, including the AI's mistakes.

## Who did what

| | Me | The AI |
|---|---|---|
| Problem understanding | Decided how to handle out-of-order messages, duplicates, status codes, edge cases | Ran and measured the real program, proposed options with trade-offs |
| Design | Every decision; the SQLite redesign; the layers; naming | Design document, sketches, measurements (e.g. durability cost) |
| Code | Reviewed through questions and the line-by-line walkthrough; directed refactors | Wrote the code and the tests |
| Verification | Ran the demo myself; judged whether tests prove the right thing | Wrote and ran tests, mutation checks, e2e, crash test, review, lint |
| Documentation | Decided what goes in | Drafted README, design doc, this log |

## How quality was kept

- **Decisions before code:** the design interview and the design document.
- **Small steps:** every commit builds and passes its tests.
- **Tests at every layer:** message rules, SQLite storage, the service, HTTP handlers.
- **Tests that hunt bugs:** a property test (100 random orderings with duplicates) against a deliberately naive oracle; a concurrency test under the race detector.
- **Real conditions:** e2e with the real `rockets` binary and the real server, including 10% injected failures (about 1,000 real duplicates per run) and a by-hand out-of-order and duplicate scenario.
- **Tests that can fail:** mutation checks for dedup, ordering and locking; each broken version made the tests fail.
- **Crash safety:** `kill -9` mid-run, restart, replay the whole stream: 0 differences from an uninterrupted run.
- **Review and lint:** an automated code review (10 findings, all fixed with tests) and staticcheck with all checks (0 issues).

## Step-by-step log

Written as the work happened (29 Oct to 1 Nov), condensed to one line per step.

| # | Step | Key facts |
|---|---|---|
| 1 | Design interview | I decided each design point; the AI measured the real program: no duplicates, reordering by one at most, 500 ms redelivery, retries dropped at exit |
| 2 | Domain rules | Decoding, validation and apply rules with table tests; all 100,000 captured real messages decode and fold correctly |
| 3 | In-memory store | Ordering, dedup, per-rocket locks; 1,000-seed random-order test, race-detector test; mutation checks fail as they should |
| 4 | HTTP API | Status codes, sorting, graceful shutdown; real program run: all 20 rockets match an independent reference |
| 5 | Debug endpoint (my request) | `/debug/store`; AI bug found while demoing it: "listening" logged before the port was bound |
| 6 | End-to-end test | Real binary through a fault-injecting proxy; AI's first version produced no real duplicates, fixed with "reply lost" failures |
| 7 | My redesign | SQLite (`rockets` and `pending` tables), loop in Go, `last_message_number`, `model`/`service`/`storage` layers; I chose `synchronous=NORMAL` after the AI measured FULL at about 100x slower |
| 8 | Code review | 10 findings fixed with tests (panic could freeze the only connection; slow drain behind a gap); AI used up my session limit with an extra review |
| 9 | Crash test | `kill -9` mid-run, restart, full replay: 0 differences from an uninterrupted run |
| 10 | Test cleanup (my request) | Redundant cases removed; coverage unchanged, 0 code blocks lost; `make test` about 100 s to 53 s |
| 11 | Minimal version (my request) | Hardening features removed and documented as a backlog; only `-addr` and `-db` flags left |
| 12 | Storage interface (my questions) | `Tx` split into one interface per table; `Storage` only runs transactions; `Tables` kept by my preference; `ChannelInfo` moved to `model` |
| 13 | Comments and lint (my request) | Stale and wordy comments fixed; staticcheck with all checks: 0 issues; `make lint` |
| 14 | Readable e2e output (my request) | Step-by-step output with summaries; one unexplained failure, test now prints diagnostics |
| 15 | README demo (my request) | Every command verified; I found `./rockets launch` did not work, replaced with `make publish`; out-of-order and duplicate scenario added to e2e; AI test traffic went into my database by mistake |
| 16 | This document | Restructured; corrected an inaccurate claim and three overstated "overrides" |
