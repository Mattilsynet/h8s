# Security Policy

## Supported Versions

Security updates are provided for the latest release of h8s. Users should
upgrade to the newest available version before reporting a vulnerability.

| Version        | Supported          |
| -------------- | ------------------ |
| Latest release | :white_check_mark: |
| Older releases | :x:                |

## Reporting a Vulnerability

If you discover a security issue in h8s, please report it privately rather
than filing a public issue.

If GitHub private vulnerability reporting is enabled for this repository, use
it first. This opens a private security advisory visible only to repository
maintainers. Otherwise, email [24.7@mattilsynet.no](mailto:24.7@mattilsynet.no),
Mattilsynet's published security contact. The repository is maintained by the
[`@Mattilsynet/applikasjonsplattform`](https://github.com/orgs/Mattilsynet/teams/applikasjonsplattform)
team.

Include:

- a description of the issue and the affected component or command;
- reproduction steps or a minimal proof of concept; and
- any known mitigations.

We will acknowledge receipt within a reasonable time, investigate the report,
and coordinate disclosure once a fix is available. Please do not disclose the
issue publicly until that process is complete.

## Scope

Examples of issues in scope include:

- authentication, authorization, credential handling, and secret exposure;
- HTTP and WebSocket request handling in `h8sd`;
- NATS subject mapping, message routing, and trust boundaries;
- backend proxying in `h8srd` and Kubernetes routing in `k8srd`;
- request smuggling, header handling, origin validation, and denial of service;
- Kubernetes permissions and ingress discovery; and
- vulnerabilities in Go dependencies or published container images.

For vulnerabilities in third-party dependencies with an existing upstream
advisory, please contact the upstream project first. If the vulnerability has
a specific impact on h8s, include those details in a private report to us.
