# PAI ULTRA benchmark, 2026-11-17

One fixture task (`bench/ultra/main.go`): `Reverse` in package `strutil` breaks non-ASCII strings. Acceptance, run by the benchmark itself after each run: `go test ./...` passes and the test file is unchanged. Every mode starts from a fresh copy of the same commit, on one Windows 11 machine, with the same permissions: Claude Code `--permission-mode acceptEdits --allowedTools "Bash(go test:*)" "Bash(git diff:*)"`, Codex `--sandbox workspace-write`. Claude modes use Haiku 5.5; Codex modes use Codex's default model; Classic uses the PAI server session's own model.

**One run per mode (two for the Claude pair). This shows the harness works end to end; it is not evidence of a quality or cost difference.** Rerun with `-n 5` or more before drawing conclusions.

| Mode | Runs | Checks passed | Time (s) | Tokens, reported (in / out / cache read) |
|---|---|---|---|---|
| bare Claude Code | 2 | 0 of 2 | 34, 39 | 598,523 (29,515 / 4,442 / 564,566); 644,406 (30,438 / 5,712 / 608,256) |
| Claude Code under ULTRA | 2 | 2 of 2 | 32, 31 | 371,790 (22,272 / 2,477 / 347,041); 337,195 (22,903 / 2,468 / 311,824) |
| bare Codex | 1 | 1 of 1 | 53 | 100,900 (5,445 / 607 / 94,848) |
| Codex under ULTRA | 1 | 1 of 1 | 41 | 66,110 (4,801 / 509 / 60,800) |
| PAI Classic | 1 | 1 of 1 | 89 | 35,735 (7,561 / 654 / 27,520); the server session's model, chatgpt/gpt-6-astra, not Haiku |
| Crash recovery, no model | 1 | correct | | |

- Bare Claude Code ended both runs with `Reverse` still reversing bytes (`Reverse("héllo") = "oll\xa9\xc3h"`). Why is not known: the benchmark does not keep the agent's transcript.
- ULTRA used one attempt in every run; it added no background model calls. Its injected context was 215 tokens (estimate) in every run.
- Classic's tokens are the session's reported usage; Classic's own memory work around the session is not counted.
- Crash recovery: a run that dies after its executor wrote an effect is recovered to blocked on the unknown operation, is not replayed, and after a person reconciles it, resumes and is accepted with the effect applied exactly once more.

Runs that found problems, fixed before release:

1. An earlier run allowed Claude Code to edit but not to run `go test`. Under ULTRA it fixed the code, reported "blocked", and ULTRA stopped without checking. ULTRA now checks every claim (commit ca17c61); both modes now get the same tool allowlist.
2. The second Claude run's recovery check read "not correct": the benchmark's simulated dead owner had a 50 ms lease, and after leases began refusing writes once expired (a857b27), its own setup writes were refused on a slow disk. The scenario now uses a 2 s lease and fails loudly on a refused setup step; rerun, it is correct.

Raw data: `results.json` (all five modes), `results-claude-run2.json` (second Claude pair; its recovery field is the stale result above).
