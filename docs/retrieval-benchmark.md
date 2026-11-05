# Retrieval benchmark for v0.3.0

Measured on 2026-11-05 on a Jetson AGX Orin with 64 GB shared memory,
Ubuntu 24.04, L4T 39.2.1, CUDA driver 595.78 / 13.2, and private Ollama 0.40.14.
The model was embeddinggemma (BF16, 307.58M parameters, 768-dimensional vectors),
digest `85462619ee721b466c5927d109d4cb765861907d5417b9109caebc4e614679f1`.
The existing EyeQ application and PAI services remained running.

## Labeled synthetic retrieval

The public [fixture](../bench/retrieval/cases.json) contains 40 active facts,
three retired facts, 40 labeled positive questions (20 paraphrases/synonyms,
10 exact identifiers, 10 constraints), and four unrelated probes. Each positive
question has one expected fact. All modes use the same current store and
scoring implementation with neutral initial utility, empty session, and k=6.
The lexical baseline uses the original hash embedder plus BM25 rank fusion;
it is not a benchmark of the entire old application binary. Cases were not
used to tune model weights or the similarity threshold.

| Mode | Expected fact first | In top 3 | In top 6 | MRR | Retired hits |
| --- | ---: | ---: | ---: | ---: | ---: |
| Hash + BM25 | 55.0% | 70.0% | 75.0% | 0.627 | 0 |
| Semantic model | **97.5%** | **100%** | **100%** | **0.983** | 0 |
| Model + BM25 fusion | 70.0% | 97.5% | 97.5% | 0.833 | 0 |

Semantic retrieval put every identifier and constraint question's expected
fact first. For synonym questions it put 19/20 first and all 20 in the top
three. The remaining rate-limiting question's expected fact ranked third.

Model-resident, first-query latency includes embedding inference, SQLite
loading and ranking. Cached latency is five repeat passes (220 requests per
mode) after the first 44 queries; it is not cold model startup.

| Mode | First-query p50 / p95 | Cached-query p50 / p95 |
| --- | ---: | ---: |
| Hash + BM25 | 5.08 / 6.79 ms | 5.26 / 7.10 ms |
| Semantic model | 15.15 / 18.78 ms | 1.43 / 2.26 ms |
| Model + BM25 fusion | 17.44 / 21.01 ms | 4.99 / 7.17 ms |

The release fixture gate requires semantic top-1 at least 95%, top-3 recall
100%, and zero retired hits. It passed. These are small, author-labeled
retrieval cases, not a held-out production question set or general answer
accuracy certification. The raw [synthetic results](../bench/retrieval/jetson-results.json)
include every case rank, including failures.

## Read-only live PAI API

The live database contained 38 valid and 47 retired memories. Querying each
valid fact's own text returned its original id first in 38/38 cases. This
checks migration and indexing integrity; copying a fact into its query is not
an independent semantic-quality evaluation. No retired facts or lexical ranks
were returned. Production memories and sessions were not changed by these
searches.

| Workload | Requests | p50 | p95 |
| --- | ---: | ---: | ---: |
| First pass through 38 facts | 38 | 27.18 ms | 56.81 ms |
| Cached serial searches | 190 | 4.44 ms | 5.01 ms |
| Cached searches, four concurrent clients | 200 | 5.67 ms | 8.91 ms |

The four-client cached run completed about 635 requests/second on loopback.
That rate applies to this tiny, fully cached corpus, not embedding throughput
or completed agent turns. The [live results](../bench/retrieval/jetson-live-results.json)
publish aggregates only; production memory texts and ids are not included.

The optional managed-runtime lifecycle was also exercised: its first
startup/warmup with a fresh CUDA cache took 72.19 seconds. The model produced
vectors, and stopping the runtime closed its private listener. Startup
readiness checks and persistent model residency account for this cost.

A full fresh-install check also downloaded the official ARM64 archive,
verified its pinned SHA-256, extracted it, downloaded embeddinggemma, and
produced a readiness vector in an isolated PAI directory. It completed in
138.62 seconds including downloads and initial warmup; starting and warming
that installed runtime again took 3.75 seconds. Runtime archive links were
materialized as regular files, satisfying PAI's installation path checks.

## Precision, tokens and limits

- The fixed 0.12 similarity cutoff still returned six candidates for each
  unrelated probe (maximum similarities about 0.24 to 0.31). A retrieved
  candidate does not prove the question has an answer. Model-specific
  calibration and answer abstention need a larger labeled evaluation before
  changing that cutoff; no precision claim is made for unrelated questions.
- On positive cases the lexical baseline returned 3.075 items on average;
  semantic retrieval returned six. Better recall can increase injected context.
  The embedding cache saves inference work; it does not itself reduce chat
  input tokens. No end-to-end token savings or model-answer correctness was
  measured here.
- The learned skip/recall policy, project-scope filtering, summarization,
  extraction, provider latency and task completion accuracy are outside this
  retrieval benchmark. See the [efficiency audit](retrieval-efficiency-audit.md)
  for their remaining accuracy risks and prioritized recommendations.
- Tail latencies are observations from one machine and small runs. There is
  no claim about large corpora, sustained load, shared-GPU contention, or every
  supported OS. Installer archive formats and path checks have automated
  tests; actual GPU lifecycle testing was on Linux ARM64.

## Reproduce

Build the command for the target and keep every output under PAI's benchmark
folder. The work directory must be new; the command refuses an existing one,
protecting a live store from accidental reuse.

```sh
go build -o working_dir/retrieval-bench ./bench/retrieval
working_dir/retrieval-bench \
  -data bench/retrieval/cases.json \
  -work /path/to/pai/benchmarks/retrieval-work \
  -out /path/to/pai/benchmarks/retrieval-results.json \
  -url http://127.0.0.1:11434/v1 -model embeddinggemma -repeats 5 -check
```

Model-free installs remain supported. New terminal setup asks before
downloading a private runtime; unattended setup and existing missing memory
configuration keep model-free retrieval. Users who select embeddings get
semantic ranking by default and can explicitly configure hybrid fusion.
