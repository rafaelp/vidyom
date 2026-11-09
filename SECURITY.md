# Security Policy

Security, auditing and structured logs are core requirements of Vidyom, not add-ons. Thank you for helping keep the project and its users safe.

## Supported versions

Vidyom has not published a release yet. The repository currently contains the documentation website and the design specification. Until the first release, only the latest commit on `main` receives fixes.

| Version | Supported |
| --- | --- |
| `main` | :white_check_mark: |
| Releases | None published yet |

## Reporting a vulnerability

**Do not report security vulnerabilities through public issues, discussions or pull requests.**

Report them privately through GitHub's [private vulnerability reporting](https://github.com/rafaelp/vidyom/security/advisories/new):

1. Open the **Security** tab of this repository.
2. Select **Report a vulnerability**.
3. Fill in the advisory form.

Please include as much of the following as you can:

- The affected component (documentation website, specification, example workflows or configuration, or runtime code once it exists) and the commit or version.
- A description of the issue and its potential impact.
- Steps to reproduce, or a proof of concept.
- Any mitigation or fix you suggest.

## What to expect

- The maintainer will acknowledge your report as soon as possible and keep you informed while it is investigated.
- Once a fix is available, the advisory is published with credit to you, unless you prefer to remain anonymous.
- Please give us reasonable time to release a fix before disclosing the issue publicly.

## Design-level security feedback

Vidyom is at the specification stage. Weaknesses in the *proposed* design that do not affect any running code, such as credential handling, private networking or plugin isolation, can be discussed in a public issue using the **Design proposal** template. If you are unsure whether something is sensitive, report it privately.

## Credentials

Never include real credentials, tokens or private data in issues, pull requests, workflow examples or documentation. If you accidentally publish a secret, revoke and rotate it immediately; removing it from the repository is not enough.
