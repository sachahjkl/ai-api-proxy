# Codex subscription reverse proxy

This service forwards requests to the Codex backend used by a ChatGPT Plus or Pro subscription. It does not proxy the standard OpenAI API.

The proxy owns the ChatGPT OAuth credential and refreshes its access token. Clients authenticate with a separate proxy token.

## Deployment

The primary deployment uses the NixOS module through `../nixconfig`. Both the NixOS service and the Docker image use the same Nix-built executable.

```nix
services.codex-proxy = {
  enable = true;
  listenAddress = "127.0.0.1:8083";
  publicUrl = "https://codex.sacha.house";
  proxyTokenFile = "/run/secrets/codex-proxy/token";
  oauthCredentialFile = "/run/secrets/codex-proxy/oauth";
};
```

Use runtime secret paths, not Nix store files, for real credentials. The module loads secrets through systemd credentials.

The service stores refreshed credentials in `/var/lib/codex-proxy/oauth.json`. systemd creates a private state directory for the service user.

The integration in `../nixconfig/services/codex-proxy-service.mod.nix` sets the public URL and uses SOPS-managed credential paths.

### Coordinated input update

This version requires `publicUrl` in the NixOS module. The matching `../nixconfig` change requires this version of the proxy input.

After publishing the proxy revision, update the `ai-api-proxy` input in `../nixconfig`. Review the lock-file change before deployment.

For local validation without changing the production pin:

```sh
nix eval 'path:../nixconfig#nixosConfigurations.homelab.config.systemd.services.codex-proxy.environment' \
  --json --override-input ai-api-proxy "path:$PWD" --no-write-lock-file
```

## Development

Use the development shell for project commands:

```sh
nix develop

gofmt -w *.go
go test -race -timeout 120s ./...
go vet ./...
prek run --all-files
nix flake check "path:$PWD" --no-write-lock-file
```

The shell includes Go, a C compiler for race detection, formatters, and Git hooks. Nix builds the deployed executable with CGO disabled.

Flake checks cover the executable, race tests, Git hooks, Docker image, and a NixOS deployment test. The deployment test requires a Linux builder with KVM.

CI runs on disposable GitHub-hosted runners rather than the deployment host.

Run fuzz tests separately:

```sh
go test -run '^$' -fuzz '^FuzzCatalog$' -fuzztime 10s -parallel 2
go test -run '^$' -fuzz '^FuzzRequestModel$' -fuzztime 10s -parallel 2
```

### Local startup

Create a seed credential outside the repository. Use this schema:

```json
{
  "access": "access-token",
  "refresh": "refresh-token",
  "expires": 1787824843000,
  "account_id": "account-id"
}
```

`expires` is a Unix timestamp in milliseconds. The example values are placeholders, not usable credentials.

```sh
export OAUTH_CREDENTIAL_FILE="$HOME/.config/codex-proxy/oauth.json"
export OAUTH_STATE_FILE="$HOME/.local/state/codex-proxy/oauth.json"
export PUBLIC_URL="http://127.0.0.1:8080"
export PROXY_TOKEN_FILE="$HOME/.config/codex-proxy/token"
chmod 600 "$OAUTH_CREDENTIAL_FILE" "$PROXY_TOKEN_FILE"
nix develop --command go run .
```

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `LISTEN_ADDR` | `127.0.0.1:8080` | HTTP listen address |
| `PUBLIC_URL` | required | Public HTTP or HTTPS origin advertised in `/api.json` |
| `UPSTREAM_URL` | `https://chatgpt.com/backend-api/codex` | Fixed HTTPS upstream |
| `PROXY_TOKEN` | empty | Shared proxy token |
| `PROXY_TOKEN_FILE` | empty | File containing the shared proxy token |
| `ALLOW_UNAUTHENTICATED` | `false` | Explicitly permit requests without a proxy token |
| `OAUTH_CREDENTIAL_FILE` | required | Seed ChatGPT OAuth credential file |
| `OAUTH_STATE_FILE` | required | Persistent file for refreshed credentials |

Set either `PROXY_TOKEN` or `PROXY_TOKEN_FILE`, not both. Clients send `Proxy-Authorization: Bearer <secret>`.

The proxy removes this header before forwarding. It replaces client `Authorization` and account headers with its own OAuth credential.

`PUBLIC_URL` contains only a scheme and host, with an optional port. The proxy ignores forwarded-origin headers when generating the catalog.

For an access-controlled VPN deployment without a token, explicitly set `ALLOW_UNAUTHENTICATED=true`. The NixOS equivalent is `allowUnauthenticated = true`.

## Docker

Nix generates the only container image through `dockerTools.buildLayeredImage`. There is no separate Dockerfile build.

```sh
nix build .#dockerImage
docker load < result
export PUBLIC_URL="https://codex-proxy.example.net"
export PROXY_TOKEN='replace-with-a-random-secret'
export OAUTH_CREDENTIAL_FILE="/secure/path/oauth.json"
docker compose up -d
```

The image runs as UID and GID `65532`. Its state directory belongs to that user and has mode `0700`.

For Docker, prepare a dedicated seed-file copy outside the repository. Set its owner to `65532:65532` and its mode to `0400`.

