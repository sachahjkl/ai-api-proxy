[English](README.md) | [Français](README.fr.md)

# Codex subscription reverse proxy

This service forwards OpenCode traffic to the Codex backend used by a ChatGPT Plus or Pro subscription. It is not a proxy for the standard OpenAI API.

Its behavior comes from the OpenAI provider in OpenCode V2 at `opencode/packages/core/src/plugin/provider/openai.ts`:

- upstream: `https://chatgpt.com/backend-api/codex`;
- main endpoint: `/responses`;
- upstream authentication: ChatGPT OAuth token in `Authorization`;
- context: `chatgpt-account-id`, `originator`, and `session-id`;
- SSE streaming responses and, when the client uses it, WebSocket.

The proxy stores the ChatGPT OAuth credential and refreshes the access token. Clients send only the proxy shared secret.

The initial OAuth file contains this JSON:

```json
{
  "access": "jeton-acces",
  "refresh": "jeton-renouvellement",
  "expires": 1787824843000,
  "account_id": "identifiant-compte"
}
```

The proxy stores refreshed tokens in `OAUTH_STATE_FILE`. Protect the initial file and the state directory.

The proxy exposes the authenticated Codex manifest at `/models`. It also publishes a complete OpenCode catalog at `/api.json`.

| Simulacra model | Upstream model |
| --- | --- |
| `master` : Master (5.6 Sol) | `gpt-5.6-sol` |
| `marshal` : Marshal (5.6 Terra) | `gpt-5.6-terra` |
| `commander` : Commander (5.6 Luna) | `gpt-5.6-luna` |
| `general` : General (5.5) | `gpt-5.5` |
| `captain` : Captain (5.4) | `gpt-5.4` |
| `scout` : Scout (5.4 Mini) | `gpt-5.4-mini` |

A `/responses` request can use a Simulacra identifier. The proxy replaces it with the corresponding upstream identifier.

## Getting Started

```sh
go test ./...
go run .
```

With Nix:

```sh
nix develop
nix flake check
nix run
```

Variables:

| Variable | Default | Description |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `UPSTREAM_URL` | `https://chatgpt.com/backend-api/codex` | Fixed HTTPS upstream |
| `PROXY_TOKEN` | empty | Optional shared secret |
| `PROXY_TOKEN_FILE` | empty | File containing the shared secret |
| `OAUTH_CREDENTIAL_FILE` | required | JSON file containing the ChatGPT OAuth credential |
| `OAUTH_STATE_FILE` | required | Persistent file for refreshed tokens |

When `PROXY_TOKEN` is set, the client must send `Proxy-Authorization: Bearer <secret>`. The proxy removes this header before the request to OpenAI. The `Authorization` header remains reserved for the ChatGPT OAuth token.

Do not use `PROXY_TOKEN` and `PROXY_TOKEN_FILE` together. The NixOS module uses `PROXY_TOKEN_FILE` with a systemd credential.

For Docker:

```sh
export PROXY_TOKEN='un-secret-long-et-aleatoire'
export OAUTH_CREDENTIAL_FILE="$PWD/oauth.json"
docker compose up -d --build
curl http://127.0.0.1:8080/healthz
```

The Compose port is exposed only on loopback. Publish it with Caddy, Traefik, or your VPN solution. Use HTTPS unless all HTTP traffic stays within an encrypted tunnel such as WireGuard or Tailscale.

Caddy example:

```caddyfile
codex-proxy.example.net {
    reverse_proxy 127.0.0.1:8080
}
```

## OpenCode V2 Configuration

Set the catalog source before starting OpenCode:

```sh
export OPENCODE_MODELS_URL="https://codex-proxy.example.net"
```

The OpenCode client does not need a ChatGPT connection. Add this configuration without a `models` block:

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
        "Proxy-Authorization": "Bearer un-secret-long-et-aleatoire"
      }
    }
  }
}
```

`apiKey` satisfies the local OpenAI provider. The proxy removes the generated `Authorization` and injects its own OAuth token.

If access is already restricted to the VPN, leave `PROXY_TOKEN` empty and remove `headers` from the configuration.

Do not put `/backend-api/codex` in `baseURL`: the proxy adds this prefix. An OpenCode request to `/responses` becomes an upstream request to `/backend-api/codex/responses`.

## Security Limits

- Anyone who has `PROXY_TOKEN` can use the centralized Codex subscription.
- Anyone who controls the proxy can read prompts, responses, and the OAuth credential.
- The included logs record neither headers nor bodies. Also check the logging configuration of the TLS reverse proxy in front of this service.
- Do not publish this service directly on the Internet without TLS and access control.
- Use remains subject to the ChatGPT and Codex terms of service.
