# Retrieval, accuracy and efficiency audit

Audited 2026-11-05. Stable `main` was fast-forwarded to `ab157aa`; implementation
is on `feat/semantic-retrieval`, based on `origin/beta` at `d28e48b`, following
AGENTS.md. This is a source-level end-to-end audit, with automated regression
checks. At the initial source audit, no production corpus or hardware
measurements were available. Later Jetson retrieval measurements are recorded
in the [benchmark report](retrieval-benchmark.md). Recommendations below remain
expected effects unless that report explicitly measures them.

## Implemented

- Explicit embedding configurations use model vectors and cosine relevance.
  Terminal setup asks whether to download a private Ollama / embeddinggemma
  runtime; declining and existing configs without memory settings retain
  model-free retrieval. Endpoint, model and key are configurable.
- Existing vectors migrate on endpoint/model-name change, retaining item ids,
  texts and history. Startup closes its database on failed re-embedding.
- A bounded, model-specific cache reuses identical embeddings and coalesces
  duplicates within a batch, including the Related -> Remember path. Item
  vectors remain persistent. Cache eviction affects speed only, not results.
- Embedding responses reject duplicate/missing/out-of-range indices, empty or
  zero vectors and inconsistent dimensions. Search refuses dimension drift
  instead of treating it as zero similarity. Rank ties are deterministic.
- Deduplication now requires matching text in addition to vector similarity.
  Different numbers, versions and negations remain separate for explicit
  consolidation. Previously a cosine above 0.93 could discard a distinct fact.
- Automatic recall failures create a durable model-visible warning; explicit
  recall returns the error. There is no silent hash fallback.

The semantic regression fixture verifies HTTP embeddings, reindexing from hash
vectors, a synonym query with no shared words, invalidation, and distinct facts
with identical vectors. It verifies plumbing, not the real model's quality.

Validation completed on Windows with Go 1.27.3: `go vet ./...`, `go test ./...`,
and builds of both `cmd/pai` and `cmd/pai-server` passed. `git diff --check`
passed. Additional regression coverage checks cache reuse/eviction/cancellation,
configuration validation and a durable warning when automatic recall fails.

## Priority findings and recommendations

| Priority | Finding and evidence | Recommendation | Expected benefit and accuracy gate |
| --- | --- | --- | --- |
| P0 | `pkg/memory/recall.go`: Search loads every valid scope; session only boosts ranking. Pinned recall also ignores scope. | Carry explicit project/session scope through Turn, Search and tool calls. Admit global plus authorized project/current-session memories before ranking, including pinned items. | Prevent irrelevant facts from another project entering answers; reduce candidate work and tokens. Test same names and contradictory facts across projects. |
| P0 | `pkg/memory/recall.go`: Settle infers use from distinctive word overlap; `pkg/memory/bandit.go` allows choosing zero items. | Replace word overlap as a quality target with evaluated evidence use, explicit citations and user feedback. Keep a conservative recall floor for questions depending on history until the policy passes a held-out evaluation. Do not add a model judge on every turn. | Stop rewarding repeated words and penalizing correct paraphrases. Gate any reduction in recall on critical-fact recall and answer correctness, not average reward. |
| P0 | `pkg/harness/memory.go`: apply marks candidates done before Merge succeeds; keep logs and skips Related failures; afterTurn advances the extraction cursor regardless of individual failures. | Make candidate processing return structured outcomes. Mark done only after persistence or an intentional validated skip; advance the extraction cursor only after a complete durable batch. Add an idempotent per-session extraction job/lock. | Avoid lasting facts being skipped after transient service failures or overlapping extraction jobs. Inject database/embedding failures and verify retry without cursor regression or duplicates. |
| P0 | `pkg/harness/memory.go`: transcript keeps only the newest bytes when over budget; call arguments/results are clipped at 600/1500 bytes before summarization. | Chunk extraction across the entire range at record boundaries; preserve exact decisions, ids, constraints and source references. Let compaction fetch omitted source excerpts on demand. | Prevent silent omission before the cursor moves past earlier facts. Use long-log tests with an important fact at the beginning and middle, plus large tool output. |
| P1 | `pkg/memory/recall.go`: fixed cosine threshold 0.12, top 40 candidates, utility/recency/session boosts; thresholds originated with hash vectors. | Calibrate per embedding model on held-out questions. Compare pure semantic with opt-in hybrid for paths, exact ids and error codes; consider reranking only ambiguous candidate sets. | Improve precision and reduce irrelevant injected tokens without losing critical recall. Do not assume embedding-only beats hybrid on exact strings. |
| P1 | `pkg/harness/memory.go`: new memory records accumulate in context; k limits item count rather than tokens; pinned items add up to ten more. | Enforce a measured memory token budget with compact facts and source ids, protect pinned constraints, and let explicit recall expand evidence. Avoid injecting already-present facts again; keep append-only records and stable prefixes. | Reduce repeated input and compaction calls. Test that long pinned constraints survive and previously recalled evidence remains accessible. |
| P1 | `pkg/harness/harness.go`: calls execute serially; `pkg/harness/remote.go` already multiplexes requests. | Add bounded concurrency only for explicitly independent reads and separate workers. Serialize writes, shared interpreter state, and calls whose dependencies are unknown; append results in deterministic call order. | Lower tool latency without changing effects. Test read-after-write, persistent shell variables, cancellation, and partial failures. |
| P1 | `pkg/memory/recall.go`: every search loads full valid items and vectors, sorts candidates; `pkg/memory/consolidate.go`: Clusters compares every pair. SQLite uses one connection. | Profile realistic memory sizes first. Cache an immutable active-vector snapshot with write invalidation, select top-k with a heap, and prepartition clustering by scope/topic. Consider an ANN index only when exact scans become a measured bottleneck. | Lower CPU, allocations and database contention. Compare optimized output against exact search; measure ANN recall before considering adoption. |
| P1 | `pkg/harness/memory.go`: complete calls the session model for extraction, consolidation and compaction with the same Params; these calls are not appended as normal Step records. | Meter maintenance calls separately (tokens, latency, errors, purpose). Add an independently configurable maintenance model/params only after measuring correctness. Keep high-quality compaction for complex tasks; route simple extraction only when the smaller model passes the same tests. | Reveal hidden token spend and enable safe cost reductions. Check exact fact retention and JSON operation correctness before routing. |
| P2 | `pkg/server/metrics.go`: each metrics request scans all session files; Performance polls and the retrieval panel requests all-time metrics. `pkg/statefile/file.go` scans whole files on Read/Open. | Build a rebuildable incremental projection indexed by file offset/record number, using append events and torn-record recovery. Cache metrics briefly and paginate history; keep session files authoritative. | Reduce dashboard I/O and competition with active sessions. Test append, restart, torn tails and index rebuilding against full scans. |
| P2 | `pkg/harness/tools.go`: large shared system prompt and worker instructions enter sessions; provider envelopes already support stable prompt caching. | Measure per-role prompt/tool usage, remove truly irrelevant sections, and keep stable system/tool prefixes. Preserve correctness rules and avoid changing prompts mid-session, since request replay checks reject incompatible histories. | Lower uncached input; preserve cache hit rates. Compare task success and replay compatibility before shipping prompt changes. |
| P2 | `pkg/harness/route.go`: child reuse is TF-IDF word/path based; orchestrator handoffs are bounded tails (`orchestrator.go`). | Treat child routing separately from long-term retrieval. If reuse errors are observed, combine semantic task vectors with exact file constraints; retain structured evidence and artifacts instead of reply tails alone. | Improve useful reuse without carrying unrelated context or losing decisions. Test unrelated tasks sharing filenames and paraphrased tasks with no shared words. |

