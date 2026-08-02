# Private Service Aliases

Serve can give application server roles stable names on their host's private Docker network. This is useful when a product is deployed from separate configuration files because its components use different images.

## Configure an alias

Declare one or more aliases on a server role:

```yaml
# serve.api.yml
service: platform-api
image: ghcr.io/acme/platform-api
destination: production

servers:
  web:
    hosts: [deploy@app.example.com]
    aliases: [api]
    app_port: 4000
    healthcheck:
      http:
        path: /up
        port: 4000
      interval: 2s
      timeout: 2s
      retries: 10

proxy:
  provider: kamal-proxy
  app_role: web
  hosts: [api.example.com]
  ssl: auto

networking:
  private_network: serve
```

Any container managed by Serve on the same host and `serve` network can connect to the role at:

```text
http://api:4000
```

`app_port` is the container port. It is not published directly on the host.

## Communicate between separate services

Use the same private network in every configuration and give each receiving service a unique alias:

```yaml
# serve.app.yml
service: platform-app
image: ghcr.io/acme/platform-app

servers:
  web:
    hosts: [deploy@app.example.com]
    aliases: [app]
    app_port: 3000

env:
  plain:
    API_URL: http://api:4000

networking:
  private_network: serve
```

```yaml
# serve.voice.yml
service: platform-voice
image: ghcr.io/acme/platform-voice

servers:
  web:
    hosts: [deploy@app.example.com]
    aliases: [voice]
    app_port: 5000
    healthcheck:
      http:
        path: /up
        port: 5000

env:
  plain:
    API_URL: http://api:4000

networking:
  private_network: serve
```

The API can similarly set `VOICE_URL: http://voice:5000`. Deploy the files independently:

```sh
serve deploy --config serve.app.yml --version v1
serve deploy --config serve.api.yml --version v1
serve deploy --config serve.voice.yml --version v1
```

## Deployment behavior

Serve protects alias handoff during blue-green deployment:

1. Candidate containers start without their configured server aliases.
2. Candidates with health checks must become healthy.
3. Serve activates aliases on the candidates and checks their health again after the network update.
4. Public proxy traffic switches to the candidates.
5. Serve removes aliases from the old version and applies retention.

Healthy old and candidate replicas may briefly share an alias during handoff. Docker DNS can return any active replica that owns the alias. If a role has no health check, Serve considers a successfully started container ready for alias activation.

A deployment fails before alias activation when another running container on the same network owns the requested alias. Existing replicas and versions of the same service, destination, and role are allowed to share it during cutover.

## Limitations

- Aliases are host-local. They cannot connect containers running on different deployment hosts.
- Every caller and receiver must use the same `networking.private_network` on that host.
- Alias names must be unique between unrelated roles, accessories, and separately deployed services sharing the network.
- Direct alias traffic bypasses kamal-proxy's ongoing health routing and TLS termination. Use health checks, request timeouts, retries, and application-level authentication as appropriate.
- Server roles still use the configuration's single top-level `image`, and one configuration still exposes only one `proxy.app_role`. Use separate configuration files for components with different images or public routes.

[Back to the README](../README.md)
