# ai-usage-exporter

Exportateur Prometheus multi-abonnements IA. Il interroge périodiquement les
quotas et consommations de plusieurs fournisseurs, conserve le dernier état
valide, et expose le tout sur `/metrics`.

- **Pas d'UI**, **pas de Prometheus entrant** : uniquement du pull HTTP.
- **Polling en arrière-plan dès le démarrage**, providers collectés en parallèle.
- **Snapshots concurrent-safe** : un scrape lit toujours un état complet.
- **Panne isolée** : un provider en erreur ne bloque ni `/metrics`, ni `/readyz`,
  ni les autres providers ; les dernières données valides restent exposées.
- **Secrets jamais exposés** : aucune clé, aucun message d'erreur dans les
  labels ou les réponses HTTP. Les erreurs journalisées sont expurgées.

Image : `ghcr.io/bapttf/ai-usage-exporter`.

## Fournisseurs

| Provider | Credential | Données |
|---|---|---|
| `opencode` | `OPENCODE_GO_API_KEY` + variantes suffixées | quotas monthly / weekly / rolling 5h |
| `cursor` | `CURSOR_API_KEY` ou `CURSOR_REFRESH_TOKEN` | quota monthly, Grok Bot weekly, dépenses incluses/on-demand, prix du plan |
| `devin` | `DEVIN_API_KEY` | quotas daily / weekly, consommation ACU par produit |

OpenCode est optionnel si Cursor ou Devin est configuré. En revanche, un
démarrage sans **aucun** provider est refusé. Cursor et Devin sont optionnels et
totalement indépendants.

## Démarrage rapide

```sh
OPENCODE_GO_API_KEY="sk-..." \
CURSOR_API_KEY="crsr_..." \
DEVIN_API_KEY="cog_..." \
DEVIN_ORG_ID="org-..." \
PORT=8080 POLL_INTERVAL=5m REQUEST_TIMEOUT=15s \
./ai-usage-exporter
```

```sh
curl -fsS localhost:8080/healthz   # {"status":"ok"}
curl -fsS localhost:8080/readyz    # {"status":"ready"}
curl -fsS localhost:8080/metrics
```

Avec Docker :

```sh
docker run --rm -p 8080:8080 \
  -e OPENCODE_GO_API_KEY="sk-..." \
  ghcr.io/bapttf/ai-usage-exporter:latest
```

## Configuration

Toute la configuration passe par l'environnement. Une valeur invalide fait
échouer le démarrage avec un message qui nomme la variable, jamais sa valeur.

| Variable | Défaut | Description |
|---|---:|---|
| `PORT` | `8080` | Port d'écoute HTTP (entier 1–65535). |
| `POLL_INTERVAL` | `5m` | Intervalle entre deux collectes. Première collecte immédiate. Durée Go positive (`30s`, `5m`). |
| `REQUEST_TIMEOUT` | `15s` | Timeout de chaque requête HTTP sortante. Durée Go positive. |
| `OPENCODE_GO_API_KEY` | — | Première clé OpenCode Go. Label `main`. |
| `OPENCODE_GO_API_KEY_<SUFFIXE>` | — | Clés supplémentaires. Le suffixe devient le label, en minuscules et limité à `[a-z0-9_-]` (64 caractères max) : `_R` → `r`, `_ALICE` → `alice`. |
| `CURSOR_API_KEY` | — | Clé API utilisateur Cursor (`crsr_…`), prioritaire. |
| `CURSOR_REFRESH_TOKEN` | — | Refresh OAuth historique. Une valeur commençant par `crsr_` est traitée comme une clé API. |
| `DEVIN_API_KEY` | — | Token Devin (`cog_…`), active le provider Devin. |
| `DEVIN_ORG_ID` | — | Organisation Devin (`org-…`). Utile quand `/v3/self` ne renvoie pas d'`org_id` pour un PAT. |
| `DEVIN_RESET_DAY` | `5` | Repli local pour le cycle de facturation Devin. Seules les valeurs 1–28 sont retenues ; hors bornes, retour à `5`. Les bornes `planStart`/`planEnd` de l'API priment quand elles courent. |
| `DEVIN_API_SERVER` | `https://server.codeium.com` | Hôte Connect-RPC des quotas daily/weekly Devin. |

