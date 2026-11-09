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
- Give plugins scoped secret references only when explicitly authorized; avoid exposing raw keyring APIs.
- Reject dangerous external URLs and prevent SSRF or local file access via remote media imports.
- Enforce CPU, GPU, disk, memory and execution-time limits.
- Verify plugin provenance and integrity, and keep versioned manifests.
- Separate publication, deletion, storage migration and credential management permissions.
- Require an approval gate for public publication unless an administrator explicitly changes trusted policy.
- Record security-relevant action, identity, resource, decision and timestamp in durable audit history.

## Defense in depth

Network binding and mTLS protect the transport; role/permission checks protect operations; worker sandboxing protects the host; keyrings protect credentials at rest; redaction protects telemetry exports. Each layer remains relevant even if other layers are present.

## Trust levels (proposed)

`system-trusted` for built-in policy plugins; `operator-trusted` for manually approved extensions; `untrusted` for imported workflows until reviewed. Global security and audit hooks must not be disabled by workflow-local configuration.
