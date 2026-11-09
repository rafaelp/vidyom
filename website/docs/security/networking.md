---
title: "Encrypted transport and private networking"
sidebar_position: 1
---

# Encrypted transport and private networking

**Decided:** CLI-to-daemon communication must be encrypted and authenticated when remote. Operators must be able to bind the server only to a Tailscale, NetBird or comparable private-network interface, with no public listener.

## Transports

| Mode | Scope | Security properties |
| --- | --- | --- |
| Unix domain socket | Same host | OS filesystem/peer-credential permissions; not encrypted by itself |
| SSH tunnel | Remote | SSH encryption, key or certificate authentication |
| HTTPS / mTLS | Remote API | TLS 1.3, server identity verification, optional mutual client certificate auth |
| Tailscale / NetBird | Private network | WireGuard-encrypted network access; still apply application authorization |

## Fail closed

If a configured private interface is unavailable, **do not fall back to `0.0.0.0`**. Startup of the affected listener should fail and the daemon should remain safe. Alternative deployment: bind daemon on `127.0.0.1` and expose through a private-network reverse proxy such as Tailscale Serve.

## Proposed client profile

```json
{
  "defaultServer": "production",
  "servers": {
    "local": {"transport": "unix", "socket": "/run/user/1000/vidyomd.sock"},
    "production": {
      "transport": "ssh",
      "host": "media.example.internal",
      "user": "operator",
      "remoteSocket": "/run/user/1000/vidyomd.sock"
    },
    "cloud": {
      "transport": "https",
      "url": "https://media.example.internal",
      "authRef": "vidyom-cloud-client"
    }
  }
}
```

Authentication confirms the caller's identity; authorization decides which workflows, connectors, storage roles, secrets and publication actions are allowed. VPN membership is **not** sufficient to grant administrator access. Use server identity checks, replay protection, limited token lifetimes and consistent audit records.
