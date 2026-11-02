# Reverie Evaluation Results (M5: T5.1–T5.4)

## Method

Every scenario and benchmark subset item is executed through the same deterministic runner (`evals.RunAll`) across four configurations:

1. **`no_memory`**: Returns an empty context pack (`0` tokens/query, `0` dream tokens).
2. **`full_context`**: Appends all in-scope raw session events chronologically up to the query token budget (recent filler crowds out older turns; contradicted or expired statements stay in the raw log).
3. **`raw_history_search`**: Agent search baseline over raw events in scope (keyword ranking over raw event lines within the query budget, without dream distillation, ontology supersession, TTL expiry, or 2-hop entity expansion).
4. **`reverie`**: Full Reverie engine (`Record`, `Remember`, `Pin`, offline `Dream` with `Replay -> Distill -> ReconcileReflect -> Forget -> Validate`, and `Recall` with FTS5 + 2-hop entity expansion + ontology TTL filtering + U-shaped assembly).

## 1. Combined Summary (Core Scenarios + LongMemEval & LoCoMo Subsets)

| Configuration | Queries | Passed | Accuracy | Avg Tokens / Query | Total Dream Cost (Tokens) |
| --- | ---: | ---: | ---: | ---: | ---: |
| `no_memory` | 13 | 0 | 0.0% | 0.0 | 0 |
| `full_context` | 13 | 6 | 46.2% | 116.2 | 0 |
| `raw_history_search` | 13 | 7 | 53.8% | 58.0 | 0 |
| `reverie` | 13 | 12 | 92.3% | 121.7 | 1280 |

## 2. Core Scenarios (`amnesia`, `contradiction`, `staleness`, `isolation`, `continuity`)

| Configuration | Queries | Passed | Accuracy | Avg Tokens / Query | Total Dream Cost (Tokens) |
| --- | ---: | ---: | ---: | ---: | ---: |
| `no_memory` | 5 | 0 | 0.0% | 0.0 | 0 |
| `full_context` | 5 | 2 | 40.0% | 189.6 | 0 |
| `raw_history_search` | 5 | 3 | 60.0% | 59.8 | 0 |
| `reverie` | 5 | 5 | 100.0% | 147.2 | 290 |

## 3. LongMemEval + LoCoMo Subsets

| Configuration | Queries | Passed | Accuracy | Avg Tokens / Query | Total Dream Cost (Tokens) |
| --- | ---: | ---: | ---: | ---: | ---: |
| `no_memory` | 8 | 0 | 0.0% | 0.0 | 0 |
| `full_context` | 8 | 4 | 50.0% | 70.2 | 0 |
| `raw_history_search` | 8 | 4 | 50.0% | 56.9 | 0 |
| `reverie` | 8 | 7 | 87.5% | 105.8 | 990 |

## 4. LongMemEval & LoCoMo Category Breakdown (`reverie`)

| Benchmark / Category | Queries | Passed | Accuracy |
| --- | ---: | ---: | ---: |
| `LongMemEval/temporal-reasoning` | 2 | 1 | 50.0% |
| `LoCoMo/multi-hop-dialog` | 1 | 1 | 100.0% |
| `LoCoMo/single-hop-factual` | 1 | 1 | 100.0% |
| `LoCoMo/temporal-dialog` | 1 | 1 | 100.0% |
| `LongMemEval/knowledge-update` | 1 | 1 | 100.0% |
| `LongMemEval/multi-session` | 1 | 1 | 100.0% |
| `LongMemEval/single-session-user` | 1 | 1 | 100.0% |

### Worst Category Identified: `LongMemEval/temporal-reasoning` (50.0% accuracy, 1/2)

`DistillPhase` normalizes relative anchors (`yesterday`, `today`, `last week`) into absolute ISO dates anchored to `Event.At`, which resolves single-anchor temporal queries (`lme-temporal-reasoning-1`, `locomo-temporal-1`). However, cross-episode interval arithmetic (`lme-temporal-reasoning-2`: computing the exact 14-day delta between two separate episode timestamps without an explicit reader-side arithmetic step) is not pre-computed into a fact, making `LongMemEval/temporal-reasoning` the lowest-scoring category (50.0%).
