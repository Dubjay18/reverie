# Recall latency, SQLite, brute-force cosine

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

Almost all the time is the scan over every embedding row. If this becomes the
limit: scan fewer rows (restrict to the newest N, or to rows a full-text
prefilter matched), then consider an ANN index. Facts and the conflict scan are
measured in M5.
