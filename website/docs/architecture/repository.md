---
title: "Repository structure"
sidebar_position: 1
---

# Repository layout

## Project files

The [repository](https://github.com/rafaelp/vidyom) also holds the product specification and the documentation site:

```text
vidyom/
├── .github/                    # CI, Dependabot, issue and pull request templates
├── spec/                       # Complete product specification (v1.2 current; v1.1 and v1.0 for history)
├── website/                    # This Docusaurus documentation site
│   ├── docs/                   # Documentation pages
│   └── examples/               # Example workflows, provider bindings and media profiles
├── CODE_OF_CONDUCT.md
├── CONTRIBUTING.md
├── LICENSE                     # Apache License 2.0
├── NOTICE
├── README.md
└── SECURITY.md
```

## Runtime layout

The runtime code follows this structure, alongside `website/`.

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
├── spec/                       # Original product specification
└── website/                    # Docusaurus documentation
```

**Design rule:** the engine owns workflow execution, nodes implement actions, connectors implement vendor adapters, hooks intercept lifecycle points, events notify asynchronous consumers, and plugins package extensions. Keep these concerns independent and version their contracts.
