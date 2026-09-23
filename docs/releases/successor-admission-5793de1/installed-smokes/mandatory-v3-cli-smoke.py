#!/usr/bin/env python3
"""Isolated packaged-CLI smoke for reliable-delivery capability v3.

Uses only a loopback synthetic HTTP server and synthetic identities. It does not
touch a Tailterm hub, task database, tmux socket, or runtime provider.
"""

import argparse
import hashlib
import http.server
import json
import os
import pathlib
import subprocess
import tempfile
import threading
from urllib.parse import parse_qs, urlparse


ROOT = pathlib.Path(__file__).resolve().parent
CLI = ROOT / "tt-darwin-arm64"
CLI_SHA256 = "2c23f9975462afb0bb587d7e213ae6272cae6062e6cf9a09bfba451133b1b378"
assert hashlib.sha256(CLI.read_bytes()).hexdigest() == CLI_SHA256

TASK = "tsk_0000000000000001"
AGENT = "agt_0000000000000002"
RUN = "run_0000000000000003"
PRODUCER = "agt_0000000000000004"
PRODUCER_RUN = "run_0000000000000005"
TOKEN = "synthetic-mandatory-v3-smoke"
RESPONSIBILITIES = [
    ("wi_0000000000000011", "execute", "item_worker", "execution"),
    ("wi_0000000000000012", "coordinate", "project_lead", "independent_dispatch"),
    ("wi_0000000000000013", "persist", "database_handler", "database_operation"),
]
requests = []
mode = "v3"


def delivery(item, action, recipient, action_class, delivery_id="dly_0000000000000020", phase="unacknowledged"):
    return {
        "id": delivery_id,
        "taskId": TASK,
        "messageSeq": 41,
        "kind": "assignment",
        "recipientKind": recipient,
        "actionKey": action,
        "actionClass": action_class,
        "agentId": AGENT,
        "runId": RUN,
        "itemTaskId": TASK,
        "itemId": item,
        "itemRevision": 5,
        "workOrderMessage": {"taskId": TASK, "seq": 39},
        "governingOrderMessage": {"taskId": TASK, "seq": 40},
        "instructionSha256": "a" * 64,
        "instructionBytes": 31,
        "enrollmentVersion": 3,
        "contextDigest": "b" * 64,
        "generation": 1,
        "current": True,
        "phase": phase,
        "executionEpoch": 1,
        "message": {"seq": 41, "taskId": TASK, "text": "Synthetic immutable instruction"},
    }


def responsibility_from_query(query):
    item = query.get("itemId", [""])[0]
    action = query.get("actionKey", [""])[0]
    for values in RESPONSIBILITIES:
        if values[0] == item and values[1] == action:
            return values
    raise AssertionError(f"unexpected responsibility selector: {item}/{action}")


