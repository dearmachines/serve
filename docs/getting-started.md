# Getting Started

## Install

Install from this repository:

```sh
git clone https://github.com/uptimenine/serve.git
cd serve
go install ./cmd/serve
```

Make sure your Go bin directory is on `PATH`:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
```

Then verify:

```sh
serve --help
serve version
```

## Agent ownership and local operation

Every Docker operation goes through a running Serve agent. The CLI never connects to Docker: local commands use a Unix socket, and remote commands use SSH to invoke a socket client on the host. Only `serve agent run` starts the Docker-owning agent process.

The default socket is `/run/serve/agent.sock`. Local `status`, `logs`, `events`, `exec`, `doctor`, `remove`, `prune`, `rollback`, `deploy --local`, and `agent apply` accept `--socket PATH`. A missing or inaccessible socket is an error; there is no standalone mode, direct-Docker fallback, or automatic agent startup.

The socket grants root-equivalent control and is owner/group accessible (`0660`). With the root systemd agent, run local commands with appropriate privileges (for example `sudo serve status`) or deliberately configure trusted group access. Never make the socket world-writable. Remote commands retain their existing `sudo` behavior.

The agent owns its state directory, registry credentials, Docker endpoint, and SOPS credentials. `--state-dir` is accepted only by `agent run`, not deploy, apply, or rollback. Do not edit live state files or run multiple agents against the same Docker workloads. A separate state directory does not isolate Docker resources.

### Migrating local commands

Start the agent manually for development (use a separate Docker environment from production). In one terminal:

```sh
serve agent run --socket /tmp/serve-dev.sock --state-dir .serve/state
```

In another terminal:

```sh
serve deploy --local --host localhost --version dev --socket /tmp/serve-dev.sock
serve status --socket /tmp/serve-dev.sock
```

For `deploy --local`, `--host` selects the **manifest identity**, not a connection destination. It defaults to `localhost`; with `hosts: [deploy@app.example.com]`, pass `--host deploy@app.example.com` even while running on that host. Every selected service must have workloads on that identity; use `--service` when needed. `--socket` cannot be combined with remote deployment or the remote `--host`/`status --config` variants.

## Run

Show help:

```sh
serve --help
```

Print the current build version:

```sh
serve version
```

Create a starter config:

```sh
serve init
```

Show local Serve-managed Docker containers:

```sh
serve status
```

Stream logs from a local Serve-managed container:

```sh
serve logs --container demo-web-local-dev-r1
```

Stream one Docker runtime event:

```sh
serve events --once
```

Run basic local checks:

```sh
serve doctor
```

Remove local Serve-managed containers:

```sh
serve remove --service demo --destination local --force
```

Prune stopped local Serve-managed containers:

```sh
serve prune --force
```

Edit SOPS secrets:

```sh
serve secrets edit --file serve.secrets.yml
```

Submit desired-state JSON to the running agent (the same apply API used by local and remote deployments):

```sh
serve agent apply ./desired.json --socket /run/serve/agent.sock
```

Deploy through the local agent:

```sh
serve deploy --local --config serve.yml --host localhost --version dev
```

Run the host agent daemon (normally installed as a systemd unit, see `packaging/systemd/serve-agent.service`):

```sh
serve agent run --state-dir /var/lib/serve/state --socket /run/serve/agent.sock
```

Ask a running agent to reconcile its current persisted desired state:

```sh
serve agent reconcile --socket /run/serve/agent.sock
```

Deploy to the remote hosts in `serve.yml` (streams per-host desired state into a transactional agent apply over SSH):

```sh
serve deploy --config serve.yml --version "$(git rev-parse --short HEAD)"
```

Observe remote hosts through their agents:

```sh
serve status --config serve.yml
serve logs --host app1.example.com --container my-app-web-production-abc123-r1
serve events --host app1.example.com --once
serve exec --host app1.example.com --container my-app-web-production-abc123-r1 -- ls -la
```

Notes:

- `deploy --local` uses the local agent API, with the same health checks, encrypted secret delivery, cutover, and state handling as remote deployment.
- The planned image tag must exist in the agent's Docker runtime or be pullable using the **agent user's** `$DOCKER_CONFIG/config.json` or `$HOME/.docker/config.json`.
- Remote deploy assumes the agent is already installed and running on each host (`serve setup` is not implemented yet).
- Deploys are blue-green: candidates start next to the old version, traffic switches through kamal-proxy only after health passes, old versions are retained per `retain_containers` for rollback.
- A server role can set `aliases` to provide stable, host-local names on `networking.private_network`. Serve health-gates alias activation when the role has a health check. Alias names must be unique across configurations sharing the network, and direct alias traffic bypasses kamal-proxy's ongoing health routing. See [Private service aliases](private-service-aliases.md).
- Applications and dependencies support `env.plain` values and names listed under `env.secret`. When any `env.secret` is configured, deploy embeds the encrypted `serve.secrets.yml` (SOPS ciphertext) in the desired state; the host agent decrypts it just-in-time with the host's credentials (`sops` binary required on hosts).
- Application roles and dependencies support `volumes` entries in `source:/absolute/container/path[:ro|rw]` form. Named volumes survive container removal; absolute bind-mount sources must already exist on the host. Volume contents are not rolled back, and replicas or blue-green versions sharing a volume can access it concurrently.
- `setup` is registered but not implemented yet.

## Rollback, removal, and pruning

`serve rollback --service SERVICE --destination DEST` asks the agent to load and health-check its last-good state. Deployment, rollback, healing, and reconciliation share service/destination operation locks; state publication happens before releasing the lock. Reconciliation reloads state under the lock, so an older snapshot cannot undo a successful deployment.

`serve remove --force` removes matching workloads from the agent's desired state as well as Docker, so healing, reconciliation, and agent restarts do not recreate them. `--role` keeps other roles running. Full service removal clears its deployment/rollback state; partial removal saves a rollback baseline without the removed roles before publishing reduced desired state. Retrying removal also repairs a stale baseline left by an older agent. These are ordered, individually atomic file writes, not a crash-atomic transaction across state files.

Removed public roles are unrouted against the actual proxy, not just the agent's in-memory cache. An already-absent proxy service is success; other proxy/Docker errors remain failures. The agent may start or adopt the proxy to clear routing state restored from its persistent volume, including after an agent restart.

If container cleanup fails after removal intent is persisted, retry removal to finish cleanup. A later explicit deploy can recreate workloads from the manifest.

`serve prune --force` deletes stopped, non-desired managed containers. It preserves running containers and desired containers awaiting healing. Both remove and prune preserve the shared proxy and volume data, and wait for in-flight lifecycle operations before selecting containers. Multi-service removal is sequential, not an all-or-nothing transaction.

If remove or prune finishes deleting containers but cannot refresh actual state, the command still exits nonzero and reports how many containers were deleted. Retry the operation to finish refreshing state; a failed refresh does not undo the deletion. The maintenance lock is host-wide, so unrelated lifecycle operations can wait while maintenance is pending or running.

Rollback emits `rollback_started` immediately to the agent's lifecycle log, followed by `rollback_completed` or `rollback_failed`. The CLI receives the event transcript when the operation finishes, including on failure, and exits nonzero for a failed or incomplete outcome. It does not stream live rollback progress. For API callers, a validated rollback attempt returns JSON with `status` (`completed` or `failed`), `output`, and an optional `error`; HTTP 200 alone is not proof of a successful rollback. Validation and last-good lookup errors still use HTTP error responses.

Logs and exec report a missing container as HTTP 404, ambiguous selection as 400, and Docker selection failures as 500.

`serve exec` sends exact argument lists to the agent and preserves output on command failure. It is non-interactive; stdin/TTY attachment is not implemented.

## Multiple services in one configuration

The legacy top-level `service` and `image` format remains supported. To deploy several independent applications from one file, put them under `services`. The map key becomes each service's identity:

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
        hosts:
          - deploy@app-1.example.com
          - deploy@app-2.example.com
        command: [./api]
        app_port: 3000
        replicas: 2
    dependencies:
      redis:
        image: redis:7-alpine
        hosts:
          - deploy@app-1.example.com
        aliases:
          - cache
    proxy:
      app_role: web
      hosts:
        - api.example.com
      ssl: auto

  worker:
    image: ghcr.io/acme/worker
    servers:
      jobs:
        hosts:
          - deploy@worker.example.com
        command: [./worker]
```

