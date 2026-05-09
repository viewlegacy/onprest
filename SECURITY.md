# Security Policy

Please do not report vulnerabilities through public GitHub issues.

Email security reports to `info@viewlegacy.com` until the project has a dedicated disclosure address.

Please include:

- Affected component
- Affected version or commit
- Steps to reproduce
- Expected impact
- Suggested remediation, if any

## Scope

This policy applies to the open source gateway, agent, CLI, examples, and documentation in this repository.

Managed service infrastructure, hosted environments, customer deployments, and the managed dashboard are outside the scope of this repository unless explicitly stated.

## Design Constraints

- Gateway compromise must not reveal SQL text or database credentials.
- Gateway logs must not include request parameters, SQL text, database credentials, agent error details, API keys, or business data.
- Capabilities must be explicitly allowed per API key.
- Agent-side `capability.yaml` remains the source of truth and the primary security boundary.
- SQL execution, parameter validation, policy enforcement, and result allow-listing are performed on the agent side.
- The gateway only routes capability names and parameters to the authenticated agent and must not construct SQL.
- The public OpenAPI/MCP surface must be filtered by API key capability permissions.
- Real `gateway.env`, `capability.yaml`, API keys, agent private keys, database credentials, customer configuration, and production logs must never be committed.

## Non-Goals

- This repository does not provide security guarantees for arbitrary user-defined SQL.
- This repository does not review or validate the operational security of third-party deployments.
- Self-hosted users are responsible for their own deployment, network controls, log retention, secrets management, monitoring, and backup policies.

## Supported Versions

Security fixes are provided for the latest released version unless otherwise stated.