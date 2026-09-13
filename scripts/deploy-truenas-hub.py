#!/usr/bin/env python3
"""Deploy the hub only after validating a handler-owned backup receipt.

Usage:
  scripts/deploy-truenas-hub.py RELEASE --plan PLAN.json \
      --preflight-receipt RECEIPT.json [--update]

This lead-side command never opens, queries, or backs up SQLite. The database
handler runs ``truenas_release_preflight.py`` first and supplies its immutable
receipt. Plan, receipt, local artifact, and a fresh read-only remote identity
probe are all verified before token, release, binary, or middleware mutation.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import secrets
import shlex
import subprocess
import sys
from typing import Any

sys.dont_write_bytecode = True

from truenas_release_preflight import (  # noqa: E402
    CHECKS,
    OPERATION,
    PROFILE_TABLES,
    PreflightFailure,
    load_plan,
    probe_remote_identity,
    ssh_argv,
    validate_receipt,
)


ROOT = pathlib.Path(__file__).resolve().parents[1]
BASE = "/mnt/deepfreeze/tailterm-hub"
APP_NAME = "tailterm-hub"
TCP_LISTENER = "100.116.238.37:18765"
SSH_ROUTE = {
    "id": "truenas-ssh",
    "kind": "ssh",
    "host": "truenas",
    "batchMode": True,
    "connectTimeoutSeconds": 10,
}


def _deployment_plan(plan: dict[str, Any], release: str) -> dict[str, str]:
    if plan["route"] != SSH_ROUTE:
        raise PreflightFailure(
            "invalid-input", "route must be the established truenas SSH route"
        )
    if plan["databaseOwner"] != "db-handler":
        raise PreflightFailure("invalid-input", "databaseOwner must be db-handler")
    if plan["operation"] != OPERATION or plan["checks"] != CHECKS:
        raise PreflightFailure("invalid-input", "backup operation/checks are not approved")
    if plan["profileTables"] != PROFILE_TABLES:
        raise PreflightFailure("invalid-input", "profile evidence tables are not approved")
    if plan["sourceDatabase"] != f"{BASE}/state/hub.sqlite":
        raise PreflightFailure(
            "invalid-input", "sourceDatabase must be the canonical TrueNAS hub database"
        )
    if plan["allowedBackupRoot"] != f"{BASE}/backups":
        raise PreflightFailure(
            "invalid-input", "allowedBackupRoot must be the established backup directory"
        )
    if plan["backupOwner"] != {"uid": 950, "gid": 950}:
        raise PreflightFailure("invalid-input", "backupOwner must be UID/GID 950:950")

    deployment = plan["deployment"]
    expected = {
        "releaseName": release,
        "binaryDestination": f"{BASE}/releases/{release}/tailterm-hub",
        "stateDirectory": f"{BASE}/state",
        "tokenPath": f"{BASE}/hub-token",
        "appName": APP_NAME,
        "tcpListener": TCP_LISTENER,
    }
    for field, value in expected.items():
        if deployment.get(field) != value:
            raise PreflightFailure("invalid-input", f"deployment.{field} must be {value}")
    return expected


def _emit(result: dict[str, Any]) -> None:
    print(json.dumps(result, sort_keys=True, separators=(",", ":")), flush=True)


def _read_receipt(path: pathlib.Path) -> tuple[dict[str, Any], str]:
    try:
        data = path.read_bytes()
        receipt = json.loads(data)
    except (OSError, json.JSONDecodeError) as error:
        raise PreflightFailure(
            "verification-failed", f"cannot read preflight receipt: {error}"
        ) from error
    if not isinstance(receipt, dict):
        raise PreflightFailure("verification-failed", "preflight receipt is not an object")
    return receipt, hashlib.sha256(data).hexdigest()


def _remote_failure(
    completed: subprocess.CompletedProcess[bytes],
    plan: dict[str, Any],
    actual_host: str,
    stage: str,
    last_completed_stage: str,
    preflight: dict[str, Any],
) -> PreflightFailure:
    detail_lines = completed.stderr.decode("utf-8", "replace").strip().splitlines()
    detail = detail_lines[-1][:500] if detail_lines else ""
    common = {
        "phase": "deployment",
        "stage": stage,
        "targetHost": plan["targetHost"],
        "verifiedHost": actual_host,
        "route": plan["route"],
        "mutationStarted": True,
        "backupAlreadyVerified": True,
        "backupMutationCompleted": True,
        "backupDestination": preflight["backupDestination"],
        "backupSha256": preflight["sha256"],
        "lastCompletedStage": last_completed_stage,
    }
    if completed.returncode == 78:
        return PreflightFailure(
            "host-mismatch",
            "execution host changed after the verified backup receipt",
            deploymentMutationStarted=False,
            deploymentMutationState="not-started",
            **common,
        )
    if completed.returncode == 79:
        return PreflightFailure(
            "executable-mismatch",
            "target executable changed after the verified backup receipt",
            deploymentMutationStarted=False,
            deploymentMutationState="not-started",
            **common,
        )
    if completed.returncode == 255:
        return PreflightFailure(
            "route-unavailable",
            "declared SSH route failed during deployment",
            routeExitCode=completed.returncode,
            routeDetail=detail,
            deploymentMutationState="unknown",
            **common,
        )
    return PreflightFailure(
        "remote-operation-failed",
        "verified remote host rejected a deployment operation",
        remoteExitCode=completed.returncode,
        remoteDetail=detail,
        deploymentMutationStarted=True,
        deploymentMutationState="started",
        **common,
    )


def _remote(
    plan: dict[str, Any],
    actual_host: str,
    command: str,
    *,
    stage: str,
    last_completed_stage: str,
    preflight: dict[str, Any],
    data: bytes | None = None,
) -> bytes:
    quoted_host = shlex.quote(actual_host)
    quoted_executable = shlex.quote(plan["targetExecutable"])
    guarded_command = (
        "actual=$(hostname 2>/dev/null) || exit 78; "
        f"if [ \"$actual\" != {quoted_host} ]; then exit 78; fi; "
        f"if [ ! -x {quoted_executable} ]; then exit 79; fi; "
        + command
    )
    try:
        completed = subprocess.run(
            ssh_argv(plan) + [guarded_command],
            input=data,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except OSError as error:
        raise PreflightFailure(
            "route-unavailable",
            f"could not start the declared SSH route: {error}",
            phase="deployment",
            stage=stage,
            route=plan["route"],
            mutationStarted=True,
            backupAlreadyVerified=True,
            backupMutationCompleted=True,
            backupDestination=preflight["backupDestination"],
            backupSha256=preflight["sha256"],
            lastCompletedStage=last_completed_stage,
            deploymentMutationStarted=False,
            deploymentMutationState="not-started",
        ) from error
    if completed.returncode != 0:
        raise _remote_failure(
            completed,
            plan,
            actual_host,
            stage,
            last_completed_stage,
            preflight,
        )
    return completed.stdout


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("release")
    parser.add_argument("--plan", required=True, type=pathlib.Path)
    parser.add_argument("--preflight-receipt", required=True, type=pathlib.Path)
    parser.add_argument("--update", action="store_true")
    arguments = parser.parse_args(argv)

    if not re.fullmatch(r"[A-Za-z0-9._-]+", arguments.release):
        _emit(PreflightFailure("invalid-input", "invalid release name").result())
        return 2

    plan: dict[str, Any] | None = None
    try:
        plan = load_plan(arguments.plan)
        deployment = _deployment_plan(plan, arguments.release)
        raw_receipt, receipt_sha256 = _read_receipt(arguments.preflight_receipt)
        preflight = validate_receipt(plan, raw_receipt)
    except PreflightFailure as error:
        _emit(error.result(plan))
        return 2

    binary = ROOT / ".build" / "ttbin" / "tailterm-hub-linux-amd64"
    try:
        binary_bytes = binary.read_bytes()
    except OSError as error:
        _emit(
            PreflightFailure(
                "local-artifact-unavailable",
                f"cannot read the local hub artifact: {error}",
                phase="deployment",
                stage="local-artifact",
                mutationStarted=False,
                backupAlreadyVerified=True,
                backupDestination=preflight["backupDestination"],
                backupSha256=preflight["sha256"],
            ).result(plan)
        )
        return 2

    identity = probe_remote_identity(plan)
    if identity.get("status") != "verified":
        identity.update(
            {
                "phase": "deployment",
                "stage": "remote-identity",
                "backupAlreadyVerified": True,
                "backupDestination": preflight["backupDestination"],
                "backupSha256": preflight["sha256"],
            }
        )
        _emit(identity)
        return 2
    actual_host = str(identity.get("actualHost"))
    if (
        actual_host != plan["targetHost"]
        or identity.get("resolvedExecutable") != preflight.get("resolvedExecutable")
    ):
        _emit(
            PreflightFailure(
                "verification-failed",
                "fresh host/executable identity does not match handler receipt",
                phase="deployment",
                stage="remote-identity",
                targetHost=plan["targetHost"],
                observedHost=actual_host,
                targetExecutable=plan["targetExecutable"],
                observedExecutable=identity.get("resolvedExecutable"),
                mutationStarted=False,
                backupAlreadyVerified=True,
                backupDestination=preflight["backupDestination"],
                backupSha256=preflight["sha256"],
            ).result(plan)
        )
        return 2

    last_completed_stage = "remote-identity"
    stage = "token"
    try:
        token_path = shlex.quote(deployment["tokenPath"])
        _remote(
            plan,
            actual_host,
            f"umask 077; test -s {token_path} || cat > {token_path}",
            stage=stage,
            last_completed_stage=last_completed_stage,
            preflight=preflight,
            data=(secrets.token_urlsafe(48) + "\n").encode(),
        )
        last_completed_stage = stage

        stage = "release-directory"
        binary_destination = deployment["binaryDestination"]
        release_directory = shlex.quote(
            str(pathlib.PurePosixPath(binary_destination).parent)
        )
        _remote(
            plan,
            actual_host,
            f"mkdir -p {release_directory}",
            stage=stage,
            last_completed_stage=last_completed_stage,
            preflight=preflight,
        )
        last_completed_stage = stage

        stage = "binary-upload"
        quoted_binary = shlex.quote(binary_destination)
        _remote(
            plan,
            actual_host,
            f"cat > {quoted_binary} && chmod 755 {quoted_binary}",
            stage=stage,
            last_completed_stage=last_completed_stage,
            preflight=preflight,
            data=binary_bytes,
        )
        last_completed_stage = stage

        compose = {
            "services": {
                "hub": {
                    "image": "gcr.io/distroless/static-debian12:nonroot",
                    "user": "950:950",
                    "restart": "unless-stopped",
                    "entrypoint": ["/opt/tailterm-hub"],
                    "read_only": True,
                    "cap_drop": ["ALL"],
                    "security_opt": ["no-new-privileges:true"],
                    "ports": [f"{deployment['tcpListener']}:18765"],
                    "environment": {
                        "TAILTERM_TCP_LISTEN": "0.0.0.0:18765",
                        "TAILTERM_STATE": "/state",
                        "TAILTERM_TOKEN_FILE": "/run/hub-token",
                        "TAILTERM_MAX_AGENTS": "32",
                    },
                    "volumes": [
                        f"{binary_destination}:/opt/tailterm-hub:ro",
                        f"{deployment['stateDirectory']}:/state",
                        f"{deployment['tokenPath']}:/run/hub-token:ro",
                    ],
                    "mem_limit": "512m",
                    "cpus": "1.0",
                }
            }
        }
        request: dict[str, Any] = {"custom_compose_config": compose}
        if arguments.update:
            middleware_command = "midclt call -j app.update tailterm-hub "
        else:
            request.update(app_name=APP_NAME, custom_app=True)
            middleware_command = "midclt call -j app.create "

        stage = "middleware-update"
        _remote(
            plan,
            actual_host,
            middleware_command + shlex.quote(json.dumps(request)),
            stage=stage,
            last_completed_stage=last_completed_stage,
            preflight=preflight,
        )
        last_completed_stage = stage
        stage = "middleware-start"
        _remote(
            plan,
            actual_host,
            "midclt call -j app.start tailterm-hub",
            stage=stage,
            last_completed_stage=last_completed_stage,
            preflight=preflight,
        )
        last_completed_stage = stage
    except PreflightFailure as error:
        _emit(error.result(plan))
        return 2

    _emit(
        {
            "version": 1,
            "status": "deployed",
            "phase": "complete",
            "classification": "success",
            "requestId": plan["requestId"],
            "targetHost": plan["targetHost"],
            "actualHost": actual_host,
            "targetExecutable": plan["targetExecutable"],
            "route": plan["route"],
            "sourceDatabase": plan["sourceDatabase"],
            "backupDestination": preflight["backupDestination"],
            "backupSha256": preflight["sha256"],
            "preflightReceiptSha256": receipt_sha256,
            "binaryDestination": deployment["binaryDestination"],
            "appName": APP_NAME,
            "tcpListener": TCP_LISTENER,
            "mutationStarted": True,
        }
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
