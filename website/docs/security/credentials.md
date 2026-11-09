---
title: "Credentials and OS keyrings"
sidebar_position: 1
---

# Credentials and OS keyrings

**Decided:** Credentials are stored in the operating system keyring, on either the client or the server. Workflow files, command-line arguments, logs and repository configuration must **never contain raw secret values**.

## Supported backends

| Host | Backend |
| --- | --- |
| macOS | Keychain |
| Windows | Credential Manager |
| Linux desktop | Secret Service (e.g. GNOME Keyring / KWallet) |
| Linux headless | Provisioned Secret Service and secure unattended unlock strategy |

Headless Linux often has no usable unlocked keyring out of the box. The daemon must fail securely, not store plain-text secrets as fallback. An adapter may later integrate system-managed or external secure secret stores, but that is a separate architectural decision.

## Location modes

- `client`: secret remains in the CLI machine keyring. This supports local operations or online interaction while the client stays connected.
- `server`: secret is held in the daemon host keyring; **preferred for autonomous background jobs**.
- `delegated`: explicitly scoped, time-limited credentials delegated over an authenticated secure channel, with expiration, revocation, and auditing.

A disconnected client cannot supply a secret that exists *only* in its local keyring. Long-lived jobs should use server-side service identities or bounded delegated tokens. Never implicitly forward the full client keyring or reusable root credentials.

## Workflow reference

```json
{
  "id": "backup",
  "type": "storage.put",
  "credentials": {"ref": "gcs-backup", "location": "server"},
  "parameters": {"storage": "archive", "source": "${inputs.file}"}
}
```

## Proposed CLI

```bash
vidyom secret add gcs-backup --location server
vidyom secret list
vidyom secret test gcs-backup
vidyom secret delete gcs-backup
```

Input secret values through safe interactive prompts; remote secret provisioning requires explicit permission and end-to-end encrypted transport. List commands must show metadata, not secret values. Rotation should support testing a replacement credential before disabling the former.