`destination`, `networking`, and `retain_containers` are global in this format and cannot be overridden inside a service. Service-specific images, roles, environment, dependencies, and proxy settings remain nested under the service.

Values under role and dependency `hosts` are SSH destinations or aliases from `~/.ssh/config`. They are defined inline rather than in a separate machine inventory. Replicas are per host: `replicas: 2` on two hosts creates four containers.

Deploy all services or select one:

```sh
serve deploy --config serve.yml --version abc123
serve deploy --config serve.yml --service api --version abc123
```

Serve validates and plans all selected services before contacting a host. Applies then run in deterministic service and host order. A host apply is transactional, but the whole configuration is not: if a later apply fails, previously deployed services and hosts remain on the new version.

`dependencies` is the canonical name for application-owned supporting containers. The legacy `accessories` field is accepted for compatibility, but configuring both fields is an error. Dependencies retain the existing application-coupled deployment, rollback, and retention lifecycle.

## Container commands

Server roles and dependencies configure commands as exact argument lists:

```yaml
dependencies:
  redis:
    image: redis:8-alpine
    command: [redis-server, --save, "", --appendonly, "no"]
```

Serve passes these arguments to Docker without shell parsing and preserves the image entrypoint. Omit `command`, or use `command: []`, to retain the image's default command. Scalar command strings are invalid.

