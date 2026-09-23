"""Run the ShouldWake test kit against Jev and score it against the answer key.

Key: $TYPESAFE_API_KEY, else ~/.config/typesafe/key.
  python run_jev.py --dry-run         build every request, no network
  python run_jev.py --models          list models the account can use
  python run_jev.py [--runs 3] [--model jev-latest] [--variants B,G]
"""
import argparse, json, os, pathlib, re, statistics, sys, time
from typesafe_sdk import Choice, Noul, Score, TypeSafeClient

HERE = pathlib.Path(__file__).parent
RULES = ["r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8", "r9", "order"]
# variant -> (acceptable `violated` choices, noul keys that should come back < 0.5)
TRUTH = {
    "ref": ({"none"}, set()), "A": ({"none"}, set()),
    "B": ({"R1"}, {"r1"}), "C": ({"R3"}, {"r3"}), "D": ({"R4"}, {"r4"}),
    "E": ({"R6"}, {"r6"}), "F": ({"R7"}, {"r7"}), "G": ({"R9"}, {"r9"}),
    "H": ({"R2", "R8"}, {"r2", "order"}),
}


def load_key():
    if os.environ.get("TYPESAFE_API_KEY", "").strip():
        return None  # SDK reads the env var itself
    f = pathlib.Path.home() / ".config/typesafe/key"
    if f.exists():
        return f.read_text().strip()
    sys.exit("No key: set TYPESAFE_API_KEY or write it to ~/.config/typesafe/key")


def questions(spec):
    out = {}
    for name, q in spec.items():
        if q["type"] == "noul":
            out[name] = Noul(instructions=q["q"])
        elif q["type"] == "choice":
            out[name] = Choice(instructions=q["q"], criteria={o: None for o in q["options"]})
        else:
            out[name] = Score(instructions=q["q"], criteria=q["rubric"])
    return out


def source(v):
    text = (HERE / ("reference.go" if v == "ref" else f"v_{v}.go")).read_text()
    # Neutral function name so the variant letter leaks nothing.
    return re.sub(r"func ShouldWake\w*\(", "func ShouldWake(", text)


def score(v, r):
    want_choice, want_low = TRUTH[v]
    nouls = {k: r["nouls"][k] for k in RULES}
    low = {k for k, p in nouls.items() if p < 0.5}
    return {
        "detected": r["violated"] in want_choice,
        "localized": (not want_low) or (min(nouls, key=nouls.get) in want_low and bool(low & want_low)),
        "false_alarms": sorted(low - want_low),
        "choice_conf": r["violated_conf"],
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--runs", type=int, default=3)
    ap.add_argument("--model", default=None)
    ap.add_argument("--variants", default=",".join(TRUTH))
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--models", action="store_true")
    a = ap.parse_args()

    crit = json.loads((HERE / "criteria.json").read_text())
    qs = questions(crit["questions"])
    variants = a.variants.split(",")
    if a.dry_run:
        for v in variants:
            state = {"spec": crit["state"]["spec"], "code": source(v)}
            body = {"state": state, "questions": {k: q.model_dump() for k, q in qs.items()}}
            print(f"{v}: {len(json.dumps(body))} bytes, {len(qs)} questions")
        return

    with TypeSafeClient(api_key=load_key()) as client:
        if a.models:
            for m in client.models.list().models:
                print(m.name, m.release_date, "-", m.description)
            return
        rows, raw = [], []
        for v in variants:
            state = {"spec": crit["state"]["spec"], "code": source(v)}
            for i in range(a.runs):
                t0 = time.perf_counter()
                resp = client.system_one(state, qs, model=a.model)
                ms = (time.perf_counter() - t0) * 1000
                r = {
                    "variant": v, "run": i, "ms": round(ms), "model": resp.model,
                    "tokens": resp.usage.input_tokens,
                    "nouls": {k: resp.nouls[k].noul for k in RULES},
                    "violated": resp.choices["violated"].choice,
                    "violated_conf": resp.choices["violated"].confidence,
                    "violated_probs": resp.choices["violated"].probabilities,
                    "overall": resp.scores["overall"].score,
                }
                r["grade"] = score(v, r)
                raw.append(r)
                g = r["grade"]
                print(f"{v:>3} run{i} {ms:5.0f}ms  violated={r['violated']:<4} ({r['violated_conf']:.2f})  "
                      f"overall={r['overall']:.2f}  detected={'Y' if g['detected'] else 'n'}  "
                      f"localized={'Y' if g['localized'] else 'n'}  false_alarms={g['false_alarms']}")
        out = HERE / f"results-{time.strftime('%Y%m%d-%H%M%S')}.json"
        out.write_text(json.dumps(raw, indent=2))

        bugs = [r for r in raw if r["variant"] not in ("ref", "A")]
        clean = [r for r in raw if r["variant"] in ("ref", "A")]
        hits = [r for r in bugs if r["grade"]["detected"]]
        misses = [r for r in bugs if not r["grade"]["detected"]]
        print("\n== summary")
        print(f"detection   {len(hits)}/{len(bugs)} buggy runs")
        print(f"localized   {sum(r['grade']['localized'] for r in bugs)}/{len(bugs)}")
        print(f"clean runs flagged  {sum(r['violated'] != 'none' for r in clean)}/{len(clean)}; "
              f"false-alarm nouls on all runs: {sum(len(r['grade']['false_alarms']) for r in raw)}")
        if hits and misses:
            print(f"confidence  hits {statistics.mean(r['violated_conf'] for r in hits):.2f} vs "
                  f"misses {statistics.mean(r['violated_conf'] for r in misses):.2f}")
        stable = {v: len({r['violated'] for r in raw if r['variant'] == v}) == 1 for v in variants}
        print(f"stable across runs  {sum(stable.values())}/{len(stable)}")
        print(f"latency     median {statistics.median(r['ms'] for r in raw):.0f}ms; "
              f"tokens {sum(r['tokens'] or 0 for r in raw)} (~${sum(r['tokens'] or 0 for r in raw) * 0.042e-6:.5f})")
        print(f"raw -> {out}")


if __name__ == "__main__":
    main()
