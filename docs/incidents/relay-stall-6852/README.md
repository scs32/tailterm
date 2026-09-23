# Recovery message starved by relay request collision

Bug `wi_427b310fa9b8e886` revision 1; owner recurrence #6852, intake #6857,
bounded implementation #6860, operational qualification/release #6873.

At 20:41 UTC on September 14 the Board had no messages since 19:50:57 UTC.
QA's exact thread had ended at 19:48 waiting for a causal recovery record.
The handler saved that record and correctly addressed message #6851 to QA.
Nevertheless, the relay's queued-through cursor remained #6847 and QA never
consumed the recovery message. The lead ended its turn without verifying resume.

The local relay state recorded HTTP 409: a request ID had already been used with
different follow-through data. Source inspection established the defect:

1. The relay derived the check request ID from stable directive fields.
2. Each poll changed the observation timestamp in the request body.
3. The server correctly rejected the same ID with different bytes.
4. The relay skipped ordinary inbox delivery after any follow-through error.

This is distinct from the earlier prematurely resolved QA dependency. Recording
that earlier incident did not fix this transport failure. The exact first
occurrence of the collision remains unknown; the retained snapshot proves the
repeating failure and stalled cursor. No provider crash is asserted.

Root implemented commit `4f8aa43ed53c6c03e353073f08913e90127925a5`:
complete immutable request content determines check identity; ordinary eligible
inbox delivery can proceed despite a check failure; an external queue attempt,
including an ambiguous failure, prevents a second attempt in the same scan.
Exact run/lifecycle checks and server receipt validation remain in force.

Both original defects were restored separately and caused their new regression
tests to fail. The corrected relay tests, race selection, vet and diff checks
passed. Lead independently reviewed the source and qualified two identical Mini
builds plus five packaged scenarios. Handler #6885 saved package acceptance.
The source/result record and negative-control outputs are retained alongside this
file. Independent checks do not establish installed prevention.

Immediate recovery used one supported queue to QA's existing exact thread, with
no runtime restart or state deletion. Handler #6878 verified native resume to
epoch 2 and real independent test work. QA then reported 207 tests and browser
checks passing, without blockers, and submitted its native result #6886. This
establishes recovery beyond mere queue acceptance.

The qualified Mini-only install and existing-relay reload completed under #6873,
with receipt #6892. Installed CLI SHA-256 is
`93efb44ef7f09c07f7c2f9dfa81b2be2a01b7ca76f8027f325f7327230d2860a`.
The old binary remains for rollback; the existing relay was reloaded once,
without changing its configuration or identities. All five installed isolated
scenarios passed, and lead #6894 submitted installed acceptance. Handler #6893
confirmed unchanged QA/Pause identities and current actions. Handler #6896 saved
final installed acceptance; #6898 closed this narrow Bug at revision 2 with
receipt `wir_3519aab52fa55221` (request `relay-bug-done-6894`).
The wider atomic recovery-to-next-action control remains pending; this fix covers
the diagnosed collision and inbox starvation only. Pause release compatibility
order #6884 prevents its older CLI package from undoing this fix.
