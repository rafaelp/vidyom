---
title: "Repository structure"
sidebar_position: 1
---

# Proposed repository layout

```text
vidyom/
├── cmd/
│   ├── vidyom/                  # Thin CLI client
│   ├── vidyomd/                 # Persistent daemon
│   └── vidyom-worker/           # Processing executable
├── internal/
│   ├── api/
│   ├── auth/
│   ├── daemon/
│   ├── jobs/
│   ├── scheduler/
│   ├── workflow/
│   ├── hooks/
│   ├── events/
│   ├── plugins/
│   ├── security/
│   ├── observability/
│   └── persistence/
├── nodes/
│   ├── storage/
│   ├── video/
│   ├── audio/
│   ├── ai/
│   ├── catalog/
│   └── flow/
├── connectors/
│   ├── storage/
│   ├── publishing/
│   ├── observability/
│   ├── catalog/
│   ├── transcription/
│   └── ai/
├── plugins/
├── schemas/
├── workflows/
│   ├── ingest-original.json
│   ├── ingest-derived.json
│   ├── create-clip.json
│   └── publish-video.json
├── profiles/
├── config/
├── tests/
└── website/                    # Docusaurus documentation
```

**Design rule:** the engine owns workflow execution, nodes implement actions, connectors implement vendor adapters, hooks intercept lifecycle points, events notify asynchronous consumers, and plugins package extensions. Keep these concerns independent and version their contracts.
