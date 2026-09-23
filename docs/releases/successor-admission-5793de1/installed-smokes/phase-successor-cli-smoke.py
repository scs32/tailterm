#!/usr/bin/env python3
"""Synthetic packaged-CLI smoke for operationalRecords v2 phase successors."""

import argparse
import hashlib
import http.server
import json
import os
import pathlib
import subprocess
import tempfile
import threading


ROOT = pathlib.Path(__file__).resolve().parent
CLI = ROOT / "tt-darwin-arm64"
CLI_SHA256 = "2c23f9975462afb0bb587d7e213ae6272cae6062e6cf9a09bfba451133b1b378"
assert hashlib.sha256(CLI.read_bytes()).hexdigest() == CLI_SHA256

TASK = "tsk_0000000000000101"
AGENT = "agt_0000000000000102"
RUN = "run_0000000000000103"
OBLIGATION = "pso_0000000000000104"
TOKEN = "synthetic-phase-successor-smoke"
requests = []
mode = "v2"


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def send_json(self, value, status=200):
        raw = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def record(self, body=None):
        value = {"method": self.command, "path": self.path}
        if body is not None:
            value["body"] = body
        requests.append(value)

    def do_GET(self):
        assert self.headers.get("Authorization") == "Bearer " + TOKEN
        self.record()
        if self.path == "/v1/capabilities":
            versions = [1, 2] if mode == "v2" else [1]
            self.send_json({"operationalRecords": {"supported": True, "versions": versions}})
            return
        if self.path == f"/v1/tasks/{TASK}/phase-successor-obligations/{OBLIGATION}":
            assert mode == "v2"
            self.send_json({
                "id": OBLIGATION,
                "taskId": TASK,
                "version": 1,
                "state": "pending",
                "itemTaskId": TASK,
                "itemId": "wi_0000000000000105",
                "itemRevision": 3,
                "scopeRevision": 2,
                "phaseKey": "candidate",
                "leadAgentId": AGENT,
                "leadRunId": RUN,
            })
            return
        self.send_json({"error": "not found"}, 404)

    def do_POST(self):
        assert self.headers.get("Authorization") == "Bearer " + TOKEN
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length))
        self.record(body)
        assert mode == "v2"
        assert self.path == f"/v1/tasks/{TASK}/operational-records"
        assert body["agentId"] == AGENT and body["runId"] == RUN
        assert body["requestId"] == "phase-smoke-propose"
        assert body["data"]["kind"] == "acceptance"
        assert body["data"]["acceptance"]["phase"] == {
            "version": 1,
            "key": "candidate",
            "terminal": False,
        }
        self.send_json({
            "record": {
                "id": "opr_0000000000000106",
                "taskId": TASK,
                "version": 1,
                "state": "proposed",
                "actorAgentId": AGENT,
                "actorRunId": RUN,
                "data": body["data"],
            },
            "receipt": {
                "id": "orr_0000000000000107",
                "requestId": body["requestId"],
                "operation": "propose",
            },
            "replay": False,
        }, 201)


def call(argv, env):
    return subprocess.run([str(CLI), *argv], env=env, text=True, capture_output=True, timeout=20)


parser = argparse.ArgumentParser()
parser.add_argument("--output-dir", help="external private evidence directory")
args = parser.parse_args()
evidence_dir = pathlib.Path(args.output_dir) if args.output_dir else pathlib.Path(
    tempfile.mkdtemp(prefix="phase-successor-evidence-")
)
evidence_dir.mkdir(mode=0o700, parents=True, exist_ok=True)

server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
env = {key: value for key, value in os.environ.items() if not key.startswith("TAILTERM_")}
env.update({
    "TAILTERM_HUB": f"http://127.0.0.1:{server.server_port}",
    "TAILTERM_TOKEN": TOKEN,
    "TAILTERM_TASK": TASK,
    "TAILTERM_AGENT": AGENT,
    "TAILTERM_RUN": RUN,
})
results = []

try:
    request_path = evidence_dir / "phase-proposal.json"
    request_path.write_text(json.dumps({
        "requestId": "phase-smoke-propose",
        "data": {
            "kind": "acceptance",
            "acceptance": {
                "phase": {"version": 1, "key": "candidate", "terminal": False},
            },
        },
    }, separators=(",", ":")) + "\n")
    os.chmod(request_path, 0o600)

    response = call(["operational-record", "propose", "--file", str(request_path)], env)
    assert response.returncode == 0, response.stderr
    proposed = json.loads(response.stdout)
    assert proposed["record"]["data"]["acceptance"]["phase"]["terminal"] is False
    results.append({"case": "v2_phase_proposal_serialization", "passed": True})

    response = call(["operational-record", "phase-successor", "get", OBLIGATION], env)
    assert response.returncode == 0, response.stderr
    obligation = json.loads(response.stdout)
    assert obligation["id"] == OBLIGATION and obligation["state"] == "pending"
    results.append({"case": "v2_phase_successor_lookup", "passed": True})

    mode = "v1"
    for argv, label in [
        (["operational-record", "propose", "--file", str(request_path)], "phase_proposal_v1_fail_closed"),
        (["operational-record", "phase-successor", "get", OBLIGATION], "phase_lookup_v1_fail_closed"),
    ]:
        before = len(requests)
        response = call(argv, env)
        assert response.returncode != 0 and "v2" in response.stderr
        assert requests[before:] == [{"method": "GET", "path": "/v1/capabilities"}]
        results.append({"case": label, "passed": True, "unsafeRequestSent": False})
finally:
    server.shutdown()
    server.server_close()

evidence = {
    "version": 1,
    "result": "pass",
    "candidateCli": {"bytes": CLI.stat().st_size, "sha256": CLI_SHA256},
    "capability": {"name": "operationalRecords", "version": 2},
    "isolatedLoopbackHTTPOnly": True,
    "syntheticIdentitiesOnly": True,
    "liveDatabaseHubQueueTmuxOrRuntimeUsed": False,
    "cases": results,
    "capturedRequests": requests,
    "limits": [
        "This smoke proves packaged CLI v2 gating and exact accepted-phase proposal/lookup serialization only.",
        "Atomic obligation creation, evidence fencing, no-op admission, retry and lifecycle semantics remain covered by focused Go tests and independent QA.",
    ],
}
evidence_path = evidence_dir / "phase-successor-cli-smoke.json"
evidence_path.write_text(json.dumps(evidence, indent=2) + "\n")
os.chmod(evidence_path, 0o600)
print(json.dumps({"pass": len(results), "httpRequests": len(requests), "evidence": str(evidence_path)}))
