#!/usr/bin/env python3
import hashlib
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

TASK = "tsk_0000000000000001"
ITEM = "wi_0000000000000002"
HANDLER = "agt_0000000000000003"
NOW = "2026-09-13T20:00:00Z"
requests = []


def coverage():
    return {
        "complete": True,
        "observedCurrentRevision": 2,
        "latestMaterializedRevision": 2,
        "snapshotCount": 2,
        "gapCount": 0,
        "conversationLinks": "explicit_only",
        "explicitMessageCount": 2,
        "sourceMessageCount": 0,
        "dispatchCount": 0,
    }


def message(seq, revision):
    return {
        "itemRevision": revision,
        "revisionCoverage": "verified",
        "relationship": "primary",
        "message": {
            "workItems": [{"itemTaskId": TASK, "itemId": ITEM, "itemRevision": revision, "relationship": "primary"}],
            "seq": seq,
            "taskId": TASK,
            "from": {"node": "synthetic", "user": "owner"},
            "text": f"synthetic message {seq}",
            "createdAt": NOW,
        },
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        parsed = urlparse(self.path)
        query = parse_qs(parsed.query)
        requests.append({"method": "GET", "path": parsed.path, "query": query})
        base = f"/v1/tasks/{TASK}/work-items/{ITEM}"
        if parsed.path == f"/v1/tasks/{TASK}/agents/{HANDLER}":
            body = {"id": HANDLER, "taskId": TASK, "role": "database_handler"}
        elif parsed.path == base:
            body = {"id": ITEM, "taskId": TASK, "kind": "feature", "title": "Synthetic", "status": "open", "priority": "normal", "revision": 2, "scopeRevision": 2}
        elif parsed.path == base + "/messages":
            after = int(query.get("after", ["0"])[0])
            if after == 0:
                body = {"links": [message(11, 1)], "nextAfter": 11, "coverage": coverage()}
            elif after == 11:
                body = {"links": [message(12, 2)], "coverage": coverage()}
            else:
                self.send_error(400)
                return
        else:
            self.send_error(404)
            return
        data = json.dumps(body, separators=(",", ":")).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def main():
    package = pathlib.Path(__file__).resolve().parent
    cli = package / "tt-darwin-arm64"
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="reader-cli-smoke-") as temp:
            manifest_path = pathlib.Path(temp) / "manifest.json"
            env = os.environ.copy()
            env.update({
                "TAILTERM_HUB": f"http://127.0.0.1:{server.server_port}",
                "TAILTERM_TASK": TASK,
                "TAILTERM_AGENT": HANDLER,
                "TAILTERM_AGENT_NAME": "synthetic-handler",
                "TAILTERM_TOKEN": "",
            })
            completed = subprocess.run([
                str(cli), "work-items", "evidence",
                "--manifest", str(manifest_path),
                "--sources", "current,messages",
                "--limit", "1", "--json", ITEM,
            ], env=env, text=True, capture_output=True, timeout=20)
            if completed.returncode != 0:
                raise RuntimeError(completed.stderr.strip())
            manifest = json.loads(completed.stdout)
            assert manifest["complete"] is True and manifest["verified"] is True
            assert manifest["snapshot"]["itemRevision"] == 2 and manifest["snapshot"]["scopeRevision"] == 2
            assert manifest["sources"]["current"]["state"] == "verified"
            assert len(manifest["sources"]["current"]["pages"]) == 2
            assert manifest["sources"]["messages"]["state"] == "verified"
            assert manifest["sources"]["messages"]["count"] == 2
            assert len(manifest["sources"]["messages"]["pages"]) == 2
            assert (manifest_path.stat().st_mode & 0o777) == 0o600
            for source in ("current", "messages"):
                for page in manifest["sources"][source]["pages"]:
                    retained = manifest_path.parent / page["path"]
                    data = retained.read_bytes()
                    assert len(data) == page["bytes"]
                    assert hashlib.sha256(data).hexdigest() == page["sha256"]
                    assert (retained.stat().st_mode & 0o777) == 0o600
            assert all(request["method"] == "GET" for request in requests)
            result = {
                "version": 1,
                "result": "pass",
                "executable": "tt-darwin-arm64",
                "cases": [
                    "handler-only actor lookup",
                    "current snapshot start/end bookend",
                    "two-page linked-message traversal",
                    "terminal complete+verified aggregate",
                    "private manifest/page modes and exact byte hashes",
                    "GET-only synthetic HTTP traffic",
                ],
                "requestCount": len(requests),
                "manifestSummary": {
                    "itemRevision": 2,
                    "scopeRevision": 2,
                    "messagePages": 2,
                    "messageCount": 2,
                },
                "liveDataUsed": False,
            }
            if len(sys.argv) > 2:
                raise RuntimeError("usage: reader-cli-smoke.py [EXTERNAL_RESULT_PATH]")
            if len(sys.argv) == 2:
                output = pathlib.Path(sys.argv[1]).resolve()
                output.write_text(json.dumps(result, indent=2) + "\n")
                os.chmod(output, 0o600)
            print(json.dumps(result, separators=(",", ":")))
    finally:
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    main()
