---
name: serve-development
description: Use when an LLM is modifying, testing, building, installing, or running the Serve deployment tool repository.
---

# Serve Development Skill

You are working on **Serve**, a Go deployment tool inspired by Kamal.

## Read first

Before changing behavior, read:

1. `README.md`
2. The package tests for the area you are changing

The current implementation and its behavior tests are the source of truth for intended architecture. Host provisioning remains governed by the manual host prerequisite policy below.

## Current product status

The core deploy loop is implemented: remote deploy over SSH, the long-running agent, event-driven healing, periodic reconciliation, health-gated blue-green cutover through kamal-proxy, health-gated private server aliases, SOPS secret delivery, rollback state, and remote status/logs/events/exec.

Implemented commands:

```sh
serve help
serve --help
serve -h
serve version
serve init [--path serve.yml] [--force]
serve status [--config serve.yml]
serve logs [--host HOST] [--container NAME] [--service SERVICE] [--destination DEST] [--role ROLE]
serve events [--host HOST] [--once]
serve doctor
serve remove [--service SERVICE] [--destination DEST] [--role ROLE] --force
serve prune --force
serve rollback --service SERVICE --destination DEST [--state-dir .serve/state]
serve secrets edit [--file serve.secrets.yml]
serve agent apply <desired.json> [--state-dir .serve/state] [--socket PATH]
serve agent run [--state-dir DIR] [--socket PATH] [--reconcile-interval 10s]
serve agent reconcile [--socket PATH]
serve agent status [--json] [--socket PATH]
serve agent logs --container NAME [--socket PATH]
serve agent events [--once] [--socket PATH]
serve deploy [--config serve.yml] [--service SERVICE] [--version VERSION]
serve deploy --local [--config serve.yml] [--service SERVICE] [--host localhost] [--version dev] [--state-dir .serve/state]
serve exec [--host HOST] --container NAME -- CMD [ARGS...]
```

`serve setup` is not a product requirement. Do not implement it unless the user explicitly changes the scope.

Do not claim an unimplemented command works. If adding command placeholders, they must return a clear `not implemented yet` message.

## TDD requirement

Follow red-green-refactor.

1. Write a failing behavior test first.
2. Run the targeted test and confirm it fails for the expected reason.
3. Implement the smallest production change.
4. Run the targeted test until it passes.
5. Run the broader suite.

Do not add production behavior without a failing test first.

## Install

From the repository root:

```sh
go install ./cmd/serve
```

Ensure Go's bin directory is on `PATH`:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
```

Verify:

```sh
serve --help
serve version
```

Alternative local build:

```sh
go build -o bin/serve ./cmd/serve
./bin/serve --help
```

Build with a version string:

```sh
go build -ldflags "-X main.version=$(git rev-parse --short HEAD)" -o bin/serve ./cmd/serve
```

## Test

Fast suite:

```sh
go test ./...
```

Make target:

```sh
make test
```

Docker integration suite:

```sh
go test -tags=integration ./...
```

Coverage:

```sh
go test -cover ./...
```

Run integration tests only when Docker is available.

## Run locally

Create or update a starter config:

```sh
serve init --path serve.yml
```

For a local smoke test, use a pullable image:

```sh
cat > serve.yml <<'YAML'
service: demo
image: busybox:1.36
destination: local

servers:
  web:
    hosts:
      - localhost
    command: sleep 3600
    replicas: 1

networking:
  private_network: serve

retain_containers: 5
YAML

docker pull busybox:1.36
serve deploy --local --config serve.yml --host localhost --version dev --state-dir .serve/state
serve status
```

Clean up:

```sh
docker rm -f demo-web-local-dev-r1
```

Apply a desired state JSON directly:

```sh
serve agent apply ./desired.json --state-dir .serve/state
```

Submit desired state to a running agent:

```sh
serve agent apply ./desired.json --socket /run/serve/agent.sock
```

## Private server aliases

Application server roles can claim stable names on their host's private Docker network:

```yaml
service: api
image: ghcr.io/acme/api

servers:
  web:
    hosts: [deploy@app.example.com]
    aliases: [api]
    app_port: 4000
    healthcheck:
      http:
        path: /up
        port: 4000

networking:
  private_network: serve
```

Another container on that host and network can use `http://api:4000`. This supports communication between independently deployed services with different images.

### Alias behavior and safety rules

