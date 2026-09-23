#!/usr/bin/env python3
import argparse
import hashlib
import http.server
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import threading


root = pathlib.Path(__file__).resolve().parent
cli = root / "tt-darwin-arm64"
candidate_sha = "7a63670a9210c7f5208cb500845e53d2f9fd42cc4f33d175c3221cf5d9d060c1"
assert hashlib.sha256(cli.read_bytes()).hexdigest() == candidate_sha
parser = argparse.ArgumentParser()
parser.add_argument("--output-dir", help="external evidence directory (default: a new private /tmp directory)")
parser.add_argument("--normalize-observation", action="store_true", help="normalize only the retained observedAt field for the pinned canonical fixture")
arguments = parser.parse_args()
evidence_dir = pathlib.Path(arguments.output_dir) if arguments.output_dir else pathlib.Path(tempfile.mkdtemp(prefix="reliable-followthrough-evidence-"))
evidence_dir.mkdir(mode=0o700, parents=True, exist_ok=True)

task = "tsk_0000000000000001"
agent = "agt_0000000000000002"
run = "run_0000000000000003"
producer = "agt_0000000000000004"
producer_run = "run_0000000000000005"
delivery_id = "dly_0000000000000006"
thread_id = "00000000-0000-4000-8000-000000000007"
token = "synthetic-followthrough-smoke"
mode = "v2"
scenario = "cli"
requests = []
coverage_calls = 0
report_calls = 0


def delivery(pending_request=""):
    return {
        "id": delivery_id,
        "taskId": task,
        "messageSeq": 41,
        "kind": "assignment",
        "agentId": agent,
        "runId": run,
        "itemTaskId": task,
        "itemId": "wi_0000000000000008",
        "itemRevision": 5,
        "workOrderMessage": {"taskId": task, "seq": 39},
        "governingOrderMessage": {"taskId": task, "seq": 40},
        "instructionSha256": "a" * 64,
        "instructionBytes": 31,
        "enrollmentVersion": 2,
        "contextDigest": "b" * 64,
        "generation": 2,
        "current": True,
        "phase": "unacknowledged",
        "executionEpoch": 3,
        "followThrough": {
            "policy": {
                "ackDeadlineSeconds": 120,
                "progressDeadlineSeconds": 600,
                "resumeDeadlineSeconds": 120,
                "confirmationDeadlineSeconds": 120,
                "dispatchReportSeconds": 30,
                "activeToolHardLimitSeconds": 1800,
                "maxQueueAttempts": 2,
            },
            "attemptCount": 0,
            "pendingAction": "queue_current_assignment" if pending_request else "",
            "pendingRequestId": pending_request,
        },
        "message": {"seq": 41, "taskId": task, "text": "Synthetic immutable instruction"},
    }


