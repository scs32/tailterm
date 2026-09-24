"""Check that redact() matches the Go scanner on the shared corpus.

  .venv/bin/python check_redact.py
"""
import json, pathlib, sys
from run_board_real import redact

corpus = pathlib.Path(__file__).parent / "../../hub/internal/jev/testdata/redact_corpus.json"
failures = [(c["in"], redact(c["in"]), c["out"]) for c in json.loads(corpus.read_text()) if redact(c["in"]) != c["out"]]
for given, got, want in failures:
    print(f"redact({given!r})\n  got  {got!r}\n  want {want!r}")
print(f"{len(failures)} mismatches")
sys.exit(1 if failures else 0)
