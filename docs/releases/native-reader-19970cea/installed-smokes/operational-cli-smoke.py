#!/usr/bin/env python3
import argparse
import hashlib
import http.server
import json
import os
import pathlib
import subprocess
import tempfile
import threading

root = pathlib.Path(__file__).resolve().parent
cli = root / "tt-darwin-arm64"
candidate_sha = "f9cb8b7e480c3c32ed1a8de567cb88d0373d4e1ca4d99b4e77ba37c6134a4198"
assert hashlib.sha256(cli.read_bytes()).hexdigest() == candidate_sha
parser = argparse.ArgumentParser()
parser.add_argument("--output-dir", help="external evidence directory (default: a new private /tmp directory)")
arguments = parser.parse_args()
evidence_dir = pathlib.Path(arguments.output_dir) if arguments.output_dir else pathlib.Path(tempfile.mkdtemp(prefix="operational-regression-evidence-"))
evidence_dir.mkdir(mode=0o700, parents=True, exist_ok=True)

task = "tsk_0000000000000001"
agent = "agt_0000000000000002"
run = "run_0000000000000003"
record = "opr_0000000000000004"
mode = "supported"
requests = []


def record_body(state):
    return {
        "id": record,
        "taskId": task,
        "version": 1,
        "state": state,
        "data": {"kind": "instruction"},
        "sources": [],
        "actorAgentId": agent,
        "actorRunId": run,
        "by": {"node": "synthetic", "user": "owner"},
        "createdAt": "2026-09-13T00:00:00Z",
    }


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def send_json(self, value, status=200):
        body = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        assert self.headers.get("Authorization") == "Bearer synthetic-operational-smoke"
        requests.append({"method": "GET", "path": self.path})
        if self.path == "/v1/capabilities":
            if mode == "supported":
                capability = {"supported": True, "versions": [1]}
            elif mode == "unknown":
                capability = {"supported": True, "versions": [999]}
            else:
                capability = {"supported": False, "versions": [1]}
            self.send_json({"schemaVersion": 1, "operationalRecords": capability})
            return
        assert self.path == f"/v1/tasks/{task}/operational-records/{record}"
        self.send_json(record_body("proposed"))

    def do_POST(self):
        assert self.headers.get("Authorization") == "Bearer synthetic-operational-smoke"
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        requests.append({"method": "POST", "path": self.path, "body": body})
        assert body["agentId"] == agent and body["runId"] == run
        if self.path == f"/v1/tasks/{task}/operational-records":
            assert body["requestId"] == "package-propose"
            assert body["data"]["kind"] == "instruction"
            response = {
                "record": record_body("proposed"),
                "receipt": {"id": "drcp_0000000000000005"},
                "replay": False,
            }
            self.send_json(response)
            return
        assert self.path == f"/v1/tasks/{task}/operational-records/{record}/commit"
        assert body == {
            "requestId": "package-commit",
            "expectedVersion": 1,
            "agentId": agent,
            "runId": run,
        }
        response = {
            "record": record_body("committed"),
            "receipt": {"id": "drcp_0000000000000006"},
            "replay": False,
        }
        self.send_json(response)


server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
env = {key: value for key, value in os.environ.items() if not key.startswith("TAILTERM_")}
env.update(
    TAILTERM_HUB=f"http://127.0.0.1:{server.server_port}",
    TAILTERM_TOKEN="synthetic-operational-smoke",
    TAILTERM_TASK=task,
    TAILTERM_AGENT=agent,
    TAILTERM_RUN=run,
)
proposal = evidence_dir / "operational-proposal.json"
proposal.write_text(json.dumps({"requestId": "package-propose", "data": {"kind": "instruction"}}) + "\n")
commit = evidence_dir / "operational-commit.json"
commit.write_text(json.dumps({"requestId": "package-commit", "expectedVersion": 1}) + "\n")
results = []


def call(args):
    return subprocess.run([str(cli), *args], env=env, text=True, capture_output=True, timeout=15)


response = call(["operational-record", "propose", "--file", str(proposal)])
assert response.returncode == 0
propose_output = json.loads(response.stdout)
assert propose_output["record"]["state"] == "proposed"
results.append({"operation": "propose", "stdout": propose_output})

response = call(["operational-record", "get", record])
assert response.returncode == 0
get_output = json.loads(response.stdout)
assert get_output["id"] == record and get_output["state"] == "proposed"
results.append({"operation": "get", "stdout": get_output})

response = call(["operational-record", "commit", "--file", str(commit), record])
assert response.returncode == 0
commit_output = json.loads(response.stdout)
assert commit_output["record"]["state"] == "committed"
results.append({"operation": "commit", "stdout": commit_output})

for value in ["unsupported", "unknown"]:
    mode = value
    before = len(requests)
    response = call(["operational-record", "get", record])
    assert response.returncode != 0 and "upgrade" in response.stderr.lower()
    assert len(requests) == before + 1
    assert requests[-1] == {"method": "GET", "path": "/v1/capabilities"}
    results.append({"operation": value, "exitCode": response.returncode, "actionRequestSent": False})

server.shutdown()
server.server_close()
evidence = {
    "candidateCli": {"bytes": cli.stat().st_size, "sha256": candidate_sha},
    "isolatedHTTPOnly": True,
    "syntheticCredentialsOnly": True,
    "liveDatabaseOrHubUsed": False,
    "results": results,
    "requests": requests,
    "limit": "Packaged successor CLI regression smoke only; accepted operational-record QA #4006 covers store semantics and transition enforcement.",
}
evidence_path = evidence_dir / "operational-cli-regression-smoke.json"
evidence_path.write_text(json.dumps(evidence, indent=2) + "\n")
print(json.dumps({"pass": len(results), "requests": len(requests), "limit": evidence["limit"], "evidence": str(evidence_path)}))