- `servers.<role>.aliases` and `dependencies.<name>.aliases` are lists of valid identifiers. `accessories` remains a deprecated compatibility alias for dependencies. Network aliases cannot conflict between unrelated workloads on the same host and network.
- Every replica of a server role receives its aliases, allowing Docker DNS to distribute lookups across active replicas.
- The planner includes desired aliases in the container spec hash, but the cutover engine must start candidates without server aliases.
- For aliased roles with health checks, candidates are checked before alias activation and checked again after Docker reconnects their network endpoint.
- Public proxy traffic switches only after alias activation succeeds. Old aliases are removed after proxy cutover and before old containers are retired.
- A failed health check, alias activation, or proxy switch must clean up candidates and preserve the old version's aliases.
- `ReplaceNetworkAliases` must be idempotent and make a best-effort restoration of the old network attachment if replacement fails.
- Docker's container-list response omits custom network aliases. The Docker runtime must inspect listed containers before reporting `ContainerState.Aliases`; do not regress to summary-only alias discovery.
- Aliases are host-local and require callers and receivers to share `networking.private_network`. They do not provide cross-host discovery.
- Direct alias traffic bypasses kamal-proxy's ongoing health routing and TLS termination. Do not describe aliases as equivalent to proxy routing.
- Roles within one normalized service share its `image` and `proxy.app_role`. Use separate entries in `services` when workloads need different images or public routes.

### Tests for alias changes

Cover the vertical path with red-green-refactor:

1. `internal/config`: strict parsing, invalid aliases, and role/dependency conflicts.
2. `internal/planner`: aliases on every intended replica and spec-hash changes.
3. `internal/runtime/fake`: state updates and idempotence.
4. `internal/runtime/docker`: real network alias replacement and aliases returned by `ListContainers` under the `integration` build tag.
5. `internal/agent/cutover`: unhealthy candidates never claim aliases, successful handoff removes old aliases, ownership conflicts preserve the current version, dependent-role health gates, and private-network changes.

See `docs/private-service-aliases.md` for user-facing configuration and limitations.

## Multi-service configuration

A `serve.yml` may use the legacy single-service shape or a `services:` map:

```yaml
destination: production
networking:
  private_network: serve
retain_containers: 5

services:
  api:
    image: ghcr.io/acme/api
    servers:
      web:
        hosts: [deploy@app.example.com]
        command: ./api
        app_port: 3000
    dependencies:
      redis:
        image: redis:7-alpine
        hosts: [deploy@app.example.com]
        aliases: [cache]
    proxy:
      app_role: web
      hosts: [api.example.com]

  worker:
    image: ghcr.io/acme/worker
    servers:
      jobs:
        hosts: [deploy@worker.example.com]
        command: ./worker
```

Multi-service rules:

- `destination`, `networking`, and `retain_containers` are global and cannot be overridden inside a service.
- The `services` map key is the service identity; do not also set a nested `service` field.
- `--service` selects one service. Omitting it deploys all services in deterministic name order.
- Plan every selected service and host before contacting any host. Applies remain sequential and configuration-wide rollback is not implemented.
- Role and dependency `hosts` are inline SSH destinations. Replicas are per host.
- Proxy hostnames must be unique across services, and a name cannot be both a server role and a dependency.
- Keep the planner focused on one normalized service and host. Multi-service expansion and orchestration belong in config and CLI layers.

## Environment and secrets

Applications and dependencies use the same `plain`/`secret` shape, but at different scopes.

### Application example

Top-level `env` applies to every application role:

```yaml
service: api
image: ghcr.io/acme/api

servers:
  web:
    hosts: [deploy@app.example.com]
    command: ./api
    app_port: 3000
  worker:
    hosts: [deploy@app.example.com]
    command: ./worker

env:
  plain:
    APP_ENV: production
    LOG_LEVEL: info
  secret:
    - DATABASE_URL
    - API_TOKEN
```

Both `web` and `worker` receive the four variables above.

### Dependency example

Nested `env` applies only to that named dependency:

```yaml
dependencies:
  postgres:
    image: postgres:16-alpine
    hosts: [deploy@app.example.com]
    aliases: [database]
    internal_port: 5432
    volumes:
      - postgres-data:/var/lib/postgresql/data
    env:
      plain:
        POSTGRES_USER: api
        POSTGRES_DB: api_production
      secret:
        - POSTGRES_PASSWORD
```

The application does not inherit the PostgreSQL variables. Likewise, the dependency does not inherit top-level application variables. To give the same secret to both, list its name in both `env.secret` lists.

The schema is image-agnostic. A second dependency can declare a completely different environment:

