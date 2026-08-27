# Codex subscription reverse proxy

Ce service fait suivre le trafic OpenCode vers le backend Codex utilise par un abonnement ChatGPT Plus ou Pro. Ce n'est pas un proxy pour l'API OpenAI classique.

Le comportement vient du provider OpenAI d'OpenCode V2 present dans `opencode/packages/core/src/plugin/provider/openai.ts` :

- upstream : `https://chatgpt.com/backend-api/codex` ;
- endpoint principal : `/responses` ;
- authentification upstream : jeton OAuth ChatGPT dans `Authorization` ;
- contexte : `chatgpt-account-id`, `originator` et `session-id` ;
- reponses en streaming SSE et, si le client l'utilise, WebSocket.

Le proxy stocke le credential OAuth ChatGPT et renouvelle le jeton d'accès. Les clients envoient seulement le secret partagé du proxy.

Le fichier OAuth initial contient ce JSON :

```json
{
  "access": "jeton-acces",
  "refresh": "jeton-renouvellement",
  "expires": 1787824843000,
  "account_id": "identifiant-compte"
}
```

Le proxy conserve les renouvellements dans `OAUTH_STATE_FILE`. Protégez le fichier initial et le répertoire d'état.

Le proxy expose le manifeste Codex authentifié sur `/models`. Il conserve les capacités et les niveaux de raisonnement fournis par OpenAI.

| Modèle Simulacra | Modèle upstream |
| --- | --- |
| `master` | `gpt-5.6-sol` |
| `marshal` | `gpt-5.6-terra` |
| `commander` | `gpt-5.6-luna` |
| `general` | `gpt-5.5` |
| `captain` | `gpt-5.4` |
| `scout` | `gpt-5.4-mini` |

Une requête `/responses` peut utiliser un identifiant Simulacra. Le proxy le remplace par l'identifiant upstream correspondant.

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
| `OAUTH_CREDENTIAL_FILE` | requis | Fichier JSON du credential OAuth ChatGPT |
| `OAUTH_STATE_FILE` | requis | Fichier persistant des jetons renouvelés |

Quand `PROXY_TOKEN` est defini, le client doit envoyer `Proxy-Authorization: Bearer <secret>`. Ce header est supprime avant la requete vers OpenAI. Le header `Authorization` reste reserve au jeton OAuth ChatGPT.

N'utilisez pas `PROXY_TOKEN` et `PROXY_TOKEN_FILE` ensemble. Le module NixOS utilise `PROXY_TOKEN_FILE` avec un credential systemd.

Pour Docker :

```sh
export PROXY_TOKEN='un-secret-long-et-aleatoire'
export OAUTH_CREDENTIAL_FILE="$PWD/oauth.json"
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

Le client OpenCode n'a pas besoin d'une connexion ChatGPT. Ajoutez ceci dans `~/.config/opencode/opencode.jsonc` ou dans la configuration du projet :

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "model": "simulacra/commander",
  "providers": {
    "simulacra": {
      "name": "Simulacra",
      "package": "@opencode-ai/ai/providers/openai/responses",
      "settings": {
        "baseURL": "https://codex-proxy.example.net",
        "apiKey": "unused"
      },
      "headers": {
        "Proxy-Authorization": "Bearer un-secret-long-et-aleatoire"
      },
      "models": {
        "master": {
          "name": "Master"
        },
        "marshal": {
          "name": "Marshal"
        },
        "commander": {
          "name": "Commander"
        },
        "general": {
          "name": "General"
        },
        "captain": {
          "name": "Captain"
        },
        "scout": {
          "name": "Scout"
        }
      }
    }
  }
}
```

`apiKey` satisfait le provider OpenAI local. Le proxy supprime l'`Authorization` généré et injecte son propre jeton OAuth.

Si l'accès est déjà limité au VPN, laissez `PROXY_TOKEN` vide et retirez `headers` de la configuration.

Ne mettez pas `/backend-api/codex` dans `baseURL` : le proxy ajoute ce prefixe. Une requete OpenCode vers `/responses` devient une requete upstream vers `/backend-api/codex/responses`.

## Limites de securite

- Toute personne qui possède `PROXY_TOKEN` peut utiliser l'abonnement Codex centralisé.
- Toute personne qui contrôle le proxy peut lire les prompts, les réponses et le credential OAuth.
- Les journaux inclus n'enregistrent ni headers ni corps. Verifiez aussi la configuration des journaux du reverse proxy TLS place devant.
- Ne publiez pas ce service directement sur Internet sans TLS et controle d'acces.
- L'usage reste soumis aux conditions du service ChatGPT et de Codex.
