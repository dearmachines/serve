# Serve

Serve deploys Docker applications to your own servers over SSH. It performs health-checked rolling deployments, keeps applications running through a host agent, and routes HTTP and HTTPS traffic through one shared `kamal-proxy` instance per machine.

## Install

Prebuilt releases are available for Linux AMD64. Install the binary on the computer you deploy from and on every deployment host:

```sh
curl -fLO https://github.com/dearmachines/serve/releases/latest/download/serve-linux-amd64
curl -fLO https://github.com/dearmachines/serve/releases/latest/download/SHA256SUMS
sha256sum --check SHA256SUMS
sudo install -m 0755 serve-linux-amd64 /usr/local/bin/serve
serve version
```

For other platforms, [build Serve from source](docs/contributing.md#build-a-local-binary).

## Prepare a server

Each deployment host needs:

- Linux with Docker running
- the Serve binary at `/usr/local/bin/serve`
- the Serve systemd unit
- ports 80 and 443 available for `kamal-proxy`
- SSH access from your deployment computer
- passwordless `sudo` access to `serve` for the deployment user

Install and start the agent once on each host:

```sh
curl -fL https://raw.githubusercontent.com/dearmachines/serve/main/packaging/systemd/serve-agent.service \
  -o serve-agent.service
sudo install -m 0644 serve-agent.service /etc/systemd/system/serve-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now serve-agent
sudo systemctl status serve-agent
```

Allow the SSH deployment user to invoke Serve without a password. For a user named `deploy`, create `/etc/sudoers.d/serve` with `visudo`:

```sudoers
deploy ALL=(root) NOPASSWD: /usr/local/bin/serve
```

The agent creates and manages the machine's shared `kamal-proxy` container when the first routed application is deployed. You do not need to start the proxy separately.

## Deploy an application

Create a directory for the application:

```sh
mkdir my-app
cd my-app
serve init
```

Edit `serve.yml`:

```yaml
service: my-app
image: ghcr.io/acme/my-app
destination: production

servers:
  web:
    hosts:
      - deploy@example.com
    command: [./server]
    app_port: 3000
    replicas: 2
    healthcheck:
      http:
        path: /up
        port: 3000
      interval: 2s
      timeout: 2s
      retries: 10

proxy:
  provider: kamal-proxy
  app_role: web
  hosts:
    - api.example.com
  ssl: auto

networking:
  private_network: serve

retain_containers: 5
```

Each value under `servers.<role>.hosts` or `dependencies.<name>.hosts` is an SSH destination and may include a user, such as `deploy@example.com`, or an alias from `~/.ssh/config`. Hosts are assigned inline; Serve does not use a separate machine inventory. `replicas` is per host, so two replicas on two hosts creates four containers. Point the proxy hostnames at the servers before enabling automatic TLS.

Push the application image, then deploy its tag:

```sh
serve deploy --config serve.yml --version v1.2.3
```

If `image` has no tag, Serve appends the value passed to `--version`. The example above deploys `ghcr.io/acme/my-app:v1.2.3`.

Serve starts the new containers, waits for their health checks, switches proxy traffic, and then retires the previous containers according to `retain_containers`.

## Operate the application

Show containers on every host in `serve.yml`:

```sh
serve status --config serve.yml
```

Stream logs from a container:

```sh
serve logs --host deploy@example.com --container my-app-web-production-v1.2.3-r1
```

Run a command in a container:

```sh
serve exec --host deploy@example.com \
  --container my-app-web-production-v1.2.3-r1 -- env
```

Stream Docker events from a host:

```sh
serve events --host deploy@example.com
```

Run `serve --help` for the complete command list.

## Multiple applications and domains

A `serve.yml` can describe several independently deployable applications. Global destination, networking, and retention settings apply to every service:

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
          - deploy@app.example.com
        command: [./api]
        app_port: 3000
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

`serve deploy` deploys every service in name order. Pass `--service api` to deploy one service. Existing single-service configuration remains supported. Serve validates and plans every selected service before contacting a host, then applies them in deterministic service and host order. Each host apply is transactional, but the whole file is not: a later failure does not roll back services or hosts already deployed.

All applications deployed to the same machine share its Serve agent, Docker network, and central `kamal-proxy` container. Proxy hostnames must be unique between services. One application can still answer on multiple domains by listing several values under its `proxy.hosts`.

## Private service aliases

Server roles can claim stable aliases on the private Docker network. This is useful when separately deployed services on the same machine need to communicate without using public proxy hostnames:

```yaml
service: billing-api
image: ghcr.io/acme/billing-api

servers:
  web:
    hosts:
      - deploy@app.example.com
    aliases:
      - billing-api
    app_port: 3000
    healthcheck:
      http:
        path: /up
        port: 3000

networking:
  private_network: serve
```

Another Serve-managed container on that host and network can connect to `http://billing-api:3000`. Every replica of the role receives the alias, so Docker DNS distributes lookups across active replicas.

Serve activates server aliases only after configured health checks pass. During a deployment, healthy old and candidate versions may briefly share the alias before the old version is detached. Without a health check, Serve considers a started container ready for alias activation.

Aliases are host-local: they do not connect services deployed to different machines. Keep aliases unique across all configurations sharing a Docker network; deployment fails if another running container already owns an alias. Direct alias traffic also bypasses kamal-proxy's ongoing health routing, so configure application-level retries and health checks where appropriate.

## Persistent volumes

Application roles and dependencies can mount Docker named volumes or absolute host paths:

```yaml
servers:
  web:
    hosts:
      - deploy@app.example.com
    volumes:
      - billing-uploads:/app/uploads
      - /srv/billing/config.yml:/app/config.yml:ro

dependencies:
  postgres:
    image: postgres:16-alpine
    hosts:
      - deploy@app.example.com
    volumes:
      - billing-postgres:/var/lib/postgresql/data
```

Each entry uses `source:/absolute/container/path` with an optional `:ro` or `:rw` suffix. An absolute source is a bind mount and must already exist on the deployment host. Any other valid source is treated as a literal Docker named-volume name. Relative bind sources and duplicate container targets are rejected.

Named volumes are local to each Docker host and survive container replacement, `serve remove`, `serve prune`, and rollback. Serve does not delete volume data, and rollback restores container configuration rather than previous volume contents. Use service-qualified volume names to avoid accidental sharing between configurations on the same host.

Replicas and blue-green versions that use the same named volume access the same data concurrently. In particular, versioned dependencies can briefly overlap during deployment, so a volume mount alone does not provide a safe zero-downtime database lifecycle.

## Private images

Authenticate Docker to private registries on every deployment host before deploying. Serve reads the host's standard Docker client configuration, including credential helpers, but does not provision credentials. See [Private registry access](docs/private-registry-access.md).

## Environment variables and secrets

Applications and dependencies use the same environment shape:

```yaml
env:
  plain:
    NAME: value
  secret:
    - SECRET_NAME
```

- `env.plain` is a map of non-sensitive values stored directly in `serve.yml`, Serve's desired state, and Docker's container configuration.
- `env.secret` is a list of names resolved from the SOPS-encrypted `serve.secrets.yml` beside `serve.yml`.
- Top-level `env` applies to every application role. A dependency's nested `env` applies only to that dependency.
- `env.clear` is not supported.

### Application environment

Use top-level `env` for web and worker containers:

```yaml
service: billing
image: ghcr.io/acme/billing

servers:
  web:
    hosts:
      - deploy@app.example.com
    command: [./billing-server]
    app_port: 3000
  worker:
    hosts:
      - deploy@app.example.com
    command: [./billing-worker]

env:
  plain:
    APP_ENV: production
    LOG_LEVEL: info
  secret:
    - DATABASE_URL
    - SECRET_KEY_BASE
```

Both `web` and `worker` receive all four variables. The plain values come from `serve.yml`; the two secret values come from `serve.secrets.yml`.

### Dependency environment

Put `env` inside a dependency when only that supporting container needs the values:

```yaml
dependencies:
  postgres:
    image: postgres:16-alpine
    hosts:
      - deploy@app.example.com
    aliases:
      - database
    internal_port: 5432
    volumes:
      - postgres-data:/var/lib/postgresql/data
    env:
      plain:
        POSTGRES_USER: billing
        POSTGRES_DB: billing_production
      secret:
        - POSTGRES_PASSWORD
```

The application does not receive `POSTGRES_USER`, `POSTGRES_DB`, or `POSTGRES_PASSWORD`. The `database` alias is available on the private Docker network; `internal_port` does not publish PostgreSQL on a host port.

The schema is generic rather than database-specific. For example, RabbitMQ can define its own plain and secret variables:

```yaml
dependencies:
  rabbitmq:
    image: rabbitmq:4-management-alpine
    hosts:
      - deploy@app.example.com
    aliases:
      - queue
    internal_port: 5672
    env:
      plain:
        RABBITMQ_DEFAULT_USER: billing
        RABBITMQ_DEFAULT_VHOST: billing
      secret:
        - RABBITMQ_DEFAULT_PASS
```

Each dependency receives only its own nested environment. The former `accessories:` field is still accepted for compatibility, but new configurations should use `dependencies:`. Configuring both fields is an error.

### Command overrides

Application roles and dependencies accept `command` as an argument list. Serve passes the list directly to Docker as the container command while preserving the image's entrypoint. Omit `command`, or use an empty list, to keep the image's default command. Scalar command strings are not accepted.

Use an argument list for ordinary runtime flags:

```yaml
dependencies:
  redis:
    image: redis:8-alpine
    command:
      - redis-server
      - --bind
      - 0.0.0.0
      - --protected-mode
      - "no"
      - --save
      - ""
      - --appendonly
      - "no"
```

Docker does not interpret shell operators or expand environment variables in an argument list. Invoke a shell explicitly when an adapter needs those features:

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
      secret:
        - CATALOG_POSTGRES_PASSWORD
```

Commands are ordinary non-secret configuration stored in Serve's desired state and Docker metadata. Serve does not interpolate command arguments. Refer to secrets by environment-variable name and let the explicitly configured process expand them inside the container; never put plaintext secret values in `command`.

### Create and edit secrets

Configure SOPS and its decryption credentials on the deployment machine and every host, then open the encrypted file through Serve:

```sh
serve secrets edit --file serve.secrets.yml
```

Inside the editor, add the names referenced by every application and dependency. The editor shows decrypted values; SOPS encrypts them when you save:

```yaml
DATABASE_URL: postgres://billing:change-me@database:5432/billing_production
SECRET_KEY_BASE: change-me
POSTGRES_PASSWORD: change-me
RABBITMQ_DEFAULT_PASS: change-me
```

Commit only the encrypted `serve.secrets.yml`. During deploy, Serve sends its ciphertext to each host, decrypts it there, injects only the names requested by each container, and removes the temporary plaintext env file after Docker creates the container. Rotating a secret recreates only containers that reference that name.

Environment secrets ultimately become Docker environment variables and are visible to users with privileged Docker access. Serve does not yet support file-mounted runtime secrets.

### Complete application and PostgreSQL example

> **Stateful dependency lifecycle:** dependencies currently follow application versions during deploy and retention. The example below demonstrates environment and secret configuration, but Serve does not yet provide a database-specific upgrade or zero-downtime lifecycle. Use an externally managed database when that lifecycle is required.

```yaml
service: billing
image: ghcr.io/acme/billing
destination: production

servers:
  web:
    hosts:
      - deploy@app.example.com
    command: [./billing-server]
    app_port: 3000
    replicas: 2
    healthcheck:
      http:
        path: /up
        port: 3000
      interval: 2s
      timeout: 2s
      retries: 10

env:
  plain:
    APP_ENV: production
    LOG_LEVEL: info
  secret:
    - DATABASE_URL
    - SECRET_KEY_BASE

dependencies:
  postgres:
    image: postgres:16-alpine
    hosts:
      - deploy@app.example.com
    aliases:
      - database
    internal_port: 5432
    volumes:
      - postgres-data:/var/lib/postgresql/data
    env:
      plain:
        POSTGRES_USER: billing
        POSTGRES_DB: billing_production
      secret:
        - POSTGRES_PASSWORD

proxy:
  provider: kamal-proxy
  app_role: web
  hosts:
    - billing.example.com
  ssl: auto

networking:
  private_network: serve

retain_containers: 5
```

The application connects to PostgreSQL through the `database` network alias. Its `DATABASE_URL` and the dependency's `POSTGRES_PASSWORD` are separate secret entries, even when they contain related credentials.

## Publish a dependency port on the host

`internal_port` alone keeps a dependency reachable only through Serve's private Docker network. To make that TCP port available on a host interface, add an explicit `publish` block:

```yaml
dependencies:
  redis:
    image: redis:8-alpine
    hosts:
      - deploy@services.example.com
    aliases:
      - redis
    internal_port: 6379
    publish:
      host_port: 6379
      host_ip: 10.0.0.5
```

`publish.host_port` is required when `publish` is present. `publish.host_ip` defaults to `127.0.0.1`; reaching the dependency through a private VM address requires explicitly setting that address. Binding every interface requires the explicit value `0.0.0.0`. Omitting `publish` never creates a host-port binding.

Serve validates the container port, host port, and host IP before contacting a deployment host. The declared binding is part of desired state, so reconciliation and rollback preserve it. Because two containers cannot own the same host binding, a deploy or rollback briefly stops the previous version of a published dependency before starting its replacement; clients should retry transient connection failures.

Serve does not install or configure host/cloud firewalls, authentication, TLS, credentials, or host networking. Those remain operator responsibilities; expose a dependency beyond loopback only inside an independently secured network boundary.

## Documentation

- [Getting started and command reference](docs/getting-started.md)
- [Private service aliases](docs/private-service-aliases.md)
- [Private registry access](docs/private-registry-access.md)
- [How to contribute](docs/contributing.md)
