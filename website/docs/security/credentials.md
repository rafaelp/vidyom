---
title: "Credentials and secrets vault"
sidebar_position: 1
---

# Credentials, keyrings and the secrets vault

**Decided:** Client machines keep credentials in the operating system keyring. On the server, secrets live in an encrypted vault inside each tenant database. Workflow files, command-line arguments, logs, job payloads and repository configuration must **never contain raw secret values**.

## Client keyrings

| Host | Backend |
| --- | --- |
| macOS | Keychain |
| Windows | Credential Manager |
| Linux desktop | Secret Service (e.g. GNOME Keyring / KWallet) |

The CLI uses the keyring for its own credentials, such as the per-device credential issued by [client pairing](./networking.md), and for secrets with `client` location.

## Server vault

**Decided:** The daemon does not depend on a server-side OS keyring, which removes the headless Linux keyring problem. Each tenant has its own encrypted vault in its tenant database, using envelope encryption:

- One data key per tenant encrypts that tenant's secrets.
- The data key is stored wrapped by a master key; wrapped tenant data keys live in the global system database.
- The master key is unsealed at boot through TPM/systemd-creds or a cloud KMS, with no manual passphrase.

The daemon must **fail closed**: if the master key is missing or sealed, it refuses to start or to serve secrets, and it never stores plain-text secrets as a fallback. **Validate:** TPM/systemd-creds and KMS unsealing on the target hosts.

## Location modes

- `client`: secret remains in the CLI machine keyring. This supports local operations or online interaction while the client stays connected.
- `server`: secret is held in the tenant vault on the daemon host; **preferred for autonomous background jobs**.
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

Secrets are scoped to the active tenant, and one tenant can never read another tenant's vault. Input secret values through safe interactive prompts; remote secret provisioning requires explicit permission and end-to-end encrypted transport. List commands must show metadata, not secret values. Rotation should support testing a replacement credential before disabling the former.