```yaml
dependencies:
  rabbitmq:
    image: rabbitmq:4-management-alpine
    hosts: [deploy@app.example.com]
    aliases: [queue]
    internal_port: 5672
    env:
      plain:
        RABBITMQ_DEFAULT_USER: api
        RABBITMQ_DEFAULT_VHOST: api
      secret:
        - RABBITMQ_DEFAULT_PASS
```

### Secrets file example

All referenced names live in the single SOPS-encrypted `serve.secrets.yml` beside `serve.yml`. Run:

```sh
serve secrets edit --file serve.secrets.yml
```

The decrypted editor view may contain:

```yaml
DATABASE_URL: postgres://api:change-me@database:5432/api_production
API_TOKEN: change-me
POSTGRES_PASSWORD: change-me
RABBITMQ_DEFAULT_PASS: change-me
```

SOPS encrypts the file when the editor closes. Hosts receive ciphertext and must have the `sops` binary and decryption credentials.

### Schema and behavior rules

- `env.plain` must be a map of names to non-sensitive string values.
- `env.secret` must be a list of names; secret values never belong in `serve.yml`.
- `env.clear`, a direct `env: {NAME: value}` map, and dependency-level `secrets:` are not supported schemas.
- Top-level `env` applies to app roles; `dependencies.<name>.env` applies only to that dependency.
- `accessories:` is a deprecated compatibility alias for `dependencies:`; configuring both is invalid.
- A missing `serve.secrets.yml` must fail before contacting deployment hosts when any app or dependency declares `env.secret`.
- Desired state carries the encrypted file, secret references, and names, never decrypted values.
- Secret material is written to a private tmpfs env file only while Docker creates the container, then deleted.
- Docker ultimately stores environment values in container metadata, visible to privileged Docker users. File-mounted runtime secrets are not implemented.
- Container spec hashes include only ciphertext for names that container references. Rotating one secret must not restart unrelated app or dependency containers.
- Stateful dependencies still follow application version, deploy, rollback, and retention behavior. Do not claim database-specific upgrade or zero-downtime semantics.

### Tests for environment changes

When changing this behavior, cover the vertical path:

1. `internal/config`: strict YAML parsing, app/dependency scope, and rejected field names.
2. `internal/planner`: plain values, secret names, encrypted file delivery, and per-container spec hashes.
3. `internal/cli`: missing-file failures and remote desired-state payloads.
4. `internal/agent/reconciler`: just-in-time secret resolution, env-file cleanup, and runtime specs.
5. `internal/runtime/docker`: Docker environment/env-file behavior when that boundary changes.

## Host prerequisites

Host provisioning is intentionally out of scope. Docker, the Serve binary, the systemd unit, required directories, SOPS, and credentials must be installed and configured manually before remote deployment. Serve may validate prerequisites, but it must not install or provision them.

## Architecture reminders

- The CLI is the deploy controller.
- The agent is the host orchestrator.
- Docker is the runtime, not the orchestrator.
- Systemd should only start the Serve agent, not individual app containers.
- App/dependency containers are managed through the agent/reconciler.
- Health-gated server aliases are activated by the cutover engine, not exposed when candidate containers are initially created.
- Plaintext secrets are materialized in tmpfs env files only while Docker creates app or dependency containers; never put them on CLI args or in logs.
- Do not add host provisioning or installation behavior to `serve setup`.

## Package map

```txt
cmd/serve                         CLI entrypoint
internal/cli                      CLI command routing and local command implementations
internal/config                   serve.yml parser/validator
internal/planner                  desired-state planner
internal/runtime                  runtime and network-alias interfaces
internal/runtime/fake             in-memory runtime for behavior tests
internal/runtime/docker           Docker Engine and network-alias implementation
internal/agent/state              desired/actual/last-good state store
internal/agent/reconciler         desired-state reconciler
internal/agent/cutover            health-gated blue-green, alias handoff, and retention engine
internal/agent/daemon             long-running agent and Unix socket API
internal/agent/events             structured lifecycle events
internal/agent/healing            restart/healing supervisor
internal/agent/health             health checker interfaces/HTTP checker
internal/agent/proxy              proxy manager interface
internal/agent/proxy/kamalproxy   kamal-proxy provider
internal/agent/secrets            env-file secret delivery
internal/agent/secrets/sops       SOPS-backed secret resolver
packaging/systemd                 manually installed Serve agent unit
```

## Development approach

Prefer vertical, runnable CLI slices over isolated internals. Choose work from the user's request and current package contracts, while keeping host provisioning out of scope.
