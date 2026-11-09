---
title: "CLI command reference"
sidebar_position: 1
---

# CLI command reference

:::warning Proposed interface
The following commands describe the planned `vidyom` client. They are **not implemented or installable** yet.
:::

## Daemon and servers

```bash
vidyom daemon status
vidyom daemon health
vidyom server add production --url https://media.example.internal
vidyom server use production
vidyom asset upload ./class.mp4 --server production --workflow ingest-original
```

Remote servers use HTTPS by default. `vidyom server add production --ssh operator@media.example.internal` remains available for host operators.

The daemon process is `vidyomd`. A proposed local launch is `vidyomd start --detach`; production should use a service supervisor.

## Originals and clips

```bash
vidyom original import ./yoga-class.mp4 --type yoga_class --title "Hatha Yoga"
vidyom original list
vidyom original show ORI-001
vidyom original search "breathing techniques"
vidyom clip create --source ORI-001 --prompt "Explain Ujjayi breathing"
vidyom clip list --source ORI-001
vidyom clip preview CLIP-001
vidyom clip approve CLIP-001
vidyom asset lineage CLIP-001
vidyom asset verify CLIP-001
```

## Workflow and job control

```bash
vidyom workflow list
vidyom workflow validate ./workflows/create-clip.json
vidyom workflow run ./workflows/create-clip.json --input source_id=ORI-001
vidyom workflow run ingest-original --dry-run
vidyom workflow resume JOB-001
vidyom node list
vidyom node describe video.cut
vidyom node run ai.transcribe --asset ORI-001
vidyom job list --status running
vidyom job status JOB-001
vidyom job attach JOB-001
vidyom job logs JOB-001 --follow
vidyom job retry JOB-001
vidyom job cancel JOB-001
```

`attach` and `logs --follow` stream updates; disconnecting the CLI must **not cancel** the running job.

A job in the `needs_attention` state stopped before an unsafe node and is never retried automatically. Proposed commands to resolve it:

```bash
vidyom job list --status needs_attention
vidyom job resolve JOB-001 --action retry
vidyom job resolve JOB-001 --action skip
```

## Device pairing, tenants and budgets

```bash
vidyom device pair-code                       # admin: generate a one-time code
vidyom device pair --server production --code <one-time-code>
vidyom device list
vidyom device revoke laptop-01
vidyom tenant list
vidyom tenant use example-org
vidyom budget status
vidyom budget set --monthly 500 --currency USD
```

Roles apply per tenant, and budget commands require the `admin` role. Vidyom alerts at 80% of the budget and blocks new paid operations at 100% unless an admin overrides it.

## Storage and publishing

```bash
vidyom connector list
vidyom storage list
vidyom storage test master
vidyom storage verify archive --asset ORI-001
vidyom storage copy --from master --to replacement --asset ORI-001
vidyom publishing list
vidyom publish CLIP-001 --to youtube-main
vidyom publish CLIP-001 --to youtube-main,vimeo-main
vidyom publish status CLIP-001
vidyom publish sync CLIP-001
```

## Secrets, plugins, and observability

```bash
vidyom secret add gcs-backup --location server
vidyom secret list
vidyom secret test youtube-oauth
vidyom security status
vidyom security audit --since 24h
vidyom security sessions
vidyom observability list
vidyom observability test loki
vidyom observability status
vidyom logs --follow
vidyom doctor --security
vidyom plugin list
vidyom plugin inspect notifications
vidyom plugin install ./notifications-plugin
vidyom hook list --workflow create-clip
vidyom event watch --type clip.created
```

## Automation conventions

Noninteractive commands should support machine-readable JSON output and predictable exit codes. Job submission returns an ID; job status provides structured state and error codes. Never print raw credentials, ephemeral signed URLs or sensitive transcript contents in default output.
