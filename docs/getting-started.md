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

Apply a desired-state JSON directly with the local runtime:

```sh
serve agent apply ./desired.json --state-dir .serve/state
```

Submit desired state to a running agent instead (required for remote deployments):

```sh
serve agent apply ./desired.json --socket /run/serve/agent.sock
```

Run the current local deploy path:

```sh
serve deploy --local --config serve.yml --host localhost --version dev --state-dir .serve/state
```

Run the host agent daemon (normally installed as a systemd unit, see `packaging/systemd/serve-agent.service`):

```sh
serve agent run --state-dir /var/lib/serve/state --socket /run/serve/agent.sock
```

Poke a running agent to reconcile desired-state files written to its state dir:

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

- `deploy --local` uses the local Docker daemon.
- The planned image tag must already exist locally or be pullable with credentials from `$DOCKER_CONFIG/config.json` or `$HOME/.docker/config.json`.
- Remote deploy assumes the agent is already installed and running on each host (`serve setup` is not implemented yet).
- Deploys are blue-green: candidates start next to the old version, traffic switches through kamal-proxy only after health passes, old versions are retained per `retain_containers` for rollback.
- A server role can set `aliases` to provide stable, host-local names on `networking.private_network`. Serve health-gates alias activation when the role has a health check. Alias names must be unique across configurations sharing the network, and direct alias traffic bypasses kamal-proxy's ongoing health routing. See [Private service aliases](private-service-aliases.md).
- Applications and dependencies support `env.plain` values and names listed under `env.secret`. When any `env.secret` is configured, deploy embeds the encrypted `serve.secrets.yml` (SOPS ciphertext) in the desired state; the host agent decrypts it just-in-time with the host's credentials (`sops` binary required on hosts).
- `setup` is registered but not implemented yet.

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
        command: ./api
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
        command: ./worker
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

Expected status output should include a running container similar to:

```txt
SERVICE  DESTINATION  ROLE  VERSION  CONTAINER                  STATUS
demo     local        web   dev      demo-web-local-dev-r1      running
```

Clean up manually for now:

```sh
docker rm -f demo-web-local-dev-r1
```

## Current commands

Implemented:

```sh
serve help
serve --help
serve -h
serve version
serve init [--path serve.yml] [--force]
serve status
serve logs [--container NAME] [--service SERVICE] [--destination DEST] [--role ROLE]
serve events [--once]
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
serve status --config serve.yml                  # remote, via each host's agent
serve logs --host HOST --container NAME          # remote
serve events --host HOST [--once]                # remote
serve exec [--host HOST] --container NAME -- CMD [ARGS...]
```

Registered but not fully implemented yet:

```sh
serve setup
```

[Back to the README](../README.md)
