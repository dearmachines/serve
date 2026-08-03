# Skills

This directory contains LLM-facing instructions for working on Serve.

Use `skills/serve/SKILL.md` with coding agents that support skill files. If your agent does not have a skill system, paste the contents of that file into the agent's system/developer instructions before asking it to modify this repository.

Host provisioning is outside Serve's scope: Docker, the Serve binary, the systemd unit, required directories, SOPS, and credentials are installed and configured manually.

## Private service alias quick reference

Server roles can define stable, host-local names on the private Docker network:

```yaml
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

Containers on that host and network can connect to `http://api:4000`. Server aliases are activated after configured health checks pass, must be unique across unrelated workloads sharing the network, and bypass kamal-proxy's ongoing health routing. They do not work across hosts. See `docs/private-service-aliases.md` and `skills/serve/SKILL.md` for the complete behavior and test requirements.

## Environment quick reference

Serve uses `env.plain` for non-sensitive values and `env.secret` for names resolved from the SOPS-encrypted `serve.secrets.yml`.

```yaml
# Applied to every application role.
env:
  plain:
    APP_ENV: production
  secret:
    - DATABASE_URL

dependencies:
  postgres:
    image: postgres:16-alpine
    hosts: [deploy@app.example.com]
    aliases: [database]
    internal_port: 5432
    # Applied only to this dependency.
    env:
      plain:
        POSTGRES_USER: app
        POSTGRES_DB: app_production
      secret:
        - POSTGRES_PASSWORD
```

The corresponding decrypted editor view of `serve.secrets.yml` contains each referenced name:

```yaml
DATABASE_URL: postgres://app:change-me@database:5432/app_production
POSTGRES_PASSWORD: change-me
```

The PostgreSQL snippet demonstrates environment scoping; stateful dependencies still follow application deploy and retention lifecycle.

See `skills/serve/SKILL.md` for complete examples, scoping rules, security constraints, and the tests required when changing this behavior.