`POLL_INTERVAL` remplace l'ancien `CACHE_TTL` du dashboard : c'est la cadence
réelle des appels sortants. Les secrets peuvent provenir de n'importe quel
gestionnaire de secrets. Les valeurs sont `TrimSpace` : un `\n` final de secret
Kubernetes est toléré.

Au démarrage, `ai_usage_collection_success` vaut `0` tant que la première
collecte n'a pas abouti ; `ai_usage_collection_last_success_timestamp_seconds`
n'apparaît qu'après le premier succès.

## Architecture

```
main
 ├─ config.Load()                 validation + découverte des clés
 ├─ collector.Run(ctx, interval)  boucle de polling (goroutine)
 │    └─ providers en parallèle (OpenCode clés en parallèle)
 │         └─ snapshot atomique immuable
 └─ server.Run(ctx)               /metrics /healthz /readyz
      └─ metrics.Collector         rend le snapshot complet à chaque scrape
```

- La première collecte démarre immédiatement ; le serveur HTTP démarre sans
  attendre sa fin.
- `elapsed_ratio`, `dry_days` et `severity` sont calculés à l'instant de la
  collecte, pas à chaque scrape. Avec un `POLL_INTERVAL` court, l'écart est
  négligeable ; ne pas les recalculer côté dashboard.
- Un échec conserve les données précédentes de **cette souscription**,
  `ai_usage_collection_success` passe à `0` et
  `ai_usage_collection_last_success_timestamp_seconds` reste inchangé.
- Une dégradation secondaire (Grok Bot Cursor, quotas Devin) laisse les données
  principales fraîches et expose `ai_usage_collection_partial 1`. Les messages
  `PlanError`, `GrokError` et `QuotaError` sont transmis au collector, expurgés
  puis journalisés ; ils ne deviennent jamais des labels ni des métriques.
- `/readyz` ne dépend que du serveur HTTP. Une panne d'API fournisseur ne doit
  jamais empêcher Prometheus de scraper le dernier état valide.
- À l'arrêt (`SIGINT`/`SIGTERM`) : le contexte est annulé, ce qui interrompt
  immédiatement toutes les requêtes sortantes en cours (OpenCode, auth/exchange
  Cursor, PlanInfo, Grok et fallback web, Devin REST/seat). Le collector ne
  démarre plus de nouvelle collecte, même si le ticker est prêt, puis
  `Shutdown` HTTP est borné à 10 s. Un provider qui ignorerait le contexte
  resterait borné par `REQUEST_TIMEOUT` ; les clients fournis ici le respectent
  tous.

## Contrat de métriques

Toutes les métriques sont des **gauges**. Les labels sont bornés : aucune
adresse, aucun message d'erreur, aucune valeur libre issue des APIs.

### Quotas

| Métrique | Labels | Sens |
|---|---|---|
| `ai_usage_quota_used_ratio` | `provider`, `subscription`, `window`, `quota` | Fraction consommée, `0..1`. |
| `ai_usage_quota_reset_timestamp_seconds` | idem | Instant de reset (Unix). Omise si inconnue. |
| `ai_usage_quota_elapsed_ratio` | idem | Fraction de la période écoulée, `0..1`. Omise quand non calculable. |
| `ai_usage_quota_dry_days` | idem | Jours passés au plafond avant le reset au rythme moyen actuel. Omise quand non calculable, toujours absente en rolling. |
| `ai_usage_quota_budget_severity` | idem | `0` green, `1` amber, `2` red. |

Valeurs de `window` : `monthly`, `weekly`, `rolling_5h`, `daily`.
Valeurs de `quota` : `total`, `included`, `api`, `on_demand`, `grok`.

