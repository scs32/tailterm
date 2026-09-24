# Jev test kit: ShouldWake (complexity 7/10)

`reference.go` decides whether the relay should wake an agent for a board message.
It has 9 interacting rules, strict precedence, human/agent asymmetry and a
time-window rate limit. `v_A`..`v_H` are variants. Ground truth is machine-checked:
`go test -v` shows which rule each variant breaks.

| Variant | Planted defect | Rule | Expected difficulty for Jev |
|---|---|---|---|
| ref | none | — | control (want "none", overall 5) |
| A | none; comments stripped, var renamed | — | false-positive test |
| B | compares sender ID to agent **Name** | R1 | easy–medium |
| C | `>` instead of `>=` | R3 | medium |
| D | board-wide posts wake every agent | R4 | medium |
| E | pause also defers human messages | R6 | easy |
| F | swarm broadcasts resume retired agents | R7 | medium |
| G | RetryAt uses newest wake, not oldest | R9 | hard (time arithmetic, known weakness) |
| H | busy check moved above closed check | R2/order | hard (precedence reasoning, known weakness) |

B–H keep the correct comment above the broken code. That tests whether Jev
reads the code or just believes the comment.

## Scoring (per variant)
- **Detection**: `violated` top choice == planted rule (H accepts R2 or R8).
- **Localization**: the noul for the planted rule is lowest of the 10, and < 0.5.
- **False alarms**: count of nouls < 0.5 on rules that are actually correct.
- **Calibration**: mean confidence on hits vs misses. You want it to be less sure when it's wrong.
- **Control**: ref and A should get `violated=none`, all nouls > 0.5, overall >= 4.

Suggested pass bar for "useful as a pre-review screen": >= 5/7 defects detected,
<= 1 false alarm across ref+A, and misses carrying visibly lower confidence.
Run each variant 3x to check stability.

Comparison: run the same spec and variants past Opus 5.5 / Sol 6 as a normal review prompt
and record cost, latency and detection. That's the real decision data.

## Running against Jev

```
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
# key: $TYPESAFE_API_KEY or ~/.config/typesafe/key (chmod 600)
.venv/bin/python run_jev.py --models        # check the key works
.venv/bin/python run_jev.py [--model jev-preview] [--runs 3]
.venv/bin/python run_board.py [--dry-run]   # two-layer board guardrail on board_samples.json
.venv/bin/python check_redact.py            # redact() matches the hub's Go scanner (shared corpus)
```

Only use the official `typesafe-sdk` package; `typesafe-ai` and `jev` on PyPI are third-party.

## First results (2026-09-23)

| | jev-latest | jev-preview |
|---|---|---|
| Buggy runs caught (of 21) | 12 | 11 |
| Correct runs flagged (of 6) | 0 | 1 |
| Median latency | 111 ms | 118 ms |
| Board samples correct (of 13) | 13 | 13 |

Always caught: C (`>` vs `>=`), G (RetryAt arithmetic), H (precedence).
Never caught: B (ID vs Name), D (board-wide scope), F (swarm resumes retired).
`choice` confidence was only ~0.3 even on correct answers, and `overall` scored the reference ~2.3/4.
Conclusion: not a code reviewer. On the board samples, bad posts scored 0.02–0.09 on
the failing question, good posts scored 0.78–0.97, and the injection post was flagged at 0.99. The samples are
hand-written and optimistic; validate on real board traffic before enforcing.

## Real board traffic (2026-09-23, 500 random agent posts of 7,832)

`run_board_real.py` exports the hub board read-only (GET only), redacts
secret-looking strings, and asks the type-agnostic questions in `QUESTIONS`.
500 posts took about 9–13 s wall at 8-way concurrency and ~$0.02.

- Posts use no type line, so the fixed-structure layer as designed would deny ~100%.
- v1 questions denied 95%, mostly wrongly. The `manipulation` noul flagged 272 ordinary lead/reviewer
  instructions; the agents *are* reviewers, so "addressed to a reviewer" describes normal traffic.
- v2 (current) denies 23%. `manipulation` now flags 0/500 real posts and still flags the planted injection at 0.96.
- Hand audit of v2 denials:
  - `cut_off` (7) is accurate. These are multi-part artifact chunks ("part 3/5").
  - `evidence` below 0.2 (13 posts): about 7 are genuine "done" claims with no evidence, and about 6 are wrong
    (plans, nudges, in-progress status). At 0.2–0.5 the posts mostly do cite evidence. Too noisy to enforce.
  - `clear_ask` (89) all sit in the 0.3–0.5 unsure band and are informational receipts. Don't gate on it.
- `readable` rated only 37/500 as readable in one pass. Spot checks agree: posts are dense runs of
  IDs, hashes and shorthand.
