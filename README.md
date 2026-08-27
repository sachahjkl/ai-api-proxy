# Codex subscription reverse proxy

Ce service fait suivre le trafic OpenCode vers le backend Codex utilise par un abonnement ChatGPT Plus ou Pro. Ce n'est pas un proxy pour l'API OpenAI classique.

Le comportement vient du provider OpenAI d'OpenCode V2 present dans `opencode/packages/core/src/plugin/provider/openai.ts` :

- upstream : `https://chatgpt.com/backend-api/codex` ;
- endpoint principal : `/responses` ;
- authentification upstream : jeton OAuth ChatGPT dans `Authorization` ;
- contexte : `chatgpt-account-id`, `originator` et `session-id` ;
- reponses en streaming SSE et, si le client l'utilise, WebSocket.

Le proxy ne stocke pas et ne renouvelle pas les jetons ChatGPT. OpenCode continue de faire la connexion OAuth et le renouvellement. Le jeton traverse donc le VPN et le proxy pendant chaque requete.

## Demarrage

```sh
go test ./...
go run .
```

Avec Nix :

```sh
nix develop
nix flake check
nix run
```

Variables :

| Variable | Defaut | Description |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8080` | Adresse d'ecoute HTTP |
| `UPSTREAM_URL` | `https://chatgpt.com/backend-api/codex` | Upstream HTTPS fixe |
| `PROXY_TOKEN` | vide | Secret partage facultatif |
| `PROXY_TOKEN_FILE` | vide | Fichier qui contient le secret partage |

Quand `PROXY_TOKEN` est defini, le client doit envoyer `Proxy-Authorization: Bearer <secret>`. Ce header est supprime avant la requete vers OpenAI. Le header `Authorization` reste reserve au jeton OAuth ChatGPT.

N'utilisez pas `PROXY_TOKEN` et `PROXY_TOKEN_FILE` ensemble. Le module NixOS utilise `PROXY_TOKEN_FILE` avec un credential systemd.

Pour Docker :

```sh
export PROXY_TOKEN='un-secret-long-et-aleatoire'
docker compose up -d --build
curl http://127.0.0.1:8080/healthz
```

Le port du compose n'est expose que sur loopback. Publiez-le avec Caddy, Traefik ou votre solution VPN. Utilisez HTTPS sauf si le trafic HTTP reste entierement dans un tunnel chiffre comme WireGuard ou Tailscale.

Exemple Caddy :

```caddyfile
codex-proxy.example.net {
    reverse_proxy 127.0.0.1:8080
}
```

## Configuration OpenCode V2

Connectez d'abord OpenCode a `ChatGPT Pro/Plus (browser)` ou `ChatGPT Pro/Plus (headless)`. Ajoutez ensuite ceci dans `~/.config/opencode/opencode.jsonc` ou dans la configuration du projet :

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "providers": {
    "openai": {
      "settings": {
        "baseURL": "https://codex-proxy.example.net"
      },
      "headers": {
        "Proxy-Authorization": "Bearer un-secret-long-et-aleatoire"
      }
    }
  }
}
```

Si l'acces est deja limite au VPN, laissez `PROXY_TOKEN` vide et retirez `headers` de la configuration. Cette option evite de conserver un second secret dans le fichier OpenCode.

Ne mettez pas `/backend-api/codex` dans `baseURL` : le proxy ajoute ce prefixe. Une requete OpenCode vers `/responses` devient une requete upstream vers `/backend-api/codex/responses`.

## Limites de securite

- Toute personne qui controle le proxy peut lire les prompts, les reponses et le jeton OAuth en memoire ou sur le reseau local si TLS s'arrete avant le proxy.
- Les journaux inclus n'enregistrent ni headers ni corps. Verifiez aussi la configuration des journaux du reverse proxy TLS place devant.
- Ne publiez pas ce service directement sur Internet sans TLS et controle d'acces.
- L'usage reste soumis aux conditions du service ChatGPT et de Codex.