Le calcul de rythme (`elapsed_ratio`, `dry_days`) reprend les règles métier
historiques : monthly commence à l'anniversaire un mois avant le reset, weekly
à reset − 7 jours et daily à reset − 24 heures.

Pour OpenCode monthly/weekly, `severity` reprend aussi `ComputeBudget` : seuils
absolus amber à 80 % et red à 95 %, red au-delà de 3 jours à sec, et plafonnage
à amber d'un red de rythme en early period (< 5 %). Si `ComputeBudget` n'est pas
calculable — reset absent ou illisible — `severity` retombe exactement sur
`AbsoluteLevel` comme le legacy, et `elapsed_ratio`/`dry_days` restent absents.
Pour Cursor et Devin, comme dans le board historique, `severity` vient toujours
de `AbsoluteLevel` (amber ≥ 70 %, red ≥ 90 %) ; le rythme reste seulement
informatif. Le rolling 5h OpenCode utilise ces mêmes seuils absolus et n'a ni
`elapsed_ratio` ni `dry_days`.

### Coûts et consommation

| Métrique | Labels | Sens |
|---|---|---|
| `ai_usage_subscription_monthly_cost_usd` | `provider`, `subscription` | Coût mensuel connu. Aujourd'hui produit pour Cursor quand `price` est un prix mensuel explicite (`$20/mo`, `$20 / month`). Jamais inventé. |
| `ai_usage_spend_usd` | `provider`, `subscription`, `bucket` | Dépense connue en USD. `bucket` ∈ `included`, `on_demand` (Cursor). |
| `ai_usage_spend_limit_usd` | idem | Plafond connu en USD pour le bucket. Omis si la limite est inconnue. |
| `ai_usage_acu_consumed` | `provider`, `subscription`, `product` | ACU Devin consommés sur le cycle. `product` ∈ `all`, `devin`, `cascade`, `terminal`. |
| `ai_usage_acu_limit` | `provider`, `subscription` | Limite ACU absolue **uniquement si réellement connue**. Aucune API actuelle ne l'expose : la série est donc normalement absente. |
| `ai_usage_acu_cycle_start_timestamp_seconds` | `provider`, `subscription` | Début du cycle de facturation ACU réellement interrogé. |
| `ai_usage_acu_cycle_reset_timestamp_seconds` | `provider`, `subscription` | Fin/reset du cycle de facturation ACU réellement interrogé. |

### Santé de collecte

| Métrique | Labels | Sens |
|---|---|---|
| `ai_usage_collection_success` | `provider`, `subscription` | `1` dernier succès, `0` échec (données précédentes conservées). |
| `ai_usage_collection_last_success_timestamp_seconds` | idem | Instant du dernier succès. Omise tant qu'aucun succès n'a eu lieu. |
| `ai_usage_collection_duration_seconds` | idem | Durée de la dernière tentative, en secondes. |
| `ai_usage_collection_partial` | idem | `1` si la collecte principale a réussi mais qu'une source secondaire est dégradée ; `0` sinon (et toujours `0` en échec dur). |

### Mapping par provider

Cursor et Devin exposent une seule souscription par credential, labellisée
`subscription="default"`. OpenCode expose une souscription par clé.

| Provider | `window` / `quota` |
|---|---|
| OpenCode | `monthly/total`, `weekly/total`, `rolling_5h/total` |
| Cursor | `monthly/total`, `monthly/included` (barre « Auto » Cursor), `monthly/api`, `monthly/on_demand` si limite connue, `weekly/grok` si allowance |
| Devin | `daily/total`, `weekly/total` ; ACU via `ai_usage_acu_consumed` |

Pour Cursor, `total` est `totalPercentUsed`, `included` est `autoPercentUsed`
(la partie « Auto » du forfait inclus) et `api` est `apiPercentUsed`. La
sémantique du dashboard historique est conservée ; seul le nom de label
`included` remplace « Auto » pour rester dans le contrat borné.

## Exemples PromQL