def mutation(current, operation="create", replay=False, block=None):
    value = {
        "delivery": current,
        "event": {"id": "dlev_0000000000000021", "kind": operation, "sequence": 2},
        "receipt": {
            "id": "drr_0000000000000022",
            "requestId": "synthetic-v3-receipt",
            "operation": operation,
            "createdAt": "2026-09-13T00:00:00Z",
        },
        "replay": replay,
    }
    if block is not None:
        value["block"] = block
    return value


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

    def read_json(self):
        length = int(self.headers.get("Content-Length", "0"))
        return json.loads(self.rfile.read(length)) if length else None

    def record(self, body=None):
        value = {"method": self.command, "path": self.path}
        if body is not None:
            value["body"] = body
        requests.append(value)

    def do_GET(self):
        assert self.headers.get("Authorization") == "Bearer " + TOKEN
        parsed = urlparse(self.path)
        query = parse_qs(parsed.query)
        self.record()
        if parsed.path == "/v1/capabilities":
            versions = [1, 2, 3] if mode == "v3" else [1, 2]
            self.send_json({"schemaVersion": 1, "reliableDelivery": {"supported": True, "versions": versions}})
            return
        values = responsibility_from_query(query)
        assert query.get("runId") == [RUN]
        current = delivery(*values)
        if parsed.path == f"/v1/tasks/{TASK}/agents/{AGENT}/current-assignment":
            self.send_json(current)
            return
        if parsed.path == f"/v1/tasks/{TASK}/agents/{AGENT}/delivery-coverage":
            self.send_json({
                "taskId": TASK,
                "agentId": AGENT,
                "runId": RUN,
                "itemTaskId": TASK,
                "itemId": values[0],
                "itemRevision": 5,
                "currentItemRevision": 5,
                "bindingWorkOrderMessage": {"taskId": TASK, "seq": 39},
                "contextDigest": "b" * 64,
                "agentStatus": "running",
                "status": "covered",
                "reason": "synthetic exact responsibility",
                "delivery": current,
            })
            return
        self.send_json({"error": "not found"}, 404)

    def do_POST(self):
        assert self.headers.get("Authorization") == "Bearer " + TOKEN
        body = self.read_json()
        self.record(body)
        if self.path == f"/v1/tasks/{TASK}/required-deliveries":
            assert body["enrollmentVersion"] == 3
            assert body["producerAgentId"] == PRODUCER and body["producerRunId"] == PRODUCER_RUN
            values = (body["itemId"], body["actionKey"], body["recipientKind"], body["actionClass"])
            assert values in RESPONSIBILITIES
            self.send_json(mutation(delivery(*values)), 201)
            return
        base = f"/v1/tasks/{TASK}/deliveries/dly_0000000000000020"
        current = delivery(*RESPONSIBILITIES[0])
        if self.path == base + "/blocks":
            assert body == {
                "requestId": "smoke-block",
                "agentId": AGENT,
                "runId": RUN,
                "expectedEpoch": 1,
                "reasonClass": "dependency",
                "text": "synthetic dependency",
            }
            block = {
                "id": "dlb_0000000000000023",
                "deliveryId": current["id"],
                "executionEpoch": 1,
                "reasonClass": "dependency",
                "text": "synthetic dependency",
                "resolved": False,
                "createdAt": "2026-09-13T00:00:00Z",
            }
            self.send_json(mutation(dict(current, phase="blocked"), "blocked", block=block))
            return
        if self.path == base + "/blocks/dlb_0000000000000023/resolutions":
            assert body["requestId"] == "smoke-resolve" and body["expectedEpoch"] == 1
            assert body["agentId"] == PRODUCER and body["runId"] == PRODUCER_RUN
            self.send_json(mutation(dict(current, phase="blocked"), "block_resolved"))
            return
        if self.path == base + "/resume":
            assert body == {
                "requestId": "smoke-resume",
                "agentId": AGENT,
                "runId": RUN,
                "expectedEpoch": 1,
                "resolutionId": "dlr_0000000000000024",
            }
            self.send_json(mutation(dict(current, phase="progressing", executionEpoch=2), "resumed"))
            return
        if self.path == base + "/recovery-incidents":
            assert body["causeStatus"] == "unknown"
            assert body["expectedGeneration"] == 1 and body["expectedEpoch"] == 1
            assert body["prevention"]["verificationCriterion"] == "synthetic prevention verification"
            incident = dict(body)
            incident.update({
                "id": "dri_0000000000000025",
                "deliveryId": current["id"],
                "generation": 1,
                "executionEpoch": 1,
                "recordedByAgentId": PRODUCER,
                "recordedByRunId": PRODUCER_RUN,
                "createdAt": "2026-09-13T00:00:00Z",
            })
            self.send_json({
                "delivery": current,
                "incident": incident,
                "event": {"id": "dlev_0000000000000026", "kind": "recovery_incident_recorded", "sequence": 3},
                "receipt": {"id": "drr_0000000000000027", "requestId": body["requestId"], "operation": "incident"},
                "replay": False,
            })
            return
        self.send_json({"error": "not found"}, 404)


parser = argparse.ArgumentParser()
parser.add_argument("--output-dir", help="external private evidence directory")
args = parser.parse_args()
evidence_dir = pathlib.Path(args.output_dir) if args.output_dir else pathlib.Path(tempfile.mkdtemp(prefix="mandatory-v3-evidence-"))
evidence_dir.mkdir(mode=0o700, parents=True, exist_ok=True)

server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
base_env = {key: value for key, value in os.environ.items() if not key.startswith("TAILTERM_")}
base_env.update(TAILTERM_HUB=f"http://127.0.0.1:{server.server_port}", TAILTERM_TOKEN=TOKEN, TAILTERM_TASK=TASK)
producer_env = dict(base_env, TAILTERM_AGENT=PRODUCER, TAILTERM_RUN=PRODUCER_RUN)
worker_env = dict(base_env, TAILTERM_AGENT=AGENT, TAILTERM_RUN=RUN)
results = []


def call(argv, env):
    return subprocess.run([str(CLI), *argv], env=env, text=True, capture_output=True, timeout=20)


