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
    command: ./server
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

The host value is an SSH destination and may include a user, such as `deploy@example.com`. Point the proxy hostnames at the server before enabling automatic TLS.

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

Use a separate `serve.yml` for each application. All applications deployed to the same machine share its Serve agent, Docker network, and central `kamal-proxy` container.

One application can answer on multiple domains:

```yaml
proxy:
  provider: kamal-proxy
  app_role: web
  hosts:
    - api.x.com
    - api.you.com
  ssl: auto
```

Different applications can also use different domains on the same machine. Keep `service` names and proxy hostnames unique between applications.

## Private images

Authenticate Docker to private registries on every deployment host before deploying. Serve reads the host's standard Docker client configuration, including credential helpers, but does not provision credentials. See [Private registry access](docs/private-registry-access.md).

## Environment variables and secrets

Applications and accessories use the same environment shape:

```yaml
env:
  plain:
    NAME: value
  secret:
    - SECRET_NAME
```

- `env.plain` is a map of non-sensitive values stored directly in `serve.yml`, Serve's desired state, and Docker's container configuration.
- `env.secret` is a list of names resolved from the SOPS-encrypted `serve.secrets.yml` beside `serve.yml`.
- Top-level `env` applies to every application role. An accessory's nested `env` applies only to that accessory.
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
    command: ./billing-server
    app_port: 3000
  worker:
    hosts:
      - deploy@app.example.com
    command: ./billing-worker

env:
  plain:
    APP_ENV: production
    LOG_LEVEL: info
  secret:
    - DATABASE_URL
    - SECRET_KEY_BASE
```

Both `web` and `worker` receive all four variables. The plain values come from `serve.yml`; the two secret values come from `serve.secrets.yml`.

### Accessory environment

Put `env` inside an accessory when only that supporting container needs the values:

```yaml
accessories:
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
accessories:
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

Each accessory receives only its own nested environment.

### Create and edit secrets

Configure SOPS and its decryption credentials on the deployment machine and every host, then open the encrypted file through Serve:

```sh
serve secrets edit --file serve.secrets.yml
```

Inside the editor, add the names referenced by every application and accessory. The editor shows decrypted values; SOPS encrypts them when you save:

```yaml
DATABASE_URL: postgres://billing:change-me@database:5432/billing_production
SECRET_KEY_BASE: change-me
POSTGRES_PASSWORD: change-me
RABBITMQ_DEFAULT_PASS: change-me
```

Commit only the encrypted `serve.secrets.yml`. During deploy, Serve sends its ciphertext to each host, decrypts it there, injects only the names requested by each container, and removes the temporary plaintext env file after Docker creates the container. Rotating a secret recreates only containers that reference that name.

Environment secrets ultimately become Docker environment variables and are visible to users with privileged Docker access. Serve does not yet support file-mounted runtime secrets.

### Complete application and PostgreSQL example

> **Stateful accessory lifecycle:** accessories currently follow application versions during deploy and retention. The example below demonstrates environment and secret configuration, but Serve does not yet provide a database-specific upgrade or zero-downtime lifecycle. Use an externally managed database when that lifecycle is required.

```yaml
service: billing
image: ghcr.io/acme/billing
destination: production

servers:
  web:
    hosts:
      - deploy@app.example.com
    command: ./billing-server
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

accessories:
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

The application connects to PostgreSQL through the `database` network alias. Its `DATABASE_URL` and the accessory's `POSTGRES_PASSWORD` are separate secret entries, even when they contain related credentials.

## Documentation

- [Getting started and command reference](docs/getting-started.md)
- [Private registry access](docs/private-registry-access.md)
- [How to contribute](docs/contributing.md)