```promql
# Quotas presque pleins
ai_usage_quota_used_ratio > 0.9

# Budget en warning ou critique
ai_usage_quota_budget_severity >= 1

# Souscriptions dont la dernière collecte a échoué
ai_usage_collection_success == 0

# Dégradations secondaires
ai_usage_collection_partial == 1

# Jours à sec avant reset
ai_usage_quota_dry_days > 3
```

## Sécurité

- Les credentials ne sont envoyés que dans les en-têtes vers leur API.
- Aucun secret, token dérivé ou message d'erreur n'apparaît dans un label,
  `/metrics`, `/healthz`, `/readyz` ou un log de démarrage.
- Les erreurs de collecte sont expurgées avant log : valeurs connues,
  `Bearer …`, JWT et préfixes `sk-`, `crsr_`, `cog_`.
- L'access JWT Cursor reste en mémoire process. Le refresh OAuth tourné est
  conservé en mémoire ; s'il est renouvelé par l'API, mettre à jour le secret
  stocké pour survivre à un redémarrage.
- Le conteneur tourne en utilisateur non-root (`65532:65532`).

## Développement

```sh
gofmt -w .
go vet ./...
go test -race ./...
go build ./cmd/ai-usage-exporter
```

Le binaire s'appelle `ai-usage-exporter`. Les tests couvrent notamment le
budget historique, les clients HTTP (succès, 401, 429, réponses invalides,
soft-fail Grok/Devin), la conservation du dernier snapshot, la parallélisation
des providers, l'expurgation des secrets, les formats Prometheus et les
transitions `/readyz`.

## Docker / CI

Le `Dockerfile` est multi-stage, construit statiquement (`CGO_ENABLED=0`) et
tourne sur une image `scratch` non-root avec les certificats CA. Il épingle Go
`1.26.11` et le digest du builder (correctifs stdlib `net/http`, `crypto/tls`,
`crypto/x509`). La CI exécute `gofmt`, `go vet`, `go test -race`, `go build`,
`govulncheck` et un build multi-architecture sur chaque PR. Sur `main`, elle
publie `linux/amd64` et `linux/arm64` sur GHCR avec les tags `latest` et
`sha-<7 hex>`, une provenance et un SBOM, avec des permissions minimales
(`contents: read`, `packages: write`).

```sh
docker build -t ai-usage-exporter .
docker run --rm -p 8080:8080 -e OPENCODE_GO_API_KEY="sk-..." ai-usage-exporter
```

## Limites et points à réconcilier

- **Limite ACU Devin** : l'API publique n'expose pas de plafond ACU absolu.
  `ai_usage_acu_limit` existe dans le contrat mais n'est pas émise sans source
  réelle. Ne pas déduire une limite d'un pourcentage.
- **Solde on-demand Devin** : `overageBalanceMicros` est parsé par le client
  (`OnDemandBalanceUSD`) mais n'est pas exposé, faute de métrique au contrat.
  À ajouter si le dashboard doit l'afficher.
- **ACU par produit** : `all` vient de `total_acus`; `devin`, `cascade` et
  `terminal` sont sommés depuis `consumption_by_date`. La catégorie `review`
  n'est volontairement pas exposée (cardinalité bornée) mais reste incluse dans
  `all` si l'API la compte.
- **Cohérence Cursor** : les trois barres historiques Total/Auto/API deviennent
  `total`/`included`/`api`. Le prix n'est émis que pour un format mensuel
  explicite ; un prix annuel ou inconnu ne produit aucune série.
- **Sévérité Cursor/Devin** : ces barres conservent l'`AbsoluteLevel` historique.
  `elapsed_ratio` et `dry_days` peuvent être présents en complément, mais ne
  modifient jamais cette sévérité.
- **Rolling 5h OpenCode** : pas d'`elapsed_ratio` ni de `dry_days` par
  construction ; seule la sévérité absolue est publiée.
- **Labels** : les suffixes OpenCode sont normalisés en minuscules. Un
  `OPENCODE_GO_API_KEY_MAIN` entre en collision avec la clé de base et fait
  échouer le démarrage (séries dupliquées interdites).