try:
    for index, values in enumerate(RESPONSIBILITIES):
        item, action, recipient, action_class = values
        request = {
            "requestId": f"smoke-v3-enroll-{index}",
            "messageSeq": 41,
            "kind": "assignment",
            "recipientKind": recipient,
            "actionKey": action,
            "actionClass": action_class,
            "agentId": AGENT,
            "runId": RUN,
            "itemTaskId": TASK,
            "itemId": item,
            "itemRevision": 5,
            "workOrderMessage": {"taskId": TASK, "seq": 39},
            "governingOrderMessage": {"taskId": TASK, "seq": 40},
            "instructionSha256": "a" * 64,
            "instructionBytes": 31,
            "enrollmentVersion": 3,
            "expectedCurrentGeneration": 0,
        }
        path = evidence_dir / f"enrollment-{index}.json"
        path.write_text(json.dumps(request) + "\n")
        os.chmod(path, 0o600)
        response = call(["delivery", "create", "--file", str(path), "--json"], producer_env)
        assert response.returncode == 0, response.stderr
        created = json.loads(response.stdout)["delivery"]
        assert (created["recipientKind"], created["actionKey"], created["actionClass"]) == (recipient, action, action_class)
        response = call(["current-assignment", "--item", item, "--action-key", action, "--json"], worker_env)
        assert response.returncode == 0, response.stderr
        current = json.loads(response.stdout)
        assert current["itemId"] == item and current["actionKey"] == action
        response = call(["delivery", "coverage", "--item", item, "--action-key", action, "--json"], worker_env)
        assert response.returncode == 0, response.stderr
        assert json.loads(response.stdout)["status"] == "covered"
        results.append({"case": f"{recipient}_independent_responsibility", "passed": True})

    before = len(requests)
    response = call(["current-assignment", "--item", RESPONSIBILITIES[0][0], "--json"], worker_env)
    assert response.returncode != 0 and "--action-key" in response.stderr and len(requests) == before
    results.append({"case": "paired_selector_required_before_http", "passed": True})

    block = call(["delivery", "block", "--request-id", "smoke-block", "--expected-epoch", "1", "--reason", "dependency", "--text", "synthetic dependency", "--json", "dly_0000000000000020"], worker_env)
    assert block.returncode == 0, block.stderr
    resolve = call(["delivery", "resolve", "--request-id", "smoke-resolve", "--expected-epoch", "1", "--block-id", "dlb_0000000000000023", "--text", "dependency satisfied", "--json", "dly_0000000000000020"], producer_env)
    assert resolve.returncode == 0, resolve.stderr
    resume = call(["delivery", "resume", "--request-id", "smoke-resume", "--expected-epoch", "1", "--resolution-id", "dlr_0000000000000024", "--json", "dly_0000000000000020"], worker_env)
    assert resume.returncode == 0, resume.stderr
    results.append({"case": "dependency_block_resolution_resume_serialization", "passed": True})

    incident_request = {
        "requestId": "smoke-required-cause",
        "expectedGeneration": 1,
        "expectedEpoch": 1,
        "causeStatus": "unknown",
        "lastSubstantiveAction": "synthetic checkpoint",
        "lastSubstantiveAt": "2026-09-13T00:00:00Z",
        "expectedNextAction": "synthetic next step",
        "stopReason": "synthetic unexplained stop",
        "causalEvidence": ["synthetic evidence"],
        "contributingConditions": ["synthetic condition"],
        "unresolvedQuestions": ["synthetic question"],
        "prevention": {
            "ownerAgentId": PRODUCER,
            "ownerRunId": PRODUCER_RUN,
            "workOrderMessage": {"taskId": TASK, "seq": 39},
            "verificationCriterion": "synthetic prevention verification",
        },
    }
    incident_path = evidence_dir / "incident.json"
    incident_path.write_text(json.dumps(incident_request) + "\n")
    os.chmod(incident_path, 0o600)
    incident = call(["delivery", "incident", "--file", str(incident_path), "--json", "dly_0000000000000020"], producer_env)
    assert incident.returncode == 0, incident.stderr
    assert json.loads(incident.stdout)["incident"]["causeStatus"] == "unknown"
    results.append({"case": "required_cause_incident_serialization", "passed": True})

    mode = "v2"
    v3_request_path = evidence_dir / "enrollment-0.json"
    for command_env, argv, label in [
        (producer_env, ["delivery", "create", "--file", str(v3_request_path), "--json"], "v3_enrollment_old_hub"),
        (worker_env, ["current-assignment", "--item", RESPONSIBILITIES[0][0], "--action-key", RESPONSIBILITIES[0][1], "--json"], "v3_selector_old_hub"),
    ]:
        before = len(requests)
        response = call(argv, command_env)
        assert response.returncode != 0 and "version 3" in response.stderr
        assert all(urlparse(value["path"]).path == "/v1/capabilities" for value in requests[before:])
        results.append({"case": label, "passed": True, "unsafeRequestSent": False})
finally:
    server.shutdown()
    server.server_close()

evidence = {
    "version": 1,
    "result": "pass",
    "candidateCli": {"bytes": CLI.stat().st_size, "sha256": CLI_SHA256},
    "capabilityVersion": 3,
    "isolatedLoopbackHTTPOnly": True,
    "syntheticIdentitiesOnly": True,
    "liveDatabaseHubTmuxOrRuntimeUsed": False,
    "cases": results,
    "capturedRequests": requests,
    "limits": [
        "This packaged-binary smoke proves CLI capability gating and exact HTTP selector/action serialization.",
        "Store/server state, CAS, replay, recovery-cause validation, migration and lifecycle semantics are covered by focused Go tests and independent QA.",
    ],
}
evidence_path = evidence_dir / "mandatory-v3-cli-smoke.json"
evidence_path.write_text(json.dumps(evidence, indent=2) + "\n")
os.chmod(evidence_path, 0o600)
print(json.dumps({"pass": len(results), "httpRequests": len(requests), "liveDataUsed": False, "evidence": str(evidence_path)}))