## Accuracy-first rollout and measurement

1. Build a versioned evaluation corpus from representative, consented sessions.
   Include synonym-only questions, exact paths/ids, numeric changes, negation,
   superseded facts, project boundaries, multilingual queries, no-answer queries
   and long histories. Label supporting memory ids and the exact facts expected.
2. Compare old hash/hybrid, semantic, and semantic/hybrid using the same corpus.
   Track Recall@k and critical-fact misses first, then ranking quality and
   unsupported-answer rate. Evaluate final answers too: retrieval alone does not
   establish accuracy. Split tuning and held-out cases.
3. Record p50/p95 end-to-end latency split into recall/embedding, context build,
   provider first token/full completion, tools, and maintenance. Track uncached
   input, cache reads/writes, output, injected-memory tokens, maintenance tokens,
   embedding requests and cache hits separately. Run warm/cold cache cases with
   fixed model versions, corpus sizes and hardware.
4. Ship low-risk caching and I/O improvements first. Require no new critical
   misses and no loss of evaluated answer correctness before reducing recall,
   shortening context, changing models or introducing approximate search.
5. Measure actual service operation: stop the embedding endpoint, change its
   dimensions, restart during migration, then recover it. Confirm a visible
   failure, retained history and successful retry. Changing weights under an
   unchanged name with unchanged dimensions still requires an operator-managed
   version change; PAI cannot infer that drift.

## Deployment and limits

See [self-hosting](self-hosting.md#memory) for setup and migration. Terminal
setup downloads and manages a private runtime only after explicit opt-in.
Existing configs without memory settings do not download a runtime or change
their retrieval behavior during an update. See the benchmark report for measured
Jetson results; initial audit observations above preceded deployment. Restart
after memory config changes.

The existing bandit and word-based usage feedback remain in place. Semantic
retrieval does not make that feedback semantic. Scope enforcement, extraction
durability, token budgets and runtime benchmarks above are recommendations,
not claims that this patch implements them. Child routing remains lexical.

External API behavior checked against the primary documentation:
[Ollama embeddings](https://docs.ollama.com/capabilities/embeddings) and
[OpenAI-compatible endpoints](https://docs.ollama.com/api/openai-compatibility).
EmbeddingGemma here is a local operational default, not an assertion that it is
the most accurate model for this user's data.
