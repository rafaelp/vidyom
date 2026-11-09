---
title: "Workflow and plugin sandboxing"
sidebar_position: 1
---

# Workflow and plugin security

Workflows and plugins represent executable instructions and must be treated as untrusted until validated and authorized.

## Required protections

- Apply schema validation and explicit allow-lists for available nodes.
- Do not enable arbitrary shell execution by default.
- Build FFmpeg commands using argument arrays with validated file references, not shell string interpolation.
- Restrict plugin filesystem/network/system access to declared capabilities and execution identities.
- Run out-of-process plugins as separate processes that speak JSON-RPC 2.0 over stdin/stdout, so the daemon controls what they can reach. Remote plugins and services use JSON-RPC over authenticated HTTPS. Native Go `.so` plugins are excluded; WASM sandboxing is not supported yet.
- Give plugins scoped secret references only when explicitly authorized; avoid exposing raw vault or keyring APIs.
- Keep tenants isolated: a plugin acts within one tenant at a time and cannot reach another tenant's database, storage prefix, secrets or cache.
- Reject dangerous external URLs and prevent SSRF or local file access via remote media imports.
- Enforce CPU, GPU, disk, memory and execution-time limits.
- Verify plugin provenance and integrity, and keep versioned manifests.
- Separate publication, deletion, storage migration and credential management permissions.
- Require an approval gate for public publication unless an administrator explicitly changes trusted policy.
- Process media with local AI by default. `aiProcessing` is `local_only` unless an admin marks a collection or asset as `external_allowed`; Vidyom records who authorized it and when. This applies to transcription, embeddings and LLM calls, and plugins must not send content to external AI services otherwise. Transcripts are private by default, and segments that editors mark as sensitive are never sent to external AI, are excluded from public outputs and are redacted in exports.
- Record security-relevant action, identity, resource, decision and timestamp in durable audit history.

## Defense in depth

Network binding and TLS protect the transport; role/permission checks protect operations; tenant isolation separates data and secrets; worker sandboxing protects the host; the encrypted vault and client keyrings protect credentials at rest; redaction protects telemetry exports. Each layer remains relevant even if other layers are present.

## Approval roles

Roles are assigned per tenant: `viewer`, `editor`, `publisher` and `admin`. Each role is a bundle of granular permissions (`storage.archive.restore`, `asset.delete`, `plugin.install`, `secret.write`, …); an admin can grant an individual permission without changing the role, and the audit records which permission authorized each action. Approval has two levels:

- An `editor` approves the clip selection (`approval.request` kind `clip_selection`).
- A `publisher` (or `admin`) approves publication (kind `publication`).

Changing the visibility or public metadata of an already public video requires a new publisher approval. A workflow can tighten the rule through `approvers`, never loosen it. Withdrawal can be done by a publisher or admin; full deletion is done by an admin.

## Trust levels

`system-trusted` for built-in policy plugins; `operator-trusted` for manually approved extensions; `untrusted` for imported workflows until reviewed. Global security and audit hooks cannot be disabled by workflow-local configuration.
