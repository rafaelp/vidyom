---
title: "Encrypted transport and private networking"
sidebar_position: 1
---

# Encrypted transport and private networking

CLI-to-daemon communication is encrypted and authenticated when remote. The default remote transport is HTTPS with TLS 1.3. Operators can bind the server only to a Tailscale, NetBird or comparable private-network interface, with no public listener.

## Transports

| Mode | Scope | Security properties |
| --- | --- | --- |
| Unix domain socket | Same host | OS filesystem/peer-credential permissions; not encrypted by itself |
| HTTPS | Remote API (default) | TLS 1.3, server identity verification, per-device credentials from client pairing |
| SSH tunnel | Host operators only | SSH encryption, key or certificate authentication; not the default for clients |
| Tailscale / NetBird | Private network | WireGuard-encrypted network access; still apply application authorization |

## Client pairing

A client is enrolled with a one-time pairing code instead of a shared token or SSH account:

1. An admin generates a one-time pairing code.
2. The CLI exchanges the code for a per-device credential.
3. The credential is revocable by an admin and rotated over time. The CLI keeps it in the OS keyring, as described in [credentials](./credentials.md).

A used or expired code is rejected, and a revoked device loses access immediately.

## Fail closed

If a configured private interface is unavailable, **do not fall back to `0.0.0.0`**. Startup of the affected listener fails and the daemon remains safe. Alternative deployment: bind daemon on `127.0.0.1` and expose through a private-network reverse proxy such as Tailscale Serve.

## Client profile

```json
{
  "defaultServer": "production",
  "servers": {
    "local": {"transport": "unix", "socket": "/run/user/1000/vidyomd.sock"},
    "production": {
      "transport": "https",
      "url": "https://media.example.internal",
      "tenant": "example-org",
      "authRef": "vidyom-production-device"
    },
    "host-admin": {
      "transport": "ssh",
      "host": "media.example.internal",
      "user": "operator",
      "remoteSocket": "/run/user/1000/vidyomd.sock"
    }
  }
}
```

Here `authRef` names the per-device credential obtained through pairing and kept in the client keyring. The `ssh` profile is only for host operators.

Authentication confirms the caller's identity; authorization decides which tenants, workflows, connectors, storage roles, secrets and publication actions are allowed. VPN membership is **not** sufficient to grant administrator access. Use server identity checks, replay protection, limited token lifetimes and consistent audit records.