def mutation(replay=False):
    return {
        "delivery": delivery(),
        "event": {"id": "dlev_0000000000000009", "kind": "enrolled", "sequence": 1},
        "receipt": {
            "id": "drcp_0000000000000010",
            "requestId": "smoke-enroll-v2",
            "operation": "create",
            "createdAt": "2026-09-13T00:00:00Z",
        },
        "replay": replay,
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

    def request_body(self):
        length = int(self.headers.get("Content-Length", "0"))
        return json.loads(self.rfile.read(length)) if length else None

    def record(self, body=None):
        value = {"method": self.command, "path": self.path}
        if body is not None:
            value["body"] = body
        requests.append(value)

    def do_GET(self):
        global coverage_calls
        if scenario == "cli":
            assert self.headers.get("Authorization") == "Bearer " + token
        self.record()
        if self.path == "/v1/capabilities":
            versions = [1, 2] if mode == "v2" else [1]
            self.send_json({"schemaVersion": 1, "reliableDelivery": {"supported": mode != "missing", "versions": versions}})
            return
        if self.path == "/v1/tasks":
            self.send_json({"tasks": []})
            return
        coverage_path = f"/v1/tasks/{task}/agents/{agent}/delivery-coverage?runId={run}"
        if self.path == coverage_path or self.path.startswith(f"/v1/tasks/{task}/agents/{agent}/delivery-coverage?"):
            coverage_calls += 1
            if scenario == "followthrough_retry" and report_calls < 2:
                pending = requests_by_suffix("/follow-through/check")[-1]["body"]["requestId"] if coverage_calls > 1 else ""
                self.send_json({"taskId": task, "agentId": agent, "runId": run, "agentStatus": "running", "status": "covered", "reason": "synthetic enrolled directive", "delivery": delivery(pending)})
            elif scenario == "cli":
                self.send_json({"taskId": task, "agentId": agent, "runId": run, "agentStatus": "running", "status": "covered", "reason": "synthetic enrolled directive", "delivery": delivery()})
            else:
                self.send_json({"taskId": task, "agentId": agent, "runId": run, "agentStatus": "running", "status": "uncovered_unverified", "reason": "synthetic stop"})
            return
        self.send_json({"error": "not found"}, 404)

    def do_POST(self):
        global report_calls
        if scenario == "cli":
            assert self.headers.get("Authorization") == "Bearer " + token
        body = self.request_body()
        self.record(body)
        if self.path == f"/v1/tasks/{task}/required-deliveries":
            assert body["producerAgentId"] == producer
            assert body["producerRunId"] == producer_run
            assert body["agentId"] == agent and body["runId"] == run
            assert body["governingOrderMessage"] == {"taskId": task, "seq": 40}
            assert body["instructionSha256"] == "a" * 64 and body["instructionBytes"] == 31
            self.send_json(mutation(), 201)
            return
        if self.path.endswith("/follow-through/check"):
            assert body["agentId"] == agent and body["runId"] == run
            assert body["expectedGeneration"] == 2 and body["expectedEpoch"] == 3
            assert body["observation"]["state"] == "unknown"
            assert body["observation"]["source"] == "codex_queue_only"
            self.send_json({
                "delivery": delivery(),
                "classification": "overdue_unknown",
                "reason": "synthetic deadline",
                "action": "queue_current_assignment",
                "attempt": 1,
                "execute": True,
                "replay": False,
            })
            return
        if self.path.endswith("/follow-through/report"):
            report_calls += 1
            if scenario == "followthrough_retry" and report_calls == 1:
                self.send_json({"error": "synthetic response loss"}, 500)
                return
            if scenario == "followthrough_invalidation" and report_calls == 1:
                self.send_json({"error": "stale lease"}, 409)
                return
            self.send_json({
                "delivery": delivery(),
                "event": {"id": "dlev_0000000000000011", "kind": "followthrough_queue_" + body["outcome"], "sequence": 2},
                "receipt": {"id": "drcp_0000000000000012", "requestId": body["requestId"], "operation": "followthrough_report", "createdAt": "2026-09-13T00:00:00Z"},
                "replay": scenario == "followthrough_retry",
            })
            return
        self.send_json({"error": "not found"}, 404)


def requests_by_suffix(suffix):
    return [value for value in requests if value["path"].endswith(suffix)]


server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
temp = pathlib.Path(tempfile.mkdtemp(prefix="reliable-followthrough-smoke-"))
relay_state = temp / "relay"
relay_state.mkdir(mode=0o700)
codex_log = temp / "fake-codex.log"
fake_codex = temp / "codex"
fake_codex.write_text("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_CODEX_LOG\"\nprintf 'Queued message\\n'\n")
fake_codex.chmod(0o700)

env = {key: value for key, value in os.environ.items() if not key.startswith("TAILTERM_")}
env.update(
    TAILTERM_HUB=f"http://127.0.0.1:{server.server_port}",
    TAILTERM_TOKEN=token,
    TAILTERM_TASK=task,
    TAILTERM_AGENT=producer,
    TAILTERM_RUN=producer_run,
    TAILTERM_RELAY_STATE=str(relay_state),
    TT_TMUX_SOCKET="synthetic-no-live-tmux",
    FAKE_CODEX_LOG=str(codex_log),
)


def call(args, command_env=None):
    return subprocess.run([str(cli), *args], env=command_env or env, text=True, capture_output=True, timeout=20)


request_v2 = {
    "requestId": "smoke-enroll-v2",
    "messageSeq": 41,
    "kind": "assignment",
    "agentId": agent,
    "runId": run,
    "itemTaskId": task,
    "itemId": "wi_0000000000000008",
    "itemRevision": 5,
    "workOrderMessage": {"taskId": task, "seq": 39},
    "governingOrderMessage": {"taskId": task, "seq": 40},
    "instructionSha256": "a" * 64,
    "instructionBytes": 31,
    "enrollmentVersion": 2,
    "expectedCurrentGeneration": 0,
}
request_v1 = dict(request_v2, requestId="smoke-enroll-v1", enrollmentVersion=1)
v2_path = temp / "enrollment-v2.json"
v1_path = temp / "enrollment-v1.json"
v2_path.write_text(json.dumps(request_v2) + "\n")
v1_path.write_text(json.dumps(request_v1) + "\n")
results = []

response = call(["delivery", "create", "--task", task, "--file", str(v2_path), "--json"])
assert response.returncode == 0, response.stderr
assert json.loads(response.stdout)["delivery"]["enrollmentVersion"] == 2
results.append({"operation": "v2_enrollment", "passed": True})

worker_env = dict(env, TAILTERM_AGENT=agent, TAILTERM_RUN=run)
response = call(["delivery", "coverage", "--task", task, "--json"], worker_env)
assert response.returncode == 0, response.stderr
assert json.loads(response.stdout)["status"] == "covered"
results.append({"operation": "v2_coverage", "passed": True})

mode = "v1"
response = call(["delivery", "create", "--task", task, "--file", str(v1_path), "--json"])
assert response.returncode == 0, response.stderr
results.append({"operation": "v1_enrollment_on_v1_hub", "passed": True})

before = len(requests)
response = call(["delivery", "create", "--task", task, "--file", str(v2_path), "--json"])
assert response.returncode != 0 and "version 2" in response.stderr
new_requests = requests[before:]
assert len(new_requests) == 2 and all(value["path"] == "/v1/capabilities" for value in new_requests)
results.append({"operation": "v2_enrollment_on_v1_hub", "passed": True, "actionRequestSent": False})

mode = "missing"
before = len(requests)
response = call(["delivery", "coverage", "--task", task, "--json"], worker_env)
assert response.returncode != 0 and "unsupported" in response.stderr
assert len(requests) == before + 1 and requests[-1]["path"] == "/v1/capabilities"
results.append({"operation": "unknown_hub_fail_closed", "passed": True, "actionRequestSent": False})


def install_binding():
    shutil.rmtree(relay_state)
    relay_state.mkdir(mode=0o700)
    hub = f"http://127.0.0.1:{server.server_port}"
    key = hashlib.sha256(hub.encode()).hexdigest() + "-" + agent
    binding = {"hub": hub, "task": task, "agent": agent, "run": run, "thread": thread_id, "codex": str(fake_codex)}
    binding_path = relay_state / (key + ".binding.json")
    binding_path.write_text(json.dumps(binding))
    return binding_path, relay_state / (key + ".progress.json")


mode = "v2"
scenario = "followthrough_retry"
coverage_calls = 0
report_calls = 0
binding_path, progress_path = install_binding()
response = call(["relay", "--once"])
assert response.returncode == 0
progress_after_loss = json.loads(progress_path.read_text())
assert progress_after_loss["pendingFollowThroughReport"]["outcome"] == "accepted"
assert len(codex_log.read_text().splitlines()) == 1
first_reports = list(requests_by_suffix("/follow-through/report"))
assert len(first_reports) == 1
response = call(["relay", "--once"])
assert response.returncode == 0
progress_after_retry = json.loads(progress_path.read_text())
assert "pendingFollowThroughReport" not in progress_after_retry
retry_reports = requests_by_suffix("/follow-through/report")
assert len(retry_reports) == 2 and retry_reports[0]["body"] == retry_reports[1]["body"]
assert len(codex_log.read_text().splitlines()) == 1
results.append({"operation": "followthrough_response_loss_replay", "passed": True, "fakeCodexCalls": 1, "stableReportReplay": True})

scenario = "followthrough_invalidation"
coverage_calls = 0
report_calls = 0
codex_log.write_text("")
binding_path, progress_path = install_binding()
pending = {
    "run": run,
    "thread": thread_id,
    "followThroughCheckedAt": "2026-09-13T00:00:00Z",
    "followThroughSupported": True,
    "pendingFollowThroughDelivery": delivery_id,
    "pendingFollowThroughReport": {
        "requestId": "stale-report",
        "agentId": agent,
        "runId": run,
        "expectedGeneration": 2,
        "expectedEpoch": 3,
        "leaseRequestId": "stale-lease",
        "outcome": "accepted",
        "text": "synthetic old report",
    },
}
progress_path.write_text(json.dumps(pending))
before_reports = len(requests_by_suffix("/follow-through/report"))
response = call(["relay", "--once"])
assert response.returncode == 0
invalidation_reports = requests_by_suffix("/follow-through/report")[before_reports:]
assert len(invalidation_reports) == 2
assert invalidation_reports[0]["body"]["outcome"] == "accepted"
assert invalidation_reports[1]["body"]["outcome"] == "invalidated"
assert invalidation_reports[1]["body"]["leaseRequestId"] == "stale-lease"
assert "pendingFollowThroughReport" not in json.loads(progress_path.read_text())
assert not codex_log.read_text()
results.append({"operation": "followthrough_stale_report_invalidation", "passed": True, "fakeCodexCalls": 0})

server.shutdown()
server.server_close()
shutil.rmtree(temp)

# The default external evidence retains the real per-run UTC observation. The
# opt-in normalization exists only to produce the separately labeled canonical
# package fixture; it never changes the request sent to the fake HTTP endpoint.
stable_requests = json.loads(json.dumps(requests))
normalized_fields = []
if arguments.normalize_observation:
    for captured in stable_requests:
        observation = (captured.get("body") or {}).get("observation")
        if observation is not None and "observedAt" in observation:
            observation["observedAt"] = "2026-09-13T00:00:00Z"
    normalized_fields.append("follow-through check body observation.observedAt")

evidence = {
    "version": 1,
    "candidateCli": {"bytes": cli.stat().st_size, "sha256": candidate_sha},
    "isolatedHTTPOnly": True,
    "syntheticCredentialsOnly": True,
    "fakeCodexExecutableOnly": True,
    "actualCodexQueueInvoked": False,
    "liveDatabaseOrHubUsed": False,
    "results": results,
    "capturedRequests": stable_requests,
    "rawRuntimeObservationsRetained": not arguments.normalize_observation,
    "normalizedVolatileFields": normalized_fields,
    "limits": [
        "Packaged CLI smoke proves capability gating and HTTP serialization only.",
        "Store/server state-machine, race, migration and lifecycle semantics are covered by the accepted focused Go suites and independent QA #4236/#4237.",
        "The fake Codex executable only verifies exact relay subprocess dispatch and never contacts a runtime provider.",
    ],
}
evidence_path = evidence_dir / "reliable-followthrough-cli-smoke.json"
evidence_path.write_text(json.dumps(evidence, indent=2) + "\n")
print(json.dumps({"pass": len(results), "httpRequests": len(requests), "actualCodexQueueInvoked": False, "evidence": str(evidence_path)}))
