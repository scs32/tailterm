# Agents library implementation report

This candidate implements feature `wi_a8f6e4e08f3a1cef` revision 2 under bounded work-order message `tsk_2cfcff70a0fbe967#1720`, from baseline `4e8e6c5b38905b25b5173d0044560be45e5b6e88`. It is an implementation candidate only: integration, deployment, production profile migration, release acceptance, and lifecycle closure need later orders.

## Data contract

- Plaintext vault/profile data uses Agents catalog version 2 and Teams schema version 2. A catalog contains at most 1,000 stable definitions; up to 30 teams each contain 1–32 references. Definitions retain their own revision and all launch fields. Team members contain only the definition ID plus optional alias and role overrides.
- Legacy embedded team members migrate one-for-one to separate definitions before legacy filtering. There is no name/content deduplication. Migration is strict and atomic: duplicate/invalid/overflow/future-version content rejects the operation, explicit `teams: []` remains empty, and the original v1 encrypted local record remains available through the authenticated recovery endpoint.
- Agent edits use definition-revision comparison. Referenced definitions and machines used by unresolved plans cannot be deleted. Example customization atomically adds copied definitions and a referenced team.

## Compatibility and encryption

- New local data is encrypted as a v2 AES-GCM envelope in `encrypted-v2`. A single IndexedDB transaction writes that record and the v2 selection pointer. The old `encrypted` record is retained; once the pointer exists, updated code never silently falls back to or merges old-namespace writes.
- Vault v1 and v2 keep PBKDF2-SHA256/600,000 and use versioned AAD. Profile v1 and v2 keep the original PBKDF2/HKDF salts/info, so profile authentication tokens and encryption-key derivation do not change. Profile envelopes use versioned AAD.
- The hub profile service advertises service version 2 and envelope versions 1/2. A valid revision CAS is checked before its atomic monotonic version fence: a stored v2 profile cannot be overwritten by v1, and rejected writes do not alter the current record or history.
- An updated client blocks profile sync to a service that cannot advertise the v2 contract. Old clients cannot edit v2 data. Rollback means using the retained legacy snapshot with old software; it does not mean downgrading or merging the current v2 profile.

## Launch snapshots and retries

- Team references resolve to copied launch fields before project/agent effects. Copies retain definition ID/revision provenance, exact work-item/order/context identity, independent reasoning/permission fields, and preallocated agent identity.
- The local encrypted retry journal is non-portable and capped at eight plans, 512 KiB per plan, and 2 MiB total. Shared work-item context is stored once per plan; server credentials are not stored. Capacity is checked before project/agent effects.
- A member becomes `uncertain` before its host request. Retry reconciles the exact preallocated agent ID and resulting run before proceeding. It never treats an unknown response as unstarted. A host-verified pre-registration failure can return to `unstarted`.
- Only an `unstarted` member can receive a deliberate folder amendment. Its machine and all definition-derived settings remain frozen, and old/new folder provenance is retained. Started and uncertain identities remain unchanged. Unresolved add-team and new-project plans can be restored after reload for the same encrypted credential/profile scope.

## Runtime controls

Reasoning, Codex approval policy, Codex sandbox, Claude permission mode, and Claude allowed-tool rules are independent fields. Inherit emits no corresponding runtime override. Unsupported explicit combinations are rejected in the UI/shared command builder and again in `tt spawn` before hub/provider effects.

The advertised Codex reasoning matrix is an exact allowlist for `gpt-5.3-codex`, `gpt-6-astra`, `gpt-5.6-sol`, `gpt-5.6-terra`, and `gpt-5.6-luna`, with `low`, `medium`, `high`, and `xhigh`. Unknown/custom/default models are inherit-only. Primary references were the [Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference) and individual model pages in the [OpenAI model catalog](https://developers.openai.com/api/docs/models). A bounded local `codex --version`/`codex --help` inspection observed `codex-cli 0.153.4` and the supported `-c`, `--sandbox`, and `--ask-for-approval` argv surfaces. No account, authentication, provider, installation, configured-MCP, or generation probe was performed.

## Verification and rollback

Synthetic-only verification covers migration, reference sharing, stale revision/deletion fences, envelope compatibility/tampering, stale old-client namespace writes, interrupted migration, profile sync/conflict/recovery/downgrade, journal bounds/context deduplication, command argv, project-handler propagation, partial launch retry, and realistic long-content layout in Chromium and WebKit. WebKit results are not represented as native Safari results.

Rollback before release is to omit this candidate. After v2 local migration, an old build can use only the retained v1 snapshot; it must not be presented as current or merged. Hub rollback while current v2 profiles exist is unsafe unless the writer remains v2-aware. The v2 pointer and current v2/profile records should not be deleted unless the owner explicitly chooses recovery and accepts losing newer data.

Known verification limitations: the broad `hub/cmd/tt` test package contains a pre-existing coordination-test HTTP connection leak/hang (`TestPostHumanReplyAndLiteralHelp`). Focused changed CLI tests pass, and all other Go packages pass. After an isolated `npm ci` supplied the lockfile dependencies, both ordinary and static production builds completed; npm reported one deprecated dependency, three moderate audit findings, and six install scripts not allowlisted by npm's script policy. No manifest was changed and no audit fix or script approval was run. No deployment, provider execution, live profile/task access, or production migration was performed.
