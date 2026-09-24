#!/usr/bin/env python3
"""Handler-owned verified-host backup gate for a TrueNAS hub release.

Handler usage:
  scripts/truenas_release_preflight.py --plan PLAN.json \
      --receipt-output RECEIPT.json

The controller streams this same file to the plan's exact target-side Python
executable. With no arguments it acts as that remote worker and reads the plan
from stdin. No local runner ever stats, creates, or opens a remote ``/mnt`` path.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import re
import shlex
import socket
import sqlite3
import subprocess
import sys
from datetime import datetime, timezone
from typing import Any


PLAN_VERSION = 1
RECEIPT_VERSION = 1
OPERATION = "sqlite-online-backup"
CHECKS = [
    "source-integrity",
    "source-foreign-keys",
    "backup-integrity",
    "backup-foreign-keys",
    "profile-snapshots",
]
PROFILE_TABLES = ["profile_meta", "profiles", "profile_history"]
REQUEST_ID_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}")
ROUTE_HOST_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._@:-]{0,254}")
HOSTNAME_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9.-]{0,253}")
BACKUP_NAME_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,199}\.sqlite")


class PreflightFailure(Exception):
    """A classified failure safe to serialize for an operator."""

    def __init__(self, classification: str, message: str, **evidence: Any):
        super().__init__(message)
        self.classification = classification
        self.message = message
        self.evidence = evidence

    def result(self, plan: Any = None) -> dict[str, Any]:
        result: dict[str, Any] = {
            "version": RECEIPT_VERSION,
            "status": "failed",
            "phase": self.evidence.pop("phase", "preflight"),
            "classification": self.classification,
            "message": self.message,
        }
        if isinstance(plan, dict):
            plan_fields = {
                "requestId": "requestId",
                "operation": "operation",
                "route": "route",
                "targetHost": "expectedHost",
                "targetExecutable": "targetExecutable",
                "sourceDatabase": "sourceDatabase",
                "backupDestination": "backupDestination",
            }
            for plan_field, result_field in plan_fields.items():
                value = plan.get(plan_field)
                if isinstance(value, (str, dict)):
                    result[result_field] = value
        result.update(self.evidence)
        if "mutationStarted" not in result and "mutationState" not in result:
            result["mutationStarted"] = False
        return result


def _object(value: Any, field: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise PreflightFailure("invalid-input", f"{field} must be an object")
    return value


def _string(value: Any, field: str) -> str:
    if not isinstance(value, str) or not value:
        raise PreflightFailure("invalid-input", f"{field} must be a non-empty string")
    return value


def validate_plan(plan: Any) -> dict[str, Any]:
    """Validate and normalize only; this never touches the filesystem."""

    plan = _object(plan, "plan")
    allowed_fields = {
        "version",
        "requestId",
        "databaseOwner",
        "operation",
        "checks",
        "profileTables",
        "route",
        "targetHost",
        "targetExecutable",
        "sourceDatabase",
        "backupDestination",
        "allowedBackupRoot",
        "backupOwner",
        "deployment",
    }
    unknown_fields = sorted(set(plan) - allowed_fields)
    if unknown_fields:
        raise PreflightFailure(
            "invalid-input", f"unsupported plan fields: {', '.join(unknown_fields)}"
        )
    if type(plan.get("version")) is not int or plan.get("version") != PLAN_VERSION:
        raise PreflightFailure("invalid-input", f"version must be {PLAN_VERSION}")

    request_id = _string(plan.get("requestId"), "requestId")
    if REQUEST_ID_RE.fullmatch(request_id) is None:
        raise PreflightFailure("invalid-input", "requestId has unsupported characters")
    if plan.get("databaseOwner") != "db-handler":
        raise PreflightFailure("invalid-input", "databaseOwner must be db-handler")
    if plan.get("operation") != OPERATION:
        raise PreflightFailure("invalid-input", f"operation must be {OPERATION}")
    if plan.get("checks") != CHECKS:
        raise PreflightFailure("invalid-input", "checks must match the required ordered set")
    if plan.get("profileTables") != PROFILE_TABLES:
        raise PreflightFailure(
            "invalid-input", "profileTables must match the required ordered set"
        )

    route = _object(plan.get("route"), "route")
    route_fields = {"id", "kind", "host", "batchMode", "connectTimeoutSeconds"}
    if set(route) != route_fields:
        raise PreflightFailure("invalid-input", "route fields do not match schema v1")
    route_id = _string(route.get("id"), "route.id")
    if REQUEST_ID_RE.fullmatch(route_id) is None:
        raise PreflightFailure("invalid-input", "route.id has unsupported characters")
    if route.get("kind") != "ssh":
        raise PreflightFailure("invalid-input", "route.kind must be ssh")
    route_host = _string(route.get("host"), "route.host")
    if ROUTE_HOST_RE.fullmatch(route_host) is None:
        raise PreflightFailure("invalid-input", "route.host has unsupported characters")
    if route.get("batchMode") is not True:
        raise PreflightFailure("invalid-input", "route.batchMode must be true")
    connect_timeout = route.get("connectTimeoutSeconds")
    if type(connect_timeout) is not int or not 1 <= connect_timeout <= 60:
        raise PreflightFailure(
            "invalid-input", "route.connectTimeoutSeconds must be an integer from 1 to 60"
        )

    target_host = _string(plan.get("targetHost"), "targetHost")
    if HOSTNAME_RE.fullmatch(target_host) is None:
        raise PreflightFailure("invalid-input", "targetHost has unsupported characters")
    target_executable = pathlib.Path(
        _string(plan.get("targetExecutable"), "targetExecutable")
    )
    source = pathlib.Path(_string(plan.get("sourceDatabase"), "sourceDatabase"))
    destination = pathlib.Path(
        _string(plan.get("backupDestination"), "backupDestination")
    )
    allowed_root = pathlib.Path(
        _string(plan.get("allowedBackupRoot"), "allowedBackupRoot")
    )
    if not all(path.is_absolute() for path in (target_executable, source, destination, allowed_root)):
        raise PreflightFailure(
            "invalid-input",
            "targetExecutable, sourceDatabase, backupDestination, and allowedBackupRoot must be absolute",
        )
    if source == destination or destination.parent != allowed_root:
        raise PreflightFailure(
            "invalid-input", "backupDestination must be a file directly inside allowedBackupRoot"
        )
    if BACKUP_NAME_RE.fullmatch(destination.name) is None:
        raise PreflightFailure(
            "invalid-input", "backupDestination must have a bounded safe .sqlite filename"
        )
    backup_owner = _object(plan.get("backupOwner"), "backupOwner")
    if set(backup_owner) != {"uid", "gid"}:
        raise PreflightFailure("invalid-input", "backupOwner fields do not match schema v1")
    if any(type(backup_owner.get(field)) is not int or backup_owner[field] < 0 for field in ("uid", "gid")):
        raise PreflightFailure("invalid-input", "backupOwner uid/gid must be non-negative integers")

    deployment = _object(plan.get("deployment"), "deployment")
    deployment_fields = {
        "releaseName",
        "binaryDestination",
        "stateDirectory",
        "tokenPath",
        "appName",
        "tcpListener",
    }
    # Optional: a Jev (TypeSafe) API key file mounted read-only into the hub.
    optional_fields = {"typesafeKeyPath"}
    if not deployment_fields <= set(deployment) <= deployment_fields | optional_fields:
        raise PreflightFailure("invalid-input", "deployment fields do not match schema v1")
    normalized_deployment = {
        field: _string(deployment.get(field), f"deployment.{field}")
        for field in sorted(set(deployment))
    }
    key_path = normalized_deployment.get("typesafeKeyPath")
    if key_path is not None and (
        not key_path.startswith("/mnt/deepfreeze/tailterm-hub/")
        or ".." in pathlib.PurePosixPath(key_path).parts
        or any(c.isspace() for c in key_path)
    ):
        raise PreflightFailure(
            "invalid-input", "deployment.typesafeKeyPath must be a plain path under /mnt/deepfreeze/tailterm-hub/"
        )

    normalized = dict(plan)
    normalized.update(
        {
            "version": PLAN_VERSION,
            "requestId": request_id,
            "databaseOwner": "db-handler",
            "operation": OPERATION,
            "checks": list(CHECKS),
            "profileTables": list(PROFILE_TABLES),
            "route": {
                "id": route_id,
                "kind": "ssh",
                "host": route_host,
                "batchMode": True,
                "connectTimeoutSeconds": connect_timeout,
            },
            "targetHost": target_host,
            "targetExecutable": str(target_executable),
            "sourceDatabase": str(source),
            "backupDestination": str(destination),
            "allowedBackupRoot": str(allowed_root),
            "backupOwner": {"uid": backup_owner["uid"], "gid": backup_owner["gid"]},
            "deployment": normalized_deployment,
        }
    )
    return normalized


def plan_digest(plan: dict[str, Any]) -> str:
    return hashlib.sha256(
        json.dumps(plan, sort_keys=True, separators=(",", ":")).encode("utf-8")
    ).hexdigest()


def load_plan(path: pathlib.Path) -> dict[str, Any]:
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise PreflightFailure("invalid-input", f"cannot read plan: {error}") from error
    return validate_plan(raw)


def ssh_argv(plan: dict[str, Any]) -> list[str]:
    route = plan["route"]
    return [
        "ssh",
        "-o",
        "BatchMode=yes",
        "-o",
        f"ConnectTimeout={route['connectTimeoutSeconds']}",
        route["host"],
    ]


def _sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _json_value(value: Any) -> Any:
    if isinstance(value, bytes):
        return {"bytesHex": value.hex()}
    return value


def _profile_evidence(database: sqlite3.Connection) -> dict[str, dict[str, Any]]:
    evidence: dict[str, dict[str, Any]] = {}
    for table in PROFILE_TABLES:
        columns = [
            str(row[1])
            for row in database.execute(f'PRAGMA table_info("{table}")').fetchall()
        ]
        if not columns:
            raise PreflightFailure(
                "verification-failed",
                f"required profile table is missing: {table}",
            )
        quoted = ",".join(f'"{column}"' for column in columns)
        order = ",".join(f'"{column}"' for column in columns)
        rows = [
            {column: _json_value(value) for column, value in zip(columns, row)}
            for row in database.execute(
                f'SELECT {quoted} FROM "{table}" ORDER BY {order}'
            )
        ]
        try:
            payload = json.dumps(
                rows,
                sort_keys=True,
                separators=(",", ":"),
                ensure_ascii=False,
                allow_nan=False,
            ).encode("utf-8")
        except (TypeError, ValueError) as error:
            raise PreflightFailure(
                "verification-failed",
                f"profile table cannot be canonicalized: {table}: {error}",
            ) from error
        evidence[table] = {
            "rows": len(rows),
            "bytes": len(payload),
            "sha256": hashlib.sha256(payload).hexdigest(),
        }
    return evidence


def _database_evidence(database: sqlite3.Connection) -> dict[str, Any]:
    integrity_rows = database.execute("PRAGMA integrity_check").fetchall()
    integrity = "\n".join(str(row[0]) for row in integrity_rows)
    foreign_keys = int(
        database.execute("SELECT count(*) FROM pragma_foreign_key_check").fetchone()[0]
    )
    return {
        "integrity": integrity,
        "foreignKeyViolations": foreign_keys,
        "profiles": _profile_evidence(database),
    }


def _open_read_only(path: pathlib.Path) -> sqlite3.Connection:
    database = sqlite3.connect(path.as_uri() + "?mode=ro", uri=True)
    database.execute("PRAGMA query_only=ON")
    return database


def _write_exclusive(path: pathlib.Path, data: bytes, mode: int = 0o600) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    with os.fdopen(descriptor, "wb") as output:
        output.write(data)
        output.flush()
        os.fsync(output.fileno())


def _publish_no_clobber(source: pathlib.Path, destination: pathlib.Path) -> None:
    try:
        os.link(source, destination)
    except FileExistsError as error:
        raise PreflightFailure(
            "destination-collision",
            "destination appeared before no-clobber publication",
            backupDestination=str(destination),
        ) from error
    source.unlink()


def _receipt_path(destination: pathlib.Path) -> pathlib.Path:
    return pathlib.Path(str(destination) + ".preflight.json")


def _identity(plan: dict[str, Any], actual_host: str, executable: pathlib.Path) -> dict[str, Any]:
    return {
        "version": RECEIPT_VERSION,
        "requestId": plan["requestId"],
        "planDigest": plan_digest(plan),
        "databaseOwner": plan["databaseOwner"],
        "operation": plan["operation"],
        "checks": plan["checks"],
        "profileTables": plan["profileTables"],
        "targetHost": plan["targetHost"],
        "actualHost": actual_host,
        "targetExecutable": plan["targetExecutable"],
        "resolvedExecutable": str(executable),
        "route": plan["route"],
        "sourceDatabase": plan["sourceDatabase"],
        "backupDestination": plan["backupDestination"],
        "allowedBackupRoot": plan["allowedBackupRoot"],
        "backupOwner": plan["backupOwner"],
        "deployment": plan["deployment"],
    }


def _receipt_evidence_digest(receipt: dict[str, Any]) -> str:
    fields = (
        "version",
        "requestId",
        "planDigest",
        "databaseOwner",
        "operation",
        "checks",
        "profileTables",
        "targetHost",
        "actualHost",
        "targetExecutable",
        "resolvedExecutable",
        "route",
        "sourceDatabase",
        "backupDestination",
        "allowedBackupRoot",
        "backupOwner",
        "deployment",
        "sourceEvidence",
        "backupEvidence",
        "size",
        "mode",
        "uid",
        "gid",
        "sha256",
        "receiptPath",
        "completedAt",
    )
    evidence = {field: receipt.get(field) for field in fields}
    return hashlib.sha256(
        json.dumps(evidence, sort_keys=True, separators=(",", ":")).encode("utf-8")
    ).hexdigest()


def _read_json(path: pathlib.Path, classification: str) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise PreflightFailure(classification, f"cannot read {path}: {error}") from error
    if not isinstance(value, dict):
        raise PreflightFailure(classification, f"{path} does not contain an object")
    return value


def _validate_binding(binding: dict[str, Any], identity: dict[str, Any]) -> None:
    for field in (
        "version",
        "requestId",
        "planDigest",
        "targetHost",
        "targetExecutable",
        "route",
        "sourceDatabase",
        "backupDestination",
        "allowedBackupRoot",
        "backupOwner",
        "deployment",
    ):
        if binding.get(field) != identity.get(field):
            raise PreflightFailure(
                "request-conflict",
                "requestId is already bound to different canonical inputs",
                requestId=identity["requestId"],
                mutationStarted=False,
            )


def _recover_receipt(identity: dict[str, Any]) -> dict[str, Any] | None:
    destination = pathlib.Path(identity["backupDestination"])
    receipt_path = _receipt_path(destination)
    if not destination.exists() and not receipt_path.exists():
        return None
    if not destination.is_file() or not receipt_path.is_file():
        raise PreflightFailure(
            "destination-collision",
            "backup or receipt exists without a complete matching pair",
            backupDestination=str(destination),
            receiptPath=str(receipt_path),
            mutationStarted=False,
        )
    receipt = _read_json(receipt_path, "destination-collision")
    for field, value in identity.items():
        if receipt.get(field) != value:
            raise PreflightFailure(
                "destination-collision",
                "existing backup belongs to a different preflight identity",
                backupDestination=str(destination),
                receiptPath=str(receipt_path),
                mutationStarted=False,
            )
    if receipt.get("sha256") != _sha256(destination):
        raise PreflightFailure(
            "verification-failed",
            "existing backup hash does not match its receipt",
            backupDestination=str(destination),
            mutationStarted=False,
        )
    if receipt.get("evidenceDigest") != _receipt_evidence_digest(receipt):
        raise PreflightFailure(
            "verification-failed",
            "existing receipt evidence digest does not match",
            backupDestination=str(destination),
            mutationStarted=False,
        )
    database = _open_read_only(destination)
    try:
        backup_evidence = _database_evidence(database)
    finally:
        database.close()
    if backup_evidence != receipt.get("backupEvidence"):
        raise PreflightFailure(
            "verification-failed",
            "existing backup evidence does not match its receipt",
            backupDestination=str(destination),
            mutationStarted=False,
        )
    replay = dict(receipt)
    replay.update(
        {
            "status": "already-satisfied",
            "phase": "complete",
            "classification": "already-satisfied",
            "replay": True,
            "mutationStarted": False,
            "mutationPreviouslyCompleted": True,
        }
    )
    return replay


def execute(plan: Any) -> dict[str, Any]:
    """Run the remote handler backup, preserving the causal mutation boundary."""

    normalized = validate_plan(plan)
    actual_host = socket.gethostname()
    if not actual_host:
        raise PreflightFailure(
            "host-verification-failed",
            "execution host identity is empty",
            expectedHost=normalized["targetHost"],
            route=normalized["route"],
            mutationStarted=False,
        )
    if actual_host != normalized["targetHost"]:
        raise PreflightFailure(
            "host-mismatch",
            "actual execution host does not match the required target host",
            expectedHost=normalized["targetHost"],
            observedHost=actual_host,
            route=normalized["route"],
            mutationStarted=False,
        )

    expected_executable = pathlib.Path(normalized["targetExecutable"])
    try:
        resolved_executable = pathlib.Path(sys.executable).resolve(strict=True)
        required_executable = expected_executable.resolve(strict=True)
    except OSError as error:
        raise PreflightFailure(
            "executable-unavailable",
            f"target executable is unavailable: {error}",
            targetExecutable=str(expected_executable),
            mutationStarted=False,
        ) from error
    if (
        required_executable != expected_executable
        or resolved_executable != required_executable
        or not required_executable.is_file()
    ):
        raise PreflightFailure(
            "executable-mismatch",
            "running executable does not match targetExecutable",
            targetExecutable=str(expected_executable),
            resolvedExecutable=str(resolved_executable),
            mutationStarted=False,
        )

    source = pathlib.Path(normalized["sourceDatabase"])
    allowed_root = pathlib.Path(normalized["allowedBackupRoot"])
    destination = pathlib.Path(normalized["backupDestination"])
    try:
        resolved_source = source.resolve(strict=True)
    except OSError as error:
        raise PreflightFailure(
            "source-unavailable",
            f"source database is unavailable: {error}",
            sourceDatabase=str(source),
            mutationStarted=False,
        ) from error
    if resolved_source != source or not resolved_source.is_file():
        raise PreflightFailure(
            "source-mismatch",
            "source database is not the required canonical regular-file path",
            sourceDatabase=str(source),
            resolvedSourceDatabase=str(resolved_source),
            mutationStarted=False,
        )
    try:
        resolved_root = allowed_root.resolve(strict=True)
    except OSError as error:
        raise PreflightFailure(
            "destination-unavailable",
            f"allowed backup root is unavailable: {error}",
            allowedBackupRoot=str(allowed_root),
            mutationStarted=False,
        ) from error
    if resolved_root != allowed_root or not resolved_root.is_dir():
        raise PreflightFailure(
            "destination-mismatch",
            "allowed backup root is not the required canonical directory",
            allowedBackupRoot=str(allowed_root),
            resolvedBackupRoot=str(resolved_root),
            mutationStarted=False,
        )

    try:
        source_database = _open_read_only(resolved_source)
        try:
            source_evidence = _database_evidence(source_database)
        except PreflightFailure as error:
            source_database.close()
            error.evidence.setdefault("mutationStarted", False)
            raise
        except sqlite3.Error as error:
            raise PreflightFailure(
                "verification-failed",
                f"source verification failed: {error}",
                sourceDatabase=str(resolved_source),
                mutationStarted=False,
            ) from error
    except sqlite3.Error as error:
        raise PreflightFailure(
            "source-unavailable",
            f"source database cannot be opened read-only: {error}",
            sourceDatabase=str(resolved_source),
            mutationStarted=False,
        ) from error
    if source_evidence["integrity"] != "ok" or source_evidence["foreignKeyViolations"] != 0:
        source_database.close()
        raise PreflightFailure(
            "integrity-failed",
            "source database failed integrity or foreign-key verification",
            sourceDatabase=str(resolved_source),
            sourceEvidence=source_evidence,
            mutationStarted=False,
        )

    identity = _identity(normalized, actual_host, resolved_executable)
    request_directory = allowed_root / ".tailterm-preflight-requests"
    request_binding = request_directory / f"{normalized['requestId']}.json"
    request_lock = request_directory / f"{normalized['requestId']}.lock"
    destination_lock = pathlib.Path(str(destination) + ".preflight.lock")
    receipt_path = _receipt_path(destination)
    temporary = destination.with_name(
        f".{destination.name}.{normalized['requestId']}.partial"
    )
    receipt_temporary = receipt_path.with_name(
        f".{receipt_path.name}.{normalized['requestId']}.partial"
    )
    binding_temporary = request_directory / f".{normalized['requestId']}.partial"
    mutation_started = False
    request_lock_owned = False
    destination_lock_owned = False
    temporary_owned = False
    receipt_temporary_owned = False
    binding_temporary_owned = False

    try:
        mutation_started = True
        request_directory.mkdir(mode=0o700, exist_ok=True)
        if (
            request_directory.resolve(strict=True) != request_directory
            or not request_directory.is_dir()
        ):
            raise PreflightFailure(
                "destination-mismatch",
                "request binding directory is not canonical",
                requestDirectory=str(request_directory),
            )
        try:
            _write_exclusive(request_lock, (normalized["requestId"] + "\n").encode())
        except FileExistsError as error:
            raise PreflightFailure(
                "request-in-progress",
                "another execution owns this requestId",
                requestId=normalized["requestId"],
            ) from error
        request_lock_owned = True

        if request_binding.exists():
            binding = _read_json(request_binding, "request-conflict")
            _validate_binding(binding, identity)
        else:
            binding = dict(identity)
            binding.update({"status": "pending", "createdAt": datetime.now(timezone.utc).isoformat()})
            _write_exclusive(
                request_binding,
                (json.dumps(binding, sort_keys=True, separators=(",", ":")) + "\n").encode(),
            )

        recovered = _recover_receipt(identity)
        if recovered is not None:
            return recovered

        try:
            _write_exclusive(destination_lock, (normalized["requestId"] + "\n").encode())
        except FileExistsError as error:
            raise PreflightFailure(
                "destination-collision",
                "another execution owns this backup destination",
                backupDestination=str(destination),
            ) from error
        destination_lock_owned = True

        if destination.exists() or receipt_path.exists():
            raise PreflightFailure(
                "destination-collision",
                "backup destination or receipt already exists",
                backupDestination=str(destination),
                receiptPath=str(receipt_path),
            )
        if temporary.exists() or receipt_temporary.exists():
            raise PreflightFailure(
                "destination-collision",
                "a prior partial artifact requires inspection",
                backupDestination=str(destination),
            )
        _write_exclusive(temporary, b"")
        temporary_owned = True

        backup_database = sqlite3.connect(str(temporary))
        try:
            source_database.backup(backup_database)
        except sqlite3.Error as error:
            raise PreflightFailure(
                "backup-failed",
                f"SQLite online backup failed: {error}",
                backupDestination=str(destination),
            ) from error
        finally:
            backup_database.close()

        backup_database = _open_read_only(temporary)
        try:
            backup_evidence = _database_evidence(backup_database)
        finally:
            backup_database.close()
        if (
            backup_evidence["integrity"] != "ok"
            or backup_evidence["foreignKeyViolations"] != 0
            or backup_evidence["profiles"] != source_evidence["profiles"]
        ):
            raise PreflightFailure(
                "integrity-failed",
                "backup failed integrity, foreign-key, or profile equivalence",
                sourceEvidence=source_evidence,
                backupEvidence=backup_evidence,
            )

        temporary_stat = temporary.stat()
        if (
            temporary_stat.st_uid != normalized["backupOwner"]["uid"]
            or temporary_stat.st_gid != normalized["backupOwner"]["gid"]
        ):
            raise PreflightFailure(
                "verification-failed",
                "backup owner does not match backupOwner",
                expectedOwner=normalized["backupOwner"],
                observedOwner={"uid": temporary_stat.st_uid, "gid": temporary_stat.st_gid},
            )

        _publish_no_clobber(temporary, destination)
        temporary_owned = False
        destination_stat = destination.stat()
        receipt = dict(identity)
        receipt.update(
            {
                "status": "success",
                "phase": "complete",
                "classification": "success",
                "replay": False,
                "mutationStarted": True,
                "sourceEvidence": source_evidence,
                "backupEvidence": backup_evidence,
                "size": destination_stat.st_size,
                "mode": oct(destination_stat.st_mode & 0o777),
                "uid": destination_stat.st_uid,
                "gid": destination_stat.st_gid,
                "sha256": _sha256(destination),
                "receiptPath": str(receipt_path),
                "completedAt": datetime.now(timezone.utc).isoformat(),
            }
        )
        receipt["evidenceDigest"] = _receipt_evidence_digest(receipt)
        receipt_bytes = (
            json.dumps(receipt, sort_keys=True, separators=(",", ":")) + "\n"
        ).encode()
        _write_exclusive(receipt_temporary, receipt_bytes)
        receipt_temporary_owned = True
        _publish_no_clobber(receipt_temporary, receipt_path)
        receipt_temporary_owned = False

        completed_binding = dict(identity)
        completed_binding.update(
            {
                "status": "complete",
                "receiptPath": str(receipt_path),
                "receiptSha256": hashlib.sha256(receipt_bytes).hexdigest(),
                "completedAt": receipt["completedAt"],
            }
        )
        _write_exclusive(
            binding_temporary,
            (
                json.dumps(completed_binding, sort_keys=True, separators=(",", ":"))
                + "\n"
            ).encode(),
        )
        binding_temporary_owned = True
        os.replace(binding_temporary, request_binding)
        binding_temporary_owned = False
        return receipt
    except PreflightFailure as error:
        error.evidence.setdefault("mutationStarted", mutation_started)
        raise
    except (OSError, sqlite3.Error) as error:
        raise PreflightFailure(
            "backup-failed",
            f"backup procedure failed: {error}",
            backupDestination=str(destination),
            mutationStarted=mutation_started,
        ) from error
    finally:
        source_database.close()
        for owned, path in (
            (temporary_owned, temporary),
            (receipt_temporary_owned, receipt_temporary),
            (binding_temporary_owned, binding_temporary),
            (destination_lock_owned, destination_lock),
            (request_lock_owned, request_lock),
        ):
            if owned:
                try:
                    path.unlink()
                except FileNotFoundError:
                    pass


def _shell_result(result: dict[str, Any]) -> str:
    return shlex.quote(json.dumps(result, sort_keys=True, separators=(",", ":")))


def _remote_command(plan: dict[str, Any], worker_source: str) -> str:
    host_failed = PreflightFailure(
        "host-verification-failed",
        "target hostname probe failed",
        expectedHost=plan["targetHost"],
        route=plan["route"],
        mutationStarted=False,
    ).result(plan)
    host_mismatch_prefix = {
        "version": RECEIPT_VERSION,
        "status": "failed",
        "phase": "preflight",
        "classification": "host-mismatch",
        "message": "actual execution host does not match the required target host",
        "requestId": plan["requestId"],
        "expectedHost": plan["targetHost"],
        "route": plan["route"],
        "mutationStarted": False,
    }
    executable_failed = PreflightFailure(
        "executable-unavailable",
        "targetExecutable is not executable on the verified host",
        expectedHost=plan["targetHost"],
        targetExecutable=plan["targetExecutable"],
        route=plan["route"],
        mutationStarted=False,
    ).result(plan)
    executable = shlex.quote(plan["targetExecutable"])
    worker = shlex.quote(worker_source)
    expected_host = shlex.quote(plan["targetHost"])
    return (
        "actual=$(hostname 2>/dev/null) || { printf '%s\\n' "
        + _shell_result(host_failed)
        + "; exit 70; }; "
        "if [ -z \"$actual\" ]; then printf '%s\\n' "
        + _shell_result(host_failed)
        + "; exit 70; fi; "
        f"if [ \"$actual\" != {expected_host} ]; then "
        "printf "
        + shlex.quote(
            json.dumps(host_mismatch_prefix, sort_keys=True, separators=(",", ":"))[:-1]
            + ',"observedHost":"%s"}\n'
        )
        + ' "$actual"; exit 71; fi; '
        f"if [ ! -x {executable} ]; then printf '%s\\n' "
        + _shell_result(executable_failed)
        + "; exit 72; fi; "
        f"exec {executable} -c {worker}"
    )


def _parse_structured(stdout: bytes) -> dict[str, Any] | None:
    for line in reversed(stdout.decode("utf-8", "replace").splitlines()):
        try:
            candidate = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(candidate, dict) and candidate.get("version") == RECEIPT_VERSION:
            return candidate
    return None


def invoke_remote(plan: Any, worker_path: pathlib.Path) -> dict[str, Any]:
    """Execute the backup worker only through the declared SSH route."""

    normalized = validate_plan(plan)
    worker_source = worker_path.read_text(encoding="utf-8")
    try:
        completed = subprocess.run(
            ssh_argv(normalized) + [_remote_command(normalized, worker_source)],
            input=(json.dumps(normalized, separators=(",", ":")) + "\n").encode(),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except OSError as error:
        return PreflightFailure(
            "route-unavailable",
            f"could not start the declared SSH route: {error}",
            route=normalized["route"],
            mutationStarted=False,
        ).result(normalized)
    result = _parse_structured(completed.stdout)
    if result is not None:
        return result
    detail = completed.stderr.decode("utf-8", "replace").strip().splitlines()
    if completed.returncode == 255:
        failure = PreflightFailure(
            "route-unavailable",
            "declared SSH route did not reach a verified preflight result",
            route=normalized["route"],
            routeExitCode=completed.returncode,
            routeDetail=detail[-1][:500] if detail else "",
            mutationStarted=False,
        )
    else:
        failure = PreflightFailure(
            "preflight-result-missing",
            "remote command returned no structured preflight result",
            route=normalized["route"],
            remoteExitCode=completed.returncode,
            remoteDetail=detail[-1][:500] if detail else "",
            mutationState="unknown",
        )
    return failure.result(normalized)


def probe_remote_identity(plan: Any) -> dict[str, Any]:
    """Verify only host/executable for a non-database deployment caller."""

    normalized = validate_plan(plan)
    probe_source = (
        "import json,os,socket,sys; "
        "print(json.dumps({'version':1,'status':'verified','phase':'identity',"
        "'classification':'verified-host','actualHost':socket.gethostname(),"
        "'resolvedExecutable':os.path.realpath(sys.executable),"
        "'mutationStarted':False},sort_keys=True,separators=(',',':')))"
    )
    try:
        completed = subprocess.run(
            ssh_argv(normalized) + [_remote_command(normalized, probe_source)],
            input=b"",
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except OSError as error:
        return PreflightFailure(
            "route-unavailable",
            f"could not start the declared SSH route: {error}",
            route=normalized["route"],
            mutationStarted=False,
        ).result(normalized)
    result = _parse_structured(completed.stdout)
    if result is not None:
        return result
    detail = completed.stderr.decode("utf-8", "replace").strip().splitlines()
    classification = "route-unavailable" if completed.returncode == 255 else "verification-failed"
    return PreflightFailure(
        classification,
        "remote identity probe returned no structured result",
        route=normalized["route"],
        remoteExitCode=completed.returncode,
        remoteDetail=detail[-1][:500] if detail else "",
        mutationStarted=False,
    ).result(normalized)


def validate_receipt(plan: Any, receipt: Any) -> dict[str, Any]:
    """Validate an immutable handler result without touching its database paths."""

    normalized = validate_plan(plan)
    receipt = _object(receipt, "receipt")
    if receipt.get("status") not in {"success", "already-satisfied"}:
        raise PreflightFailure("verification-failed", "handler receipt is not successful")
    expected = {
        "version": RECEIPT_VERSION,
        "requestId": normalized["requestId"],
        "planDigest": plan_digest(normalized),
        "databaseOwner": normalized["databaseOwner"],
        "operation": normalized["operation"],
        "checks": normalized["checks"],
        "profileTables": normalized["profileTables"],
        "targetHost": normalized["targetHost"],
        "targetExecutable": normalized["targetExecutable"],
        "route": normalized["route"],
        "sourceDatabase": normalized["sourceDatabase"],
        "backupDestination": normalized["backupDestination"],
        "allowedBackupRoot": normalized["allowedBackupRoot"],
        "backupOwner": normalized["backupOwner"],
        "deployment": normalized["deployment"],
    }
    for field, value in expected.items():
        if receipt.get(field) != value:
            raise PreflightFailure(
                "verification-failed", f"handler receipt does not match plan field {field}"
            )
    source_evidence = receipt.get("sourceEvidence", {})
    backup_evidence = receipt.get("backupEvidence", {})
    source_profiles = source_evidence.get("profiles")
    backup_profiles = backup_evidence.get("profiles")
    profile_evidence_valid = isinstance(source_profiles, dict) and all(
        type(value.get("rows")) is int
        and value["rows"] >= 0
        and type(value.get("bytes")) is int
        and value["bytes"] >= 2
        and re.fullmatch(r"[0-9a-f]{64}", str(value.get("sha256", ""))) is not None
        for value in source_profiles.values()
        if isinstance(value, dict)
    )
    if isinstance(source_profiles, dict):
        profile_evidence_valid = profile_evidence_valid and len(source_profiles) == sum(
            isinstance(value, dict) for value in source_profiles.values()
        )
    success_state_valid = (
        receipt.get("status") == "success"
        and receipt.get("classification") == "success"
        and receipt.get("replay") is False
        and receipt.get("mutationStarted") is True
    ) or (
        receipt.get("status") == "already-satisfied"
        and receipt.get("classification") == "already-satisfied"
        and receipt.get("replay") is True
        and receipt.get("mutationStarted") is False
        and receipt.get("mutationPreviouslyCompleted") is True
    )
    resolved_executable = receipt.get("resolvedExecutable")
    if (
        receipt.get("phase") != "complete"
        or not success_state_valid
        or receipt.get("actualHost") != normalized["targetHost"]
        or not isinstance(resolved_executable, str)
        or not pathlib.PurePosixPath(resolved_executable).is_absolute()
        or resolved_executable != normalized["targetExecutable"]
        or source_evidence.get("integrity") != "ok"
        or source_evidence.get("foreignKeyViolations") != 0
        or backup_evidence.get("integrity") != "ok"
        or backup_evidence.get("foreignKeyViolations") != 0
        or source_profiles != backup_profiles
        or set(source_profiles or {}) != set(PROFILE_TABLES)
        or not profile_evidence_valid
        or receipt.get("mode") != "0o600"
        or type(receipt.get("uid")) is not int
        or type(receipt.get("gid")) is not int
        or receipt.get("uid") != normalized["backupOwner"]["uid"]
        or receipt.get("gid") != normalized["backupOwner"]["gid"]
        or type(receipt.get("size")) is not int
        or receipt.get("size") <= 0
        or not re.fullmatch(r"[0-9a-f]{64}", str(receipt.get("sha256", "")))
        or receipt.get("evidenceDigest") != _receipt_evidence_digest(receipt)
        or receipt.get("receiptPath")
        != str(_receipt_path(pathlib.Path(normalized["backupDestination"])))
        or not isinstance(receipt.get("completedAt"), str)
        or not receipt.get("completedAt")
    ):
        raise PreflightFailure(
            "verification-failed", "handler receipt lacks valid backup/profile evidence"
        )
    return dict(receipt)


def _save_local_receipt(
    path: pathlib.Path, result: dict[str, Any], plan: dict[str, Any]
) -> str:
    data = (json.dumps(result, sort_keys=True, separators=(",", ":")) + "\n").encode()
    if path.exists():
        try:
            existing = json.loads(path.read_bytes())
            validate_receipt(plan, existing)
        except (json.JSONDecodeError, PreflightFailure) as error:
            raise PreflightFailure(
                "request-conflict", "receipt output contains a different result"
            ) from error
        if (
            existing.get("requestId") != result.get("requestId")
            or existing.get("planDigest") != result.get("planDigest")
            or existing.get("sha256") != result.get("sha256")
        ):
            raise PreflightFailure(
                "request-conflict", "receipt output contains a different result"
            )
        return hashlib.sha256(path.read_bytes()).hexdigest()
    path.parent.mkdir(parents=True, exist_ok=True)
    _write_exclusive(path, data)
    return hashlib.sha256(data).hexdigest()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--plan", type=pathlib.Path)
    parser.add_argument("--receipt-output", type=pathlib.Path)
    arguments = parser.parse_args(argv)

    plan: Any = None
    try:
        if arguments.plan is None:
            if arguments.receipt_output is not None:
                raise PreflightFailure(
                    "invalid-input", "--receipt-output requires --plan"
                )
            plan = json.load(sys.stdin)
            result = execute(plan)
        else:
            plan = load_plan(arguments.plan)
            result = invoke_remote(plan, pathlib.Path(__file__).resolve())
            if arguments.receipt_output is not None and result.get("status") in {
                "success",
                "already-satisfied",
            }:
                local_receipt_sha256 = _save_local_receipt(
                    arguments.receipt_output, result, plan
                )
                result["receiptOutput"] = {
                    "path": str(arguments.receipt_output),
                    "sha256": local_receipt_sha256,
                }
    except PreflightFailure as error:
        result = error.result(plan)
    except (OSError, json.JSONDecodeError) as error:
        result = PreflightFailure("invalid-input", f"cannot read input: {error}").result(plan)
    print(json.dumps(result, sort_keys=True, separators=(",", ":")), flush=True)
    return 0 if result.get("status") in {"success", "already-satisfied"} else 2


if __name__ == "__main__":
    raise SystemExit(main())
