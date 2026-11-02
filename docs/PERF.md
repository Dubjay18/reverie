# Recall latency and load targets, SQLite (M2 + M5)

## 1. M2: 10,000 embedded episodes (`TestRecallLatencyAt10K`)

Measured with `REVERIE_PERF=1 go test -run TestRecallLatencyAt10K -v ./internal/recall`
on the dev machine.

Setup: 10,000 closed episodes, each with a 1536-dimension embedding (random
vectors, the OpenAI size) and a text entry in the full-text index. A query runs
both lists, fuses them with RRF, scores, and assembles a 1,500-token pack.

| run | load | p50 | p95 | max |
| --- | --- | --- | --- | --- |
| before the blob-scan optimisation | not recorded | 204 ms | 292 ms | 320 ms |
| after, run 1 | load average about 4 | 192 ms | 239 ms | 263 ms |
| after, run 2 | load average about 4 | 188 ms | 235 ms | 248 ms |
| after, run 3 | busy machine | 243 ms | **382 ms** | 404 ms |

Target: p95 under 300 ms at 10K episodes.

**Not met unconditionally.** Two of three runs after the optimisation were under
300 ms, with the best at 235 ms. The third, on a busier machine, missed it at
382 ms. There is no controlled quiet-machine measurement, and three runs is too
few to call a distribution; treat 235-240 ms as a best case, not a guarantee.

## 2. M5 (T5.5): 10,000 embedded episodes + 5,000 facts and conflict scan (`TestLoad10KEpisodes5KFactsAndConflictScan`)

Measured with `REVERIE_PERF=1 go test -run TestLoad10KEpisodes5KFactsAndConflictScan -v ./internal/recall`.

Setup:
- 10,000 closed episodes with 1536-d embeddings and FTS5 index entries (seeded in 1m3.5s).
- 5,000 active facts across 250 entities and 5 relations (`uses`, `prefers`, `works_on`, `depends_on`, `lives_in`), indexed in FTS5 (seeded in 1m24.0s).
- **Recall benchmark:** 40 hybrid queries exercising FTS5, 1536-d cosine scan, 2-hop entity/fact expansion, RRF fusion, recency/importance scoring, and U-shaped pack assembly (budget 1,500 tokens).
- **Conflict-scan benchmark:** 1,250 `Facts().Live(entity, relation)` lookups scanning all 5,000 live facts across all 250 entities × 5 relations during a dream reconcile pass.

| metric | target | measured | outcome |
| --- | --- | --- | --- |
| Recall p50 (10K episodes + 5K facts) | — | **228.4 ms** | — |
| Recall p95 (10K episodes + 5K facts) | < 300 ms | **250.0 ms** (max 252.7 ms) | Pass |
| Conflict scan (5,000 facts / 1,250 `Live` lookups) | < 1 s per dream | **419.2 ms** (`335.4 µs` / lookup) | Pass (composite `(tenant, agent, project, user, entity, relation)` index) |

Almost all of the Recall time remains the brute-force cosine scan over the 10,000
embedding blobs; adding 5,000 facts and 2-hop entity expansion adds ~10–15 ms
because `facts` and `fact_sources` queries hit B-tree indexes.