Use an explicit shell argument list when the command needs operators or environment expansion:

```yaml
dependencies:
  catalog-postgres:
    image: postgres:18-alpine
    command:
      - /bin/sh
      - -c
      - |
        export POSTGRES_PASSWORD="$CATALOG_POSTGRES_PASSWORD"
        exec docker-entrypoint.sh postgres
    env:
      secret: [CATALOG_POSTGRES_PASSWORD]
```

Commands are non-secret desired-state configuration and are visible in Docker metadata. Serve does not interpolate them, so reference secret environment variables by name and never include plaintext secret values in `command`.

## Local smoke test

This uses `busybox` so you can verify the local deploy path without building an app image.

```sh
cat > serve.yml <<'YAML'
service: demo
image: busybox:1.36
destination: local

servers:
  web:
    hosts:
      - localhost
    command: [sleep, "3600"]
    replicas: 1

networking:
  private_network: serve

retain_containers: 5
YAML

# First start the development agent in another terminal, as shown above.
serve deploy --local --config serve.yml --host localhost --version dev --socket /tmp/serve-dev.sock
serve status --socket /tmp/serve-dev.sock
```

Expected status output should include a running container similar to:

```txt
SERVICE  DESTINATION  ROLE  VERSION  CONTAINER                  STATUS
demo     local        web   dev      demo-web-local-dev-r1      running
```

Clean up through the same agent before stopping it:

```sh
serve remove --service demo --destination local --force --socket /tmp/serve-dev.sock
```

## Current commands

Implemented:

```sh
serve help
serve --help
serve -h
serve version
serve init [--path serve.yml] [--force]
serve status [--socket PATH]
serve logs [--container NAME] [--service SERVICE] [--destination DEST] [--role ROLE] [--socket PATH]
serve events [--once] [--socket PATH]
serve doctor [--socket PATH]
serve remove [--service SERVICE] [--destination DEST] [--role ROLE] --force [--socket PATH]
serve prune --force [--socket PATH]
serve rollback --service SERVICE --destination DEST [--socket PATH]
serve secrets edit [--file serve.secrets.yml]
serve agent apply <desired.json> [--socket PATH]
serve agent run [--state-dir DIR] [--socket PATH] [--reconcile-interval 10s]
serve agent reconcile [--socket PATH]
serve agent status [--json] [--socket PATH]
serve agent logs --container NAME [--socket PATH]
serve agent events [--once] [--socket PATH]
serve deploy [--config serve.yml] [--service SERVICE] [--version VERSION]
serve deploy --local [--config serve.yml] [--service SERVICE] [--host localhost] [--version dev] [--socket PATH]
serve status --config serve.yml                  # remote, via each host's agent
serve logs --host HOST --container NAME          # remote
serve events --host HOST [--once]                # remote
serve exec [--socket PATH | --host HOST] --container NAME -- CMD [ARGS...]
```

Registered but not fully implemented yet:

```sh
serve setup
```

[Back to the README](../README.md)
