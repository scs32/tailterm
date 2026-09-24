import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdtempSync,
  mkdirSync,
  readFileSync,
  realpathSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { hostname, tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const root = resolve(import.meta.dirname, "..");
const worker = join(root, "scripts", "truenas_release_preflight.py");
const deploy = join(root, "scripts", "deploy-truenas-hub.py");
const pythonExecutable = spawnSync(
  "python3",
  ["-c", "import os,sys; print(os.path.realpath(sys.executable))"],
  { encoding: "utf8" },
).stdout.trim();

function workspace(name) {
  return realpathSync(mkdtempSync(join(tmpdir(), `tailterm-${name}-`)));
}

function createDatabase(path) {
  const source = `
import sqlite3, sys
database = sqlite3.connect(sys.argv[1])
database.execute("create table evidence(value text)")
database.execute("insert into evidence values ('preserved')")
database.execute("create table profile_meta(instance_id text primary key, revision integer)")
database.execute("create table profiles(username text primary key, envelope text)")
database.execute("create table profile_history(username text, revision integer, envelope text)")
database.execute("insert into profile_meta values ('synthetic', 2)")
database.execute("insert into profiles values ('fixture', 'ciphertext-only')")
database.execute("insert into profile_history values ('fixture', 1, 'older-ciphertext')")
database.commit()
database.close()
`;
  const result = spawnSync("python3", ["-c", source, path], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
}

function sha256(path) {
  return createHash("sha256").update(readFileSync(path)).digest("hex");
}

function planFor(directory, overrides = {}) {
  const releaseName = "synthetic-release";
  const allowedBackupRoot = join(directory, "backups");
  return {
    version: 1,
    requestId: "synthetic-preflight-1",
    databaseOwner: "db-handler",
    operation: "sqlite-online-backup",
    checks: [
      "source-integrity",
      "source-foreign-keys",
      "backup-integrity",
      "backup-foreign-keys",
      "profile-snapshots",
    ],
    profileTables: ["profile_meta", "profiles", "profile_history"],
    route: {
      id: "truenas-ssh",
      kind: "ssh",
      host: "truenas",
      batchMode: true,
      connectTimeoutSeconds: 10,
    },
    targetHost: hostname(),
    targetExecutable: pythonExecutable,
    sourceDatabase: join(directory, "source.sqlite"),
    backupDestination: join(allowedBackupRoot, "before-release.sqlite"),
    allowedBackupRoot,
    backupOwner: { uid: process.getuid(), gid: process.getgid() },
    deployment: {
      releaseName,
      binaryDestination: `/mnt/deepfreeze/tailterm-hub/releases/${releaseName}/tailterm-hub`,
      stateDirectory: "/mnt/deepfreeze/tailterm-hub/state",
      tokenPath: "/mnt/deepfreeze/tailterm-hub/hub-token",
      appName: "tailterm-hub",
      tcpListener: "100.116.238.37:18765",
    },
    ...overrides,
  };
}

function runWorker(plan) {
  const process = spawnSync("python3", [worker], {
    cwd: root,
    input: `${JSON.stringify(plan)}\n`,
    encoding: "utf8",
  });
  return { process, result: JSON.parse(process.stdout.trim()) };
}

function fakeSsh(directory) {
  const bin = join(directory, "bin");
  mkdirSync(bin, { recursive: true });
  const ssh = join(bin, "ssh");
  const hostnameCommand = join(bin, "hostname");
  writeFileSync(
    ssh,
    "#!/bin/sh\nif [ \"${FAKE_SSH_MODE:-execute}\" = fail ]; then echo 'synthetic route refused' >&2; exit 255; fi\nif [ \"${FAKE_SSH_MODE:-execute}\" = empty ]; then echo 'synthetic remote failure' >&2; exit 7; fi\nfor argument do command=$argument; done\nexec /bin/sh -c \"$command\"\n",
  );
  writeFileSync(
    hostnameCommand,
    "#!/bin/sh\nif [ \"${FAKE_HOSTNAME_MODE:-ok}\" = fail ]; then exit 1; fi\nexec /bin/hostname\n",
  );
  chmodSync(ssh, 0o755);
  chmodSync(hostnameCommand, 0o755);
  return bin;
}

function runHandler(plan, directory, modes = {}) {
  const planPath = join(directory, "plan.json");
  const receiptPath = join(directory, "handler-receipt.json");
  writeFileSync(planPath, JSON.stringify(plan));
  const bin = fakeSsh(directory);
  const process = spawnSync(
    "python3",
    [worker, "--plan", planPath, "--receipt-output", receiptPath],
    {
      cwd: root,
      env: {
        ...globalThis.process.env,
        PATH: `${bin}:${globalThis.process.env.PATH}`,
        FAKE_SSH_MODE: modes.ssh ?? "execute",
        FAKE_HOSTNAME_MODE: modes.hostname ?? "ok",
      },
      encoding: "utf8",
    },
  );
  return {
    process,
    receiptPath,
    result: JSON.parse(process.stdout.trim().split("\n").at(-1)),
  };
}

function validateReceipt(planPath, receiptPath) {
  const source = `
import importlib.util, json, pathlib, sys
spec = importlib.util.spec_from_file_location("preflight", sys.argv[1])
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
try:
    module.validate_receipt(
        module.load_plan(pathlib.Path(sys.argv[2])),
        json.loads(pathlib.Path(sys.argv[3]).read_text()),
    )
except module.PreflightFailure as error:
    print(error.classification)
    raise SystemExit(2)
print("valid")
`;
  return spawnSync("python3", ["-c", source, worker, planPath, receiptPath], {
    encoding: "utf8",
    env: { ...globalThis.process.env, PYTHONDONTWRITEBYTECODE: "1" },
  });
}

function deploymentFailure(plan, directory, actualHost, mode = "execute") {
  const planPath = join(directory, "deployment-plan.json");
  writeFileSync(planPath, JSON.stringify(plan));
  const bin = fakeSsh(directory);
  const source = `
import importlib.util, json, pathlib, sys
scripts = pathlib.Path(sys.argv[1]).parent
sys.path.insert(0, str(scripts))
spec = importlib.util.spec_from_file_location("deploy", sys.argv[1])
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
plan = json.loads(pathlib.Path(sys.argv[2]).read_text())
preflight = {"backupDestination": plan["backupDestination"], "sha256": "a" * 64}
try:
    module._remote(
        plan,
        sys.argv[3],
        "exit 7",
        stage="token",
        last_completed_stage="remote-identity",
        preflight=preflight,
    )
except module.PreflightFailure as error:
    print(json.dumps(error.result(plan), sort_keys=True))
    raise SystemExit(2)
`;
  const observed = spawnSync(
    "python3",
    ["-c", source, deploy, planPath, actualHost],
    {
      encoding: "utf8",
      env: {
        ...globalThis.process.env,
        PATH: `${bin}:${globalThis.process.env.PATH}`,
        FAKE_SSH_MODE: mode,
        PYTHONDONTWRITEBYTECODE: "1",
      },
    },
  );
  return {
    process: observed,
    result: JSON.parse(observed.stdout.trim()),
  };
}

test("wrong execution host fails before filesystem or database mutation", () => {
  const directory = workspace("wrong-host");
  const source = join(directory, "source.sqlite");
  createDatabase(source);
  const sourceHash = sha256(source);
  const allowedBackupRoot = join(directory, "never-created");
  const plan = planFor(directory, {
    targetHost: "definitely-not-this-host.invalid",
    allowedBackupRoot,
    backupDestination: join(allowedBackupRoot, "backup.sqlite"),
  });

  const observed = runWorker(plan);
  assert.equal(observed.process.status, 2);
  assert.equal(observed.result.classification, "host-mismatch");
  assert.equal(observed.result.mutationStarted, false);
  assert.equal(existsSync(allowedBackupRoot), false);
  assert.equal(sha256(source), sourceHash);
});

test("noncanonical source fails before destination creation or SQLite mutation", () => {
  const directory = workspace("wrong-source");
  const source = join(directory, "source.sqlite");
  const alias = join(directory, "source-alias.sqlite");
  createDatabase(source);
  symlinkSync(source, alias);
  const sourceHash = sha256(source);
  const allowedBackupRoot = join(directory, "never-created");
  const plan = planFor(directory, {
    sourceDatabase: alias,
    allowedBackupRoot,
    backupDestination: join(allowedBackupRoot, "backup.sqlite"),
  });

  const observed = runWorker(plan);
  assert.equal(observed.process.status, 2);
  assert.equal(observed.result.classification, "source-mismatch");
  assert.equal(observed.result.mutationStarted, false);
  assert.equal(existsSync(allowedBackupRoot), false);
  assert.equal(sha256(source), sourceHash);
});

test("handler route creates one checked backup and replays exact identity", () => {
  const directory = workspace("verified-route");
  const source = join(directory, "source.sqlite");
  createDatabase(source);
  const sourceHash = sha256(source);
  const plan = planFor(directory);
  mkdirSync(plan.allowedBackupRoot);

  const first = runHandler(plan, directory);
  assert.equal(first.process.status, 0, first.process.stderr);
  assert.equal(first.result.status, "success");
  assert.equal(first.result.classification, "success");
  assert.equal(first.result.mutationStarted, true);
  assert.equal(first.result.route.id, "truenas-ssh");
  assert.equal(first.result.resolvedExecutable, pythonExecutable);
  assert.deepEqual(first.result.sourceEvidence.profiles, first.result.backupEvidence.profiles);
  assert.equal(first.result.backupEvidence.integrity, "ok");
  assert.equal(first.result.backupEvidence.foreignKeyViolations, 0);
  assert.equal(statSync(plan.backupDestination).mode & 0o777, 0o600);
  assert.equal(sha256(source), sourceHash);

  const savedReceipt = readFileSync(first.receiptPath);
  const replay = runHandler(plan, directory);
  assert.equal(replay.process.status, 0, replay.process.stderr);
  assert.equal(replay.result.status, "already-satisfied");
  assert.equal(replay.result.replay, true);
  assert.equal(replay.result.mutationStarted, false);
  assert.equal(replay.result.sha256, first.result.sha256);
  assert.deepEqual(readFileSync(replay.receiptPath), savedReceipt);

  const changedDestination = join(plan.allowedBackupRoot, "changed.sqlite");
  const changed = runWorker({
    ...plan,
    backupDestination: changedDestination,
  });
  assert.equal(changed.process.status, 2);
  assert.equal(changed.result.classification, "request-conflict");
  assert.equal(existsSync(changedDestination), false);
});

test("no-clobber publisher preserves a destination inserted after precheck", () => {
  const directory = workspace("publish-race");
  const partial = join(directory, "partial.sqlite");
  const destination = join(directory, "backup.sqlite");
  writeFileSync(partial, "candidate");
  writeFileSync(destination, "competing");
  const source = `
import importlib.util, pathlib, sys
spec = importlib.util.spec_from_file_location("preflight", sys.argv[1])
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
try:
    module._publish_no_clobber(pathlib.Path(sys.argv[2]), pathlib.Path(sys.argv[3]))
except module.PreflightFailure as error:
    print(error.classification)
    raise SystemExit(2)
`;
  const observed = spawnSync("python3", ["-c", source, worker, partial, destination], {
    encoding: "utf8",
    env: { ...globalThis.process.env, PYTHONDONTWRITEBYTECODE: "1" },
  });
  assert.equal(observed.status, 2);
  assert.equal(observed.stdout.trim(), "destination-collision");
  assert.equal(readFileSync(destination, "utf8"), "competing");
  assert.equal(readFileSync(partial, "utf8"), "candidate");
});

test("receipt consumer rejects incomplete host, source, executable, and mutation evidence", () => {
  const directory = workspace("receipt-validation");
  createDatabase(join(directory, "source.sqlite"));
  const plan = planFor(directory);
  mkdirSync(plan.allowedBackupRoot);
  const completed = runHandler(plan, directory);
  assert.equal(completed.process.status, 0, completed.process.stderr);
  const receipt = JSON.parse(readFileSync(completed.receiptPath, "utf8"));
  const mutations = [
    (value) => (value.sourceEvidence.integrity = "not ok"),
    (value) => (value.actualHost = "another-host.invalid"),
    (value) => (value.resolvedExecutable = "relative-python"),
    (value) => (value.mutationStarted = false),
    (value) => (value.size = 0),
    (value) => (value.sha256 = "f".repeat(64)),
  ];
  for (const [index, mutate] of mutations.entries()) {
    const tampered = structuredClone(receipt);
    mutate(tampered);
    const path = join(directory, `tampered-${index}.json`);
    writeFileSync(path, JSON.stringify(tampered));
    const observed = validateReceipt(join(directory, "plan.json"), path);
    assert.equal(observed.status, 2);
    assert.equal(observed.stdout.trim(), "verification-failed");
  }
});

test("external handler pin rejects changed backup SHA even with recomputed evidence digest", () => {
  const directory = workspace("external-receipt-pin");
  createDatabase(join(directory, "source.sqlite"));
  const plan = planFor(directory);
  mkdirSync(plan.allowedBackupRoot);
  const completed = runHandler(plan, directory);
  assert.equal(completed.process.status, 0, completed.process.stderr);
  const expectedPin = completed.result.receiptOutput.sha256;
  assert.equal(expectedPin, sha256(completed.receiptPath));

  const tamperedPath = join(directory, "tampered-recomputed.json");
  const tamperSource = `
import importlib.util, json, pathlib, sys
spec = importlib.util.spec_from_file_location("preflight", sys.argv[1])
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
receipt = json.loads(pathlib.Path(sys.argv[2]).read_text())
receipt["sha256"] = "f" * 64
receipt["evidenceDigest"] = module._receipt_evidence_digest(receipt)
pathlib.Path(sys.argv[3]).write_text(json.dumps(receipt, sort_keys=True, separators=(",", ":")) + "\\n")
`;
  const tamper = spawnSync(
    "python3",
    ["-c", tamperSource, worker, completed.receiptPath, tamperedPath],
    {
      encoding: "utf8",
      env: { ...globalThis.process.env, PYTHONDONTWRITEBYTECODE: "1" },
    },
  );
  assert.equal(tamper.status, 0, tamper.stderr);

  const checkSource = `
import importlib.util, pathlib, sys
scripts = pathlib.Path(sys.argv[1]).parent
sys.path.insert(0, str(scripts))
spec = importlib.util.spec_from_file_location("deploy", sys.argv[1])
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
try:
    module._read_receipt(pathlib.Path(sys.argv[2]), sys.argv[3])
except module.PreflightFailure as error:
    print(error.classification)
    raise SystemExit(2)
print("valid")
`;
  const accepted = spawnSync(
    "python3",
    ["-c", checkSource, deploy, completed.receiptPath, expectedPin],
    {
      encoding: "utf8",
      env: { ...globalThis.process.env, PYTHONDONTWRITEBYTECODE: "1" },
    },
  );
  assert.equal(accepted.status, 0, accepted.stderr);
  assert.equal(accepted.stdout.trim(), "valid");
  const rejected = spawnSync(
    "python3",
    ["-c", checkSource, deploy, tamperedPath, expectedPin],
    {
      encoding: "utf8",
      env: { ...globalThis.process.env, PYTHONDONTWRITEBYTECODE: "1" },
    },
  );
  assert.equal(rejected.status, 2);
  assert.equal(rejected.stdout.trim(), "verification-failed");
  assert.equal(existsSync(join(directory, "remote-probe-ran")), false);
});

test("route, hostname, and executable failures retain distinct classifications", () => {
  for (const fixture of [
    { name: "route", modes: { ssh: "fail" }, classification: "route-unavailable" },
    {
      name: "hostname",
      modes: { hostname: "fail" },
      classification: "host-verification-failed",
    },
  ]) {
    const directory = workspace(`${fixture.name}-failure`);
    createDatabase(join(directory, "source.sqlite"));
    const plan = planFor(directory);
    const observed = runHandler(plan, directory, fixture.modes);
    assert.equal(observed.process.status, 2);
    assert.equal(observed.result.classification, fixture.classification);
    assert.equal(observed.result.mutationStarted, false);
    assert.equal(existsSync(plan.allowedBackupRoot), false);
  }

  const wrongHostDirectory = workspace("controller-wrong-host");
  createDatabase(join(wrongHostDirectory, "source.sqlite"));
  const wrongHostPlan = planFor(wrongHostDirectory, {
    targetHost: "not-the-synthetic-host.invalid",
  });
  const wrongHost = runHandler(wrongHostPlan, wrongHostDirectory);
  assert.equal(wrongHost.process.status, 2);
  assert.equal(wrongHost.result.classification, "host-mismatch");
  assert.equal(wrongHost.result.observedHost, hostname());
  assert.equal(wrongHost.result.mutationStarted, false);

  const directory = workspace("executable-failure");
  createDatabase(join(directory, "source.sqlite"));
  const plan = planFor(directory, { targetExecutable: "/missing/python3" });
  const observed = runHandler(plan, directory);
  assert.equal(observed.process.status, 2);
  assert.equal(observed.result.classification, "executable-unavailable");
  assert.equal(observed.result.mutationStarted, false);

  const mismatchDirectory = workspace("executable-mismatch");
  createDatabase(join(mismatchDirectory, "source.sqlite"));
  const wrapper = join(mismatchDirectory, "python-wrapper");
  writeFileSync(wrapper, `#!/bin/sh\nexec ${pythonExecutable} \"$@\"\n`);
  chmodSync(wrapper, 0o755);
  const mismatchPlan = planFor(mismatchDirectory, { targetExecutable: wrapper });
  const mismatch = runHandler(mismatchPlan, mismatchDirectory);
  assert.equal(mismatch.process.status, 2);
  assert.equal(mismatch.result.classification, "executable-mismatch");
  assert.equal(mismatch.result.mutationStarted, false);
});

test("deployment requires established route and handler receipt before any remote action", () => {
  const source = readFileSync(deploy, "utf8");
  assert.doesNotMatch(source, /import sqlite3|sqlite3\./);
  assert.match(source, /validate_receipt\(plan, raw_receipt\)/);
  assert.ok(source.indexOf("validate_receipt(plan, raw_receipt)") < source.indexOf("probe_remote_identity(plan)"));

  const directory = workspace("deploy-route");
  const plan = planFor(directory, {
    route: {
      id: "unapproved",
      kind: "ssh",
      host: "some-other-host",
      batchMode: true,
      connectTimeoutSeconds: 10,
    },
  });
  const planPath = join(directory, "plan.json");
  const receiptPath = join(directory, "receipt.json");
  writeFileSync(planPath, JSON.stringify(plan));
  writeFileSync(receiptPath, "{}");
  const observed = spawnSync(
    "python3",
    [
      deploy,
      plan.deployment.releaseName,
      "--plan",
      planPath,
      "--preflight-receipt",
      receiptPath,
      "--preflight-receipt-sha256",
      "0".repeat(64),
    ],
    { cwd: root, encoding: "utf8" },
  );
  const result = JSON.parse(observed.stdout.trim());
  assert.equal(observed.status, 2);
  assert.equal(result.classification, "invalid-input");
  assert.match(result.message, /established truenas SSH route/);
});

test("deployment separates guard, transport-unknown, and started-command mutation states", () => {
  for (const fixture of [
    {
      name: "guard",
      actualHost: "different-host.invalid",
      mode: "execute",
      classification: "host-mismatch",
      state: "not-started",
      mutationState: "completed",
      started: false,
    },
    {
      name: "transport",
      actualHost: hostname(),
      mode: "fail",
      classification: "route-unavailable",
      state: "unknown",
      mutationState: "unknown",
      started: undefined,
    },
    {
      name: "command",
      actualHost: hostname(),
      mode: "execute",
      classification: "remote-operation-failed",
      state: "started",
      mutationState: "started",
      started: true,
    },
  ]) {
    const directory = workspace(`deployment-${fixture.name}`);
    const plan = planFor(directory);
    const observed = deploymentFailure(
      plan,
      directory,
      fixture.actualHost,
      fixture.mode,
    );
    assert.equal(observed.process.status, 2, observed.process.stderr);
    assert.equal(observed.result.classification, fixture.classification);
    assert.equal(observed.result.mutationStarted, true);
    assert.equal(observed.result.backupMutationCompleted, true);
    assert.equal(observed.result.deploymentMutationState, fixture.state);
    assert.equal(observed.result.mutationState, fixture.mutationState);
    assert.equal(observed.result.deploymentMutationStarted, fixture.started);
    assert.equal(observed.result.lastCompletedStage, "remote-identity");
  }
});

test("an optional Jev key is validated and mounted read-only", () => {
  const directory = workspace("typesafe-key");
  const withKey = planFor(directory, {});
  withKey.deployment.typesafeKeyPath = "/mnt/deepfreeze/tailterm-hub/typesafe-key";
  const probe = (plan) =>
    spawnSync(
      "python3",
      [
        "-c",
        [
          "import json, sys, importlib.util as u",
          "sys.path.insert(0, 'scripts')",
          "def load(name, path):",
          "    spec = u.spec_from_file_location(name, path); m = u.module_from_spec(spec); spec.loader.exec_module(m); return m",
          "pre = load('pre', 'scripts/truenas_release_preflight.py')",
          "dep = load('dep', 'scripts/deploy-truenas-hub.py')",
          "plan = json.loads(sys.stdin.read())",
          "try:",
          "    n = pre.validate_plan(plan)",
          "except pre.PreflightFailure as e:",
          "    print(json.dumps({'error': e.message})); sys.exit(0)",
          "print(json.dumps(dep.hub_compose(n['deployment'], n['deployment']['binaryDestination'])))",
        ].join("\n"),
      ],
      { cwd: root, input: JSON.stringify(plan), encoding: "utf8" },
    );
  const compose = JSON.parse(probe(withKey).stdout).services.hub;
  assert.equal(compose.environment.TAILTERM_TYPESAFE_KEY_FILE, "/run/typesafe-key");
  assert.ok(compose.volumes.includes("/mnt/deepfreeze/tailterm-hub/typesafe-key:/run/typesafe-key:ro"));
  const without = JSON.parse(probe(planFor(directory, {})).stdout).services.hub;
  assert.equal(without.environment.TAILTERM_TYPESAFE_KEY_FILE, undefined);
  assert.equal(without.volumes.length, 3);
  for (const bad of ["/etc/passwd", "/mnt/deepfreeze/tailterm-hub/../x", "/mnt/deepfreeze/tailterm-hub/a b"]) {
    const plan = planFor(directory, {});
    plan.deployment.typesafeKeyPath = bad;
    assert.match(JSON.parse(probe(plan).stdout).error, /typesafeKeyPath/);
  }
  const extra = planFor(directory, {});
  extra.deployment.somethingElse = "x";
  assert.match(JSON.parse(probe(extra).stdout).error, /schema v1/);
});

test("the optional Discord bridge is validated all-or-none and runs beside the hub", () => {
  const directory = workspace("discord-bridge");
  const base = "/mnt/deepfreeze/tailterm-hub";
  const bridge = {
    discordTokenPath: `${base}/discord-token`,
    bridgeTokenPath: `${base}/bridge-token`,
    bridgeBinaryDestination: `${base}/releases/r1/tailterm-discord`,
    bridgeStateDirectory: `${base}/bridge-state`,
    discordGuildId: "1552733164781437080",
    discordApplicationId: "1552735643925356666",
    discordOwnerIds: "709503984238592120",
    tailosUrl: "https://tailos.tailarr.com",
  };
  const probe = (plan) =>
    spawnSync(
      "python3",
      [
        "-c",
        [
          "import json, sys, importlib.util as u",
          "sys.path.insert(0, 'scripts')",
          "def load(name, path):",
          "    spec = u.spec_from_file_location(name, path); m = u.module_from_spec(spec); spec.loader.exec_module(m); return m",
          "pre = load('pre', 'scripts/truenas_release_preflight.py')",
          "dep = load('dep', 'scripts/deploy-truenas-hub.py')",
          "plan = json.loads(sys.stdin.read())",
          "try:",
          "    n = pre.validate_plan(plan)",
          "except pre.PreflightFailure as e:",
          "    print(json.dumps({'error': e.message})); sys.exit(0)",
          "print(json.dumps(dep.hub_compose(n['deployment'], n['deployment']['binaryDestination'])))",
        ].join("\n"),
      ],
      { cwd: root, input: JSON.stringify(plan), encoding: "utf8" },
    );
  const withBridge = planFor(directory, {});
  Object.assign(withBridge.deployment, bridge);
  const services = JSON.parse(probe(withBridge).stdout).services;
  assert.equal(services.hub.environment.TAILTERM_BRIDGE_TOKEN_FILE, "/run/bridge-token");
  assert.ok(services.hub.volumes.includes(`${base}/bridge-token:/run/bridge-token:ro`));
  const b = services["discord-bridge"];
  assert.equal(b.environment.TAILTERM_HUB_URL, "http://hub:18765");
  assert.equal(b.environment.DISCORD_GUILD_ID, bridge.discordGuildId);
  assert.equal(b.read_only, true);
  assert.deepEqual(b.cap_drop, ["ALL"]);
  assert.equal(b.ports, undefined, "the bridge publishes no ports");
  assert.ok(b.volumes.includes(`${base}/discord-token:/run/discord-token:ro`));
  assert.ok(b.volumes.includes(`${base}/releases/r1/tailterm-discord:/opt/tailterm-discord:ro`));
  const without = JSON.parse(probe(planFor(directory, {})).stdout).services;
  assert.equal(without["discord-bridge"], undefined);
  assert.equal(without.hub.environment.TAILTERM_BRIDGE_TOKEN_FILE, undefined);
  const partial = planFor(directory, {});
  partial.deployment.discordTokenPath = bridge.discordTokenPath;
  assert.match(JSON.parse(probe(partial).stdout).error, /given together/);
  for (const [field, bad] of [
    ["discordTokenPath", "/etc/discord-token"],
    ["bridgeStateDirectory", `${base}/../state`],
    ["discordGuildId", "12ab"],
    ["discordOwnerIds", "1,2,x"],
    ["tailosUrl", "http://tailos.tailarr.com"],
  ]) {
    const plan = planFor(directory, {});
    Object.assign(plan.deployment, bridge, { [field]: bad });
    assert.match(JSON.parse(probe(plan).stdout).error, new RegExp(field));
  }
});