Do not change ownership of the secret managed by the NixOS deployment. Docker and NixOS must not run concurrently with the same rotating credential.

Compose uses a persistent state volume and publishes its HTTP port only on loopback. The container filesystem is read-only except for the state volume.

For an existing volume, check its ownership before startup. Fresh volumes inherit the image's state-directory ownership.

## Health and shutdown

- `GET /healthz` reports local process liveness without authentication or external calls.
- `GET /readyz` checks OAuth readiness and requires the configured proxy authentication.
- Readiness returns HTTP 200 when credentials are usable, or HTTP 503 when refresh or persistence fails.

Readiness does not test model availability or send an inference request. It includes the credential expiry time but never includes tokens.

OAuth refresh has a ten-second deadline. Concurrent requests share one refresh, and waiting clients can cancel independently.

After refresh or persistence failure, requests receive an error during a thirty-second retry delay. Successful token rotation is retained in memory despite persistence failure.

On SIGTERM, the server waits up to ten seconds for HTTP requests. It then closes remaining connections, including upgraded WebSocket connections.

The process also waits up to ten seconds for an active OAuth update. systemd and Compose allow twenty-five seconds before forced termination.

## Models and transports

The authenticated `/models` endpoint publishes the mapped Codex manifest. The public `/api.json` endpoint adds Simulacra to the OpenCode model catalog.

| Simulacra model | Upstream model |
| --- | --- |
| `grandmaster`: Grandmaster (6 Astra) | `gpt-6-astra` |
| `master`: Master (5.6 Sol) | `gpt-5.6-sol` |
| `master-1m`: Master (5.6 Sol, 1M) | `gpt-5.6-sol` |
| `marshal`: Marshal (5.6 Terra) | `gpt-5.6-terra` |
| `marshal-1m`: Marshal (5.6 Terra, 1M) | `gpt-5.6-terra` |
| `commander`: Commander (5.6 Luna) | `gpt-5.6-luna` |
| `commander-1m`: Commander (5.6 Luna, 1M) | `gpt-5.6-luna` |
| `general`: General (5.5) | `gpt-5.5` |
| `captain`: Captain (5.4) | `gpt-5.4` |
| `scout`: Scout (5.4 Mini) | `gpt-5.4-mini` |

HTTP POST requests to `/responses` translate Simulacra model identifiers. SSE responses stream without buffering.

WebSocket connections pass through without message rewriting. Use native upstream model identifiers inside WebSocket messages, not Simulacra aliases.

The `-1m` aliases advertise 1,000,000 context tokens and 872,000 input tokens. Other aliases advertise 400,000 context tokens and 272,000 input tokens, except `grandmaster`, which retains native limits.

Output limits come from the upstream model catalog. All configured model aliases must exist in that catalog before `/api.json` becomes available.

The proxy validates catalogs before caching them for five minutes. A failed fetch has a thirty-second retry delay and does not replace cached data.

Expired cached data is not served. Invalid or oversized catalogs return HTTP 502.

## OpenCode V2 configuration

Set the catalog source before starting OpenCode:

```sh
export OPENCODE_MODELS_URL="https://codex-proxy.example.net"
```

The client does not need a ChatGPT connection. Add this configuration without a `models` block:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "model": "simulacra/commander",
  "providers": {
    "simulacra": {
      "settings": {
        "apiKey": "unused"
      },
      "headers": {
        "Proxy-Authorization": "Bearer replace-with-a-random-secret"
      }
    }
  }
}
```

`apiKey` satisfies the local OpenAI provider. The proxy replaces the generated `Authorization` header with its own OAuth token.

Do not add `/backend-api/codex` to the client URL. The proxy adds that prefix to upstream requests.

## Credential recovery

Only one proxy process can own a rotating OAuth credential. Do not share its refresh token with another proxy or an independently refreshing client.

The state file takes precedence over the seed file. Replacing only the seed does not replace existing persisted credentials.

If persistence fails, restore directory access or disk space while the process remains running. The next request after the retry delay retries persistence without rotating again.

If the refresh token is revoked or recovery requires a new login:

1. Stop the proxy.
2. Obtain a new OAuth credential through the authorized login flow.
3. Replace the seed secret through the deployment's secret manager.
4. Remove the obsolete state file from the proxy's private state directory.
5. Start the proxy.
6. Check authenticated `/readyz` and inspect the service logs.

On NixOS, use the service's resolved state path. Dynamic-user state can reside under `/var/lib/private/codex-proxy`.

## Security and resource limits

- Anyone with the proxy token can consume the centralized subscription.
- Anyone controlling the proxy can read prompts, responses, and OAuth credentials.
- Request bodies are limited to 32 MiB. Oversized `/responses` requests return HTTP 413.
- Request uploads have a sixty-second deadline. Upstream response headers have a separate sixty-second deadline.
- Model catalogs are limited to 16 MiB. OAuth refresh responses are limited to 1 MiB.
- Established SSE and WebSocket streams have no total duration limit.
- Logs contain request methods, paths, statuses, and durations, but not headers, query strings, or bodies.
- Use TLS and access control before publishing the service. An encrypted VPN can provide both.
- Check the logging configuration of any reverse proxy in front of this service.
- Use remains subject to the ChatGPT and Codex terms of service.
