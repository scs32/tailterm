#!/usr/bin/env python3
"""Deploy the hub only after validating a handler-owned backup receipt.

Usage:
  scripts/deploy-truenas-hub.py RELEASE --plan PLAN.json \
      --preflight-receipt RECEIPT.json \
      --preflight-receipt-sha256 SHA256 [--update]
  scripts/deploy-truenas-hub.py --rollback-to RELEASE --target hub|bridge \
      --expect-sha256 SHA256

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
    if "typesafeKeyPath" in deployment:
        expected["typesafeKeyPath"] = f"{BASE}/typesafe-key"
    if "discordTokenPath" in deployment:
        expected.update(
            discordTokenPath=f"{BASE}/discord-token",
            bridgeTokenPath=f"{BASE}/bridge-token",
            bridgeBinaryDestination=f"{BASE}/releases/{release}/tailterm-discord",
            bridgeStateDirectory=f"{BASE}/bridge-state",
        )
        for field in ("discordGuildId", "discordApplicationId", "discordOwnerIds", "tailosUrl"):
            expected[field] = deployment[field]
        for field in ("discordHelperTask", "discordHelperChannelId"):
            if field in deployment:
                expected[field] = deployment[field]
    targets = deployment.get("targets", ["hub", "bridge"])
    if (not isinstance(targets, list) or not targets or
            any(not isinstance(t, str) or t not in ("hub", "bridge") for t in targets) or len(targets) != len(set(targets))):
        raise PreflightFailure("invalid-input", "invalid release targets")
    if "targets" in deployment:
        expected["targets"] = targets
    for target, field, binary in (("hub", "binaryDestination", "tailterm-hub"),
                                   ("bridge", "bridgeBinaryDestination", "tailterm-discord")):
        if target not in targets and field in expected:
            retained = deployment.get(field, "")
            if not re.fullmatch(re.escape(BASE) + r"/releases/[A-Za-z0-9._-]+/" + binary, retained):
                raise PreflightFailure("invalid-input", "unchanged target requires retained immutable mount")
            expected[field] = retained
    for field, value in expected.items():
        if deployment.get(field) != value:
            raise PreflightFailure("invalid-input", f"deployment.{field} must be {value}")
    return expected


def _emit(result: dict[str, Any]) -> None:
    print(json.dumps(result, sort_keys=True, separators=(",", ":")), flush=True)


def _read_receipt(path: pathlib.Path, expected_sha256: str) -> tuple[dict[str, Any], str]:
    if re.fullmatch(r"[0-9a-f]{64}", expected_sha256) is None:
        raise PreflightFailure(
            "invalid-input", "preflight receipt SHA-256 must be 64 lowercase hex characters"
        )
    try:
        data = path.read_bytes()
    except OSError as error:
        raise PreflightFailure(
            "verification-failed", f"cannot read preflight receipt: {error}"
        ) from error
    actual_sha256 = hashlib.sha256(data).hexdigest()
    if actual_sha256 != expected_sha256:
        raise PreflightFailure(
            "verification-failed",
            "preflight receipt bytes do not match the handler-saved SHA-256 pin",
            expectedReceiptSha256=expected_sha256,
            observedReceiptSha256=actual_sha256,
            mutationStarted=False,
        )
    try:
        receipt = json.loads(data)
    except json.JSONDecodeError as error:
        raise PreflightFailure(
            "verification-failed", f"cannot parse preflight receipt: {error}"
        ) from error
    if not isinstance(receipt, dict):
        raise PreflightFailure("verification-failed", "preflight receipt is not an object")
    return receipt, actual_sha256


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
            mutationState="completed",
            **common,
        )
    if completed.returncode == 79:
        return PreflightFailure(
            "executable-mismatch",
            "target executable changed after the verified backup receipt",
            deploymentMutationStarted=False,
            deploymentMutationState="not-started",
            mutationState="completed",
            **common,
        )
    if completed.returncode == 255:
        return PreflightFailure(
            "route-unavailable",
            "declared SSH route failed during deployment",
            routeExitCode=completed.returncode,
            routeDetail=detail,
            deploymentMutationState="unknown",
            mutationState="unknown",
            **common,
        )
    return PreflightFailure(
        "remote-operation-failed",
        "verified remote host rejected a deployment operation",
        remoteExitCode=completed.returncode,
        remoteDetail=detail,
        deploymentMutationStarted=True,
        deploymentMutationState="started",
        mutationState="started",
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
            mutationState="completed",
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



def hub_compose(deployment: dict[str, Any], binary_destination: str) -> dict[str, Any]:
    """The tailterm-hub app definition; an optional Jev key is mounted read-only."""
    environment = {
        "TAILTERM_TCP_LISTEN": "0.0.0.0:18765",
        "TAILTERM_STATE": "/state",
        "TAILTERM_TOKEN_FILE": "/run/hub-token",
        "TAILTERM_MAX_AGENTS": "32",
    }
    volumes = [
        f"{binary_destination}:/opt/tailterm-hub:ro",
        f"{deployment['stateDirectory']}:/state",
        f"{deployment['tokenPath']}:/run/hub-token:ro",
    ]
    if deployment.get("typesafeKeyPath"):
        environment["TAILTERM_TYPESAFE_KEY_FILE"] = "/run/typesafe-key"
        volumes.append(f"{deployment['typesafeKeyPath']}:/run/typesafe-key:ro")
    services: dict[str, Any] = {}
    if deployment.get("discordTokenPath"):
        # Broker phase 2b: the hub accepts the route-limited bridge token, and
        # the bridge reaches the hub on the app network, never the tailnet.
        environment["TAILTERM_BRIDGE_TOKEN_FILE"] = "/run/bridge-token"
        volumes.append(f"{deployment['bridgeTokenPath']}:/run/bridge-token:ro")
        services["discord-bridge"] = {
            "image": "gcr.io/distroless/static-debian12:nonroot",
            "user": "950:950",
            "restart": "unless-stopped",
            "entrypoint": ["/opt/tailterm-discord"],
            "read_only": True,
            "cap_drop": ["ALL"],
            "security_opt": ["no-new-privileges:true"],
            "depends_on": ["hub"],
            "environment": {
                "TAILTERM_HUB_URL": "http://hub:18765",
                "TAILTERM_BRIDGE_TOKEN_FILE": "/run/bridge-token",
                "TAILTERM_BRIDGE_STATE": "/state",
                "DISCORD_TOKEN_FILE": "/run/discord-token",
                "DISCORD_GUILD_ID": deployment["discordGuildId"],
                "DISCORD_APPLICATION_ID": deployment["discordApplicationId"],
                "DISCORD_OWNER_IDS": deployment["discordOwnerIds"],
                "TAILOS_URL": deployment["tailosUrl"],
            },
            "volumes": [
                f"{deployment['bridgeBinaryDestination']}:/opt/tailterm-discord:ro",
                f"{deployment['bridgeStateDirectory']}:/state",
                f"{deployment['bridgeTokenPath']}:/run/bridge-token:ro",
                f"{deployment['discordTokenPath']}:/run/discord-token:ro",
            ],
            "mem_limit": "256m",
            "cpus": "0.5",
        }
        # The owner helper conversation (docs/discord-helper-chat.md) stays off
        # unless the plan names its project; absent fields add nothing.
        bridge_environment = services["discord-bridge"]["environment"]
        if "discordHelperTask" in deployment:
            bridge_environment["DISCORD_HELPER_TASK"] = deployment["discordHelperTask"]
        if "discordHelperChannelId" in deployment:
            bridge_environment["DISCORD_HELPER_CHANNEL"] = deployment["discordHelperChannelId"]
    return {
        "services": {
            **services,
            "hub": {
                "image": "gcr.io/distroless/static-debian12:nonroot",
                "user": "950:950",
                "restart": "unless-stopped",
                "entrypoint": ["/opt/tailterm-hub"],
                "read_only": True,
                "cap_drop": ["ALL"],
                "security_opt": ["no-new-privileges:true"],
                "ports": [f"{deployment['tcpListener']}:18765"],
                "environment": environment,
                "volumes": volumes,
                "mem_limit": "512m",
                "cpus": "1.0",
            }
        }
    }

ROLLBACK_MOUNTS = {
    "hub": ("hub", "/opt/tailterm-hub", "tailterm-hub"),
    "bridge": ("discord-bridge", "/opt/tailterm-discord", "tailterm-discord"),
}
DESTINATIONS = {"hub": "binaryDestination", "bridge": "bridgeBinaryDestination"}


def _configured(deployment: dict[str, Any]) -> list[str]:
    return ["hub", "bridge"] if deployment.get("discordTokenPath") else ["hub"]


def release_directories(deployment: dict[str, Any]) -> list[str]:
    """The new release directory of every selected target, once each.

    A bridge-only plan retains the hub's mount, so its own release directory
    comes from the bridge destination, not the hub's.
    """
    selected = deployment.get("targets", ["hub", "bridge"])
    directories: list[str] = []
    for target in _configured(deployment):
        if target in selected:
            directory = str(pathlib.PurePosixPath(deployment[DESTINATIONS[target]]).parent)
            if directory not in directories:
                directories.append(directory)
    return directories


def stale_partner(deployment: dict[str, Any], compose: Any) -> bool:
    """True when a single-target plan's retained partner is not what runs now.

    Such a plan would put an older partner back. A paired plan (or a hub
    without a bridge) has no partner, so nothing can be stale.
    """
    selected = deployment.get("targets", ["hub", "bridge"])
    partners = [t for t in _configured(deployment) if t not in selected]
    for partner in partners:
        service, mount, _ = ROLLBACK_MOUNTS[partner]
        volumes = compose.get("services", {}).get(service, {}).get("volumes") if isinstance(compose, dict) else None
        live = [v for v in volumes or [] if isinstance(v, str) and v.split(":")[1:2] == [mount]]
        if live != [f"{deployment[DESTINATIONS[partner]]}:{mount}:ro"]:
            return True
    return False


def rollback(argv: list[str]) -> int:
    """Point one target back at a retained release binary.

    The retained binary's hash is verified over the established SSH route,
    then only that target's executable mount changes in the live app
    definition; the other target's mount and everything else are kept. No
    backup, token, SQLite or Tailscale change: live database writes remain.
    Remote output is never echoed.
    """
    parser = argparse.ArgumentParser(description=rollback.__doc__)
    parser.add_argument("--rollback-to", required=True)
    parser.add_argument("--target", required=True, choices=sorted(ROLLBACK_MOUNTS))
    parser.add_argument("--expect-sha256", required=True)
    arguments = parser.parse_args(argv)
    release, target = arguments.rollback_to, arguments.target
    if not re.fullmatch(r"[A-Za-z0-9._-]+", release) or not re.fullmatch(r"[0-9a-f]{64}", arguments.expect_sha256):
        _emit(PreflightFailure("invalid-input", "invalid release name or expected hash", phase="rollback").result())
        return 2
    service, mount, binary = ROLLBACK_MOUNTS[target]
    retained = f"{BASE}/releases/{release}/{binary}"
    ssh = ["ssh", "-o", "BatchMode=yes", "-o", f"ConnectTimeout={SSH_ROUTE['connectTimeoutSeconds']}", SSH_ROUTE["host"]]
    stage, mutation = "retained-binary", False

    def remote(command: str) -> bytes:
        try:
            completed = subprocess.run(ssh + [command], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, check=False)
        except OSError as error:
            raise PreflightFailure("route-unavailable", "could not start the declared SSH route", phase="rollback", stage=stage, mutationStarted=mutation) from error
        if completed.returncode != 0:
            classification = "route-unavailable" if completed.returncode == 255 else "remote-operation-failed"
            raise PreflightFailure(classification, "remote rollback step failed", phase="rollback", stage=stage, remoteExitCode=completed.returncode, mutationStarted=mutation)
        return completed.stdout

    try:
        digest = remote(f"sha256sum {shlex.quote(retained)}").split(b" ", 1)[0].decode("ascii", "replace")
        if digest != arguments.expect_sha256:
            raise PreflightFailure("verification-failed", "retained binary does not match the expected hash", phase="rollback", stage=stage, mutationStarted=False)
        stage = "app-config"
        compose = json.loads(remote(f"midclt call app.config {APP_NAME}"))
        volumes = compose.get("services", {}).get(service, {}).get("volumes")
        live = [i for i, v in enumerate(volumes or []) if isinstance(v, str) and v.split(":")[1:2] == [mount]]
        pattern = re.escape(BASE) + r"/releases/[A-Za-z0-9._-]+/" + binary + ":" + re.escape(mount) + ":ro"
        if len(live) != 1 or not re.fullmatch(pattern, volumes[live[0]]):
            raise PreflightFailure("verification-failed", "live app definition has no single retained release mount", phase="rollback", stage=stage, mutationStarted=False)
        volumes[live[0]] = f"{retained}:{mount}:ro"
        stage, mutation = "middleware-update", True
        remote(f"midclt call -j app.update {APP_NAME} " + shlex.quote(json.dumps({"custom_compose_config": compose})))
        stage = "middleware-start"
        remote(f"midclt call -j app.start {APP_NAME}")
    except (PreflightFailure, ValueError, AttributeError, TypeError) as error:
        if not isinstance(error, PreflightFailure):
            error = PreflightFailure("verification-failed", "live app definition is not readable", phase="rollback", stage=stage, mutationStarted=mutation)
        _emit(error.result())
        return 2
    _emit({"version": 1, "status": "rolled-back", "phase": "complete", "target": target, "release": release,
           "binaryDestination": retained, "sha256": arguments.expect_sha256, "appName": APP_NAME, "databaseTouched": False})
    return 0


def main(argv: list[str] | None = None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    if any(a == "--rollback-to" or a.startswith("--rollback-to=") for a in argv):
        return rollback(argv)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("release")
    parser.add_argument("--plan", required=True, type=pathlib.Path)
    parser.add_argument("--preflight-receipt", required=True, type=pathlib.Path)
    parser.add_argument("--preflight-receipt-sha256", required=True)
    parser.add_argument("--update", action="store_true")
    arguments = parser.parse_args(argv)

    if not re.fullmatch(r"[A-Za-z0-9._-]+", arguments.release):
        _emit(PreflightFailure("invalid-input", "invalid release name").result())
        return 2

    plan: dict[str, Any] | None = None
    try:
        plan = load_plan(arguments.plan)
        deployment = _deployment_plan(plan, arguments.release)
        raw_receipt, receipt_sha256 = _read_receipt(
            arguments.preflight_receipt, arguments.preflight_receipt_sha256
        )
        preflight = validate_receipt(plan, raw_receipt)
    except PreflightFailure as error:
        _emit(error.result(plan))
        return 2

    binary = ROOT / ".build" / "ttbin" / "tailterm-hub-linux-amd64"
    try:
        binary_bytes = binary.read_bytes() if "hub" in deployment.get("targets", ["hub", "bridge"]) else b""
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
    if arguments.update and len(_configured(deployment)) > len(deployment.get("targets", ["hub", "bridge"])):
        # A single-target plan names its partner's retained mount; refuse it
        # unless that is the mount live now, before any mutation.
        try:
            live = json.loads(_remote(plan, actual_host, f"midclt call app.config {APP_NAME}",
                                      stage="partner-mount", last_completed_stage=last_completed_stage, preflight=preflight))
            stale = stale_partner(deployment, live)
        except (PreflightFailure, ValueError, AttributeError, TypeError):
            stale = True
        if stale:
            _emit(
                PreflightFailure(
                    "verification-failed",
                    "the plan's retained partner mount is not the live one",
                    phase="deployment",
                    stage="partner-mount",
                    mutationStarted=False,
                    backupAlreadyVerified=True,
                    backupDestination=preflight["backupDestination"],
                    backupSha256=preflight["sha256"],
                ).result(plan)
            )
            return 2
        last_completed_stage = "partner-mount"
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
        _remote(
            plan,
            actual_host,
            "mkdir -p " + " ".join(shlex.quote(d) for d in release_directories(deployment)),
            stage=stage,
            last_completed_stage=last_completed_stage,
            preflight=preflight,
        )
        last_completed_stage = stage

        if "hub" in deployment.get("targets", ["hub", "bridge"]):
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

        if deployment.get("discordTokenPath") and "bridge" in deployment.get("targets", ["hub", "bridge"]):
            bridge_binary = ROOT / ".build" / "ttbin" / "tailterm-discord-linux-amd64"
            try:
                bridge_bytes = bridge_binary.read_bytes()
            except OSError as error:
                raise PreflightFailure(
                    "local-artifact-unavailable",
                    f"cannot read the local bridge artifact: {error}",
                    phase="deployment",
                    stage="bridge-artifact",
                    mutationStarted=True,
                    lastCompletedStage=last_completed_stage,
                    backupAlreadyVerified=True,
                    backupDestination=preflight["backupDestination"],
                    backupSha256=preflight["sha256"],
                ) from error
            stage = "bridge-token"
            bridge_token = shlex.quote(deployment["bridgeTokenPath"])
            _remote(
                plan,
                actual_host,
                f"umask 077; test -s {bridge_token} || cat > {bridge_token}",
                stage=stage,
                last_completed_stage=last_completed_stage,
                preflight=preflight,
                data=(secrets.token_urlsafe(48) + "\n").encode(),
            )
            last_completed_stage = stage
            stage = "bridge-binary-upload"
            quoted_bridge = shlex.quote(deployment["bridgeBinaryDestination"])
            _remote(
                plan,
                actual_host,
                f"cat > {quoted_bridge} && chmod 755 {quoted_bridge}",
                stage=stage,
                last_completed_stage=last_completed_stage,
                preflight=preflight,
                data=bridge_bytes,
            )
            last_completed_stage = stage
            stage = "bridge-state-and-token-check"
            state_dir = shlex.quote(deployment["bridgeStateDirectory"])
            discord_token = shlex.quote(deployment["discordTokenPath"])
            _remote(
                plan,
                actual_host,
                f"mkdir -p {state_dir} && chmod 700 {state_dir} && test \"$(stat -c %u {state_dir})\" = 950"
                f" && test -s {discord_token} && test \"$(stat -c %u {discord_token})\" = 950"
                f" && test \"$(stat -c %u {bridge_token})\" = 950",
                stage=stage,
                last_completed_stage=last_completed_stage,
                preflight=preflight,
            )
            last_completed_stage = stage

        if deployment.get("typesafeKeyPath"):
            # A missing key file would fail the bind mount and stop the hub.
            stage = "typesafe-key-check"
            key = shlex.quote(deployment["typesafeKeyPath"])
            _remote(
                plan,
                actual_host,
                f"test -s {key} && test \"$(stat -c %u {key})\" = 950",
                stage=stage,
                last_completed_stage=last_completed_stage,
                preflight=preflight,
            )
            last_completed_stage = stage

        compose = hub_compose(deployment, binary_destination)
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
            "bridgeBinaryDestination": deployment.get("bridgeBinaryDestination"),
            "appName": APP_NAME,
            "tcpListener": TCP_LISTENER,
            "mutationStarted": True,
            "mutationState": "completed",
        }
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
