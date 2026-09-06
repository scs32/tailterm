# Tailterm security operations

The production Pages project is the static browser distribution. It has no
Node gateway, server vault, or application backend. The optional legacy gateway
in this repository is a separate deployment, not a fallback used by Pages.

## Repository and releases

- `.github/workflows/checks.yml` runs unit and real SSH integration tests on pull
  requests and main. Actions are pinned to full commits, the job token has only
  read access, checkout does not retain credentials, and no deployment secrets
  are exposed to pull-request jobs.
- Main should require the `Unit and SSH integration tests` check and a pull
  request, including for administrators, and reject force pushes and deletion.
  This is a single-owner repository: zero additional approving reviewers avoids
  blocking all merges. A pull request provides a reviewable change and check
  record; it does not provide independent human review.
- Build from a clean committed checkout. Run the browser regressions relevant to
  the change and `npm run verify:release`, then use `npm run deploy:static` to publish `dist-static/` to the
  existing `tailterm` Pages project. This command enforces a clean matching build
  and the main branch before invoking Wrangler. Record the source commit, manifest hash,
  and deployment URL. Never deploy the source directory, `.build`, or `data`.
- `release.json` records hashes of the complete deployment, including WASM,
  JavaScript, fonts, speech model parts, headers, and notices. These hashes help
  compare a deployed release with a separately retained trusted build. They are
  not signatures or a claim of reproducibility. Compromised application code
  can replace both its own checks and a manifest served beside it.

## Account controls requiring account-owner configuration

The 2026-09-06 inspection found no rulesets or branch protection before this
work. GitHub's API did not report the account's MFA status; that is **unknown**,
not evidence that MFA is disabled. Verify MFA/passkeys and recovery methods in
GitHub and Cloudflare account settings. Keep recovery codes outside this vault.

Wrangler currently authenticates with a broad interactive OAuth grant. Do not
revoke it without checking other projects that depend on it. For a dedicated
deployment credential, create a Cloudflare API token with **Account → Cloudflare
Pages → Edit**, scoped to the intended account, and use it only in the trusted
deployment environment. This is an account-level permission, not a claim of
single-project isolation. Do not put that token in source, a browser build, or a
pull-request workflow. Retire the old grant only after confirming other uses
and testing the replacement. No replacement token has been provisioned by this
repository change.

Sources: [Cloudflare Pages API](https://developers.cloudflare.com/pages/configuration/api/),
[GitHub branch protection](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches).

## Tailnet and SSH access review

The local device inventory contained an online Tailterm browser identity and
older offline Tailterm/Tailserve browser identities. An untagged node or an
offline node alone does not establish excessive access. The actual tailnet
policy, remote SSH configurations, and intended destination/user list have not
been verified. No devices, grants, or SSH permissions were revoked.

For the owner to finish the review:

1. List the destinations, SSH ports, and actual OS usernames that this browser
   should use. Include subnet-routed destinations explicitly if needed.
2. Identify the current browser node in Tailscale's admin console. Restrict its
   network access to those destinations and SSH ports. Review **all** matching
   grants and legacy ACLs: permissions are additive, so a narrow new rule does
   not override an existing broad rule. Do not replace the whole tailnet policy
   with an isolated example and inadvertently disconnect unrelated machines.
3. For Tailscale SSH, additionally restrict the SSH policy's destinations and OS
   users. For ordinary SSH over Tailscale, the target's sshd configuration and
   authorized keys control login; Tailscale SSH policy does not replace them.
4. Prefer named non-root remote users and only the sudo rights needed for the
   actual work. Retain a tested separate administrative login before changing
   permissions. Test allowed connections and a deliberately disallowed target
   from the browser identity, not merely from the Mac's Tailscale identity.
5. Review old browser identities and revoke only those confirmed unused.

Source: [Tailscale grants](https://tailscale.com/docs/reference/syntax/grants).

## Speech asset provenance

The model is `onnx-community/whisper-tiny.en` at revision
`3a6d57ee9c665610614068e8592d8baee0188181`, converted from OpenAI Whisper.
`client/speech-model-manifest.json` pins every packaged source file and part.
The model's upstream license is shipped as `licenses/whisper.txt`.
Transformers.js and ONNX runtime are pinned by the npm lockfile; their notices
are also shipped. Updating the model requires reviewing the source revision
and updating the manifest together; never regenerate hashes automatically to
accept a download that failed verification.

Source: [pinned model](https://huggingface.co/onnx-community/whisper-tiny.en/tree/3a6d57ee9c665610614068e8592d8baee0188181),
[Whisper license](https://github.com/openai/whisper/blob/main/LICENSE).

## If someone deploys the optional Node gateway

This applies only to the separate legacy mode. Standard SSH originates from
the Node host, so access to that gateway can expose destinations reachable by
that host. Use a dedicated single-owner container/VM and restrict outbound
traffic to intended SSH destinations. An SSH TCP dial alone does not prove
that arbitrary HTTP metadata can be fetched.

Its synchronous password KDF can stall the event loop; concurrent attempts
need bounded asynchronous/worker execution if exposed to less-trusted users.
Behind a reverse proxy, identify the client only through explicitly trusted
proxies, not arbitrary `X-Forwarded-For` headers. Unique exclusively created
temporary vault files and ownership checks address writable-directory races;
fsync addresses crash durability. These server-side changes are not part of
this browser-deployment change.
