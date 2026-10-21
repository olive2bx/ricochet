# ricochet

Proxy HTTP/HTTPS local en Go pour déboguer des outils shell (curl, git, kubectl, helm, etc.).

## Objectif

1. **Override FQDN → IP** : pour certains noms de domaine, router la connexion TCP directement vers une IP configurée, sans toucher à l'URL, au header `Host` ni au SNI.
2. **Chaînage amont** : pour tous les autres FQDN, relayer vers un proxy HTTP amont (ex. `go-ntlm`, `cntlm`, etc.).
3. **Dashboard web** : voir les requêtes en temps réel et mesurer leurs durées (par phase quand c'est possible).

## Contraintes

- Poste de travail restreint : **aucune installation système**. Tout doit tenir dans **un seul binaire Go** compilé localement.
- Les dépendances Go passent par un **proxy Artifactory** (ne pas supposer l'accès direct à `proxy.golang.org`, `sum.golang.org` ou GitHub). Garder la liste de dépendances **courte** et justifiée.
- Pas de CGO (cross-compilation simple). Pas de build front (pas de Node/npm) : le dashboard est du HTML/JS/CSS vanilla embarqué via `go:embed`.
- Module : `github.com/olive2bx/ricochet`. Go : version stable récente, à fixer dans `go.mod`.
- Cibles : Linux, macOS, Windows (amd64/arm64) si possible, sans code spécifique à l'OS.

## Dépendances autorisées

| Lib | Usage |
|---|---|
| `github.com/elazarl/goproxy` | Cœur du proxy, CONNECT, MITM optionnel, hooks `OnRequest`/`OnResponse` |
| `github.com/fsnotify/fsnotify` | Rechargement à chaud du fichier de règles (fallback polling du mtime si échec) |
| `modernc.org/sqlite` | **Optionnel, phase 4** : historique persistant (pur Go) |

Tout ajout de dépendance doit être justifié dans la PR/commit. Privilégier la stdlib.

## Architecture

```
Outil shell ──► listener :8080 (goproxy)
                  ├─ CONNECT (HTTPS) ─► tunnel TCP   (ou MITM si activé pour ce host)
                  └─ HTTP clair / MITM ─► http.Transport instrumenté
                          │
                  Routeur : resolve(host) ?
                     ├─ règle trouvée ─► dial direct vers l'IP forcée
                     └─ sinon ─► proxy amont
                          │
                  Collecteur ─► ring buffer (+ SQLite optionnel) ─► SSE ─► Dashboard (:8081)
```

### Structure du repo

```
ricochet/
├── CLAUDE.md
├── README.md
├── go.mod
├── cmd/ricochet/main.go      # flags, wiring, signaux
├── internal/
│   ├── rules/                # parsing + matching + reload
│   ├── proxy/                # goproxy setup, dialers, chaînage amont, MITM
│   ├── collector/            # modèle d'événements, ring buffer, pub/sub SSE
│   └── dashboard/            # serveur HTTP (API + SSE) + web/ (go:embed)
│       └── web/index.html
└── testdata/
```

## Spécifications fonctionnelles

### 1. Fichier de règles

Format texte inspiré de `/etc/hosts`, un fichier unique (`--rules rules.txt`) :

```
# commentaire
10.1.2.3      api.exemple.com
10.1.2.4      *.interne.exemple.com
10.1.2.5:8443 svc.exemple.com        # optionnel : override du port de destination
```

- Match exact prioritaire sur wildcard ; wildcard `*.domaine` = tout sous-domaine (pas le domaine nu).
- Insensible à la casse, ignorer les lignes vides et commentaires (`#`).
- IPv4 et IPv6 supportés.
- Rechargement à chaud sans redémarrer ni couper les connexions en cours. En cas de fichier invalide : **conserver les anciennes règles** et logger l'erreur (et l'afficher dans le dashboard).
- Une seule fonction `Resolve(host string) (ip netip.Addr, port uint16, ok bool)` utilisée par **tous** les chemins (HTTP clair, CONNECT, MITM).

### 2. Routage

- **Règle trouvée** : dialer directement `ip:port` (le port d'origine est conservé sauf override). L'amont est ignoré pour ce host. Le `Host` header, l'URL et le SNI ne changent pas.
- **Pas de règle** : passer par l'amont `--upstream http://host:port` :
  - HTTP clair : `Transport.Proxy` vers l'amont.
  - HTTPS (CONNECT) : envoyer `CONNECT host:port` à l'amont (avec `Proxy-Authorization` si `--upstream-auth` est fourni), lire la réponse, puis tunneliser. Utiliser `ConnectDial` de goproxy.
- Si `--upstream` est absent : connexion directe (pratique pour tester les règles seules).
- Respecter `NO_PROXY`-like via `--bypass` (liste de hosts qui vont en direct sans passer par l'amont) — nice-to-have.

### 3. HTTPS : tunnel par défaut, MITM à la demande

- **Par défaut : tunnel pur** (pas de déchiffrement). On enregistre : host, port, route choisie, durée du dial, TTFB approximatif, octets up/down, durée totale, erreur éventuelle.
- **MITM optionnel** : `--mitm` (tous les hosts) ou `--mitm-hosts "*.exemple.com,api.foo.com"`.
  - Génère une CA locale au premier lancement dans `--ca-dir` (défaut `./.ricochet-ca/`, permissions 0600 pour la clé).
  - Ne **jamais** installer la CA dans le store système. Afficher au démarrage la commande à utiliser, par exemple :
    `SSL_CERT_FILE=./.ricochet-ca/ca.pem curl -x http://localhost:8080 https://...`
  - Avec MITM : méthode, chemin, statut, headers (avec masquage par défaut de `Authorization`, `Cookie`, `Set-Cookie`), taille, timings par phase.
- Les corps de requêtes/réponses **ne sont pas stockés** par défaut (option `--capture-bodies` avec limite de taille, phase 4).

### 4. Mesures (par requête / tunnel)

Modèle d'événement minimal :

```go
type Event struct {
    ID         uint64
    StartedAt  time.Time
    ClientAddr string
    Scheme     string // http | https-tunnel | https-mitm
    Host       string
    Port       uint16
    Method     string // vide en tunnel
    Path       string // vide en tunnel
    Status     int    // 0 en tunnel
    Route      string // "override:10.1.2.3" | "upstream:<addr>" | "direct"
    RuleMatch  string // ligne de règle appliquée, si applicable
    // Durées
    DNS, Connect, TLS, TTFB, Total time.Duration
    BytesUp, BytesDown int64
    Err        string
}
```

- HTTP clair et MITM : `net/http/httptrace` (DNS, connect, TLS handshake, first byte, total).
- Tunnel : dial time, délai premier octet serveur après premier octet client, total, octets.
- Horloge monotone (`time.Since`), pas de `time.Now()` pour calculer des durées.

### 5. Dashboard

- Servi sur `--dashboard 127.0.0.1:8081` (**loopback par défaut**, jamais `0.0.0.0` sans flag explicite).
- HTML/JS vanilla embarqué (`go:embed`), pas de CDN externe (poste restreint, hors-ligne possible).
- Endpoints :
  - `GET /` : UI
  - `GET /api/requests?limit=&host=&min_ms=` : historique JSON
  - `GET /events` : flux SSE temps réel
  - `GET /api/rules` : règles actives + date de dernier chargement + dernière erreur
  - `POST /api/clear` : vider l'historique
- UI : tableau live (heure, scheme, host, méthode/chemin, statut, route, durée totale, octets), tri par colonne (surtout durée), filtres (texte, route, erreurs, durée min), panneau de détail avec **barres de timing par phase** (DNS / connect / TLS / TTFB / transfert), badge visuel pour les requêtes overridées vs amont, bouton pause et clear.
- Ring buffer en mémoire (`--history 5000` par défaut).

## Flags CLI

```
--listen       127.0.0.1:8080     adresse du proxy
--dashboard    127.0.0.1:8081     adresse du dashboard ("" pour désactiver)
--rules        rules.txt          fichier de règles FQDN→IP
--upstream     http://host:port   proxy amont (optionnel)
--upstream-auth user:pass         credentials amont (optionnel, aussi via env RICOCHET_UPSTREAM_AUTH)
--bypass       a.com,*.b.com      hosts qui ignorent l'amont
--mitm                            MITM sur tous les hosts
--mitm-hosts   ...                MITM sur ces hosts uniquement
--ca-dir       ./.ricochet-ca     dossier de la CA
--history      5000               taille du ring buffer
--verbose                         logs détaillés
```

## Plan d'implémentation (par phases, un commit par phase)

1. **Squelette + routage** : `go.mod`, flags, goproxy en tunnel pur, package `rules` (parsing, matching, tests), override FQDN→IP, chaînage amont HTTP + CONNECT.
2. **Collecteur + dashboard** : modèle d'événements, ring buffer, SSE, UI vanilla avec tableau live et tri par durée.
3. **Timings détaillés + MITM** : `httptrace`, CA auto-générée, MITM par host, masquage des headers sensibles, vue détail avec phases.
4. **Finitions** : rechargement à chaud (fsnotify), persistance SQLite optionnelle, `--capture-bodies`, export HAR/JSON, README.

## Critères d'acceptation

- [x] `go build ./...` et `go vet ./...` passent ; `go test ./...` couvre au minimum `rules` (exact, wildcard, priorité, casse, port override, fichier invalide) et le routeur.
- [x] `curl -x http://localhost:8080 http://api.exemple.com/` avec une règle → la connexion part vers l'IP forcée, le header `Host` reste `api.exemple.com` (vérifiable avec un serveur de test local).
- [x] Même chose en HTTPS (tunnel) : le SNI reste `api.exemple.com`, le certificat est celui du vrai host.
- [x] Un host sans règle passe par l'amont (vérifiable avec un faux proxy amont de test qui logge les `CONNECT` et requêtes reçus).
- [x] Modifier `rules.txt` à chaud est pris en compte en moins de 2 s sans couper les connexions actives ; un fichier invalide ne casse pas les règles courantes.
- [x] Le dashboard affiche les requêtes en temps réel, triables par durée, avec le détail des phases pour HTTP clair et MITM.
- [x] Mode MITM : aucune modification du store de certificats système ; la CA est utilisable via `SSL_CERT_FILE` / `--cacert`.
- [x] Le binaire démarre sans aucun accès réseau externe (dashboard 100 % embarqué).

## Points d'attention

- **Ne pas réécrire l'URL** pour appliquer un override : seul `DialContext` change l'adresse cible. Sinon SNI et `Host` seraient faux.
- Gérer proprement `http.Transport` : désactiver la réutilisation de connexions entre hosts overridés différents si cela fausse les mesures (ou documenter le comportement). Pour les timings par requête, privilégier des connexions fraîches avec un flag `--no-keepalive` si besoin.
- Attention aux fuites de goroutines et de connexions dans le tunnel (fermeture des deux côtés, `CloseWrite` quand disponible).
- Les credentials amont ne doivent **jamais** apparaître dans les logs ni dans le dashboard.
- Dashboard et proxy écoutent sur loopback par défaut : l'outil expose du trafic potentiellement sensible.
- Si `go mod download` échoue à cause d'Artifactory : vérifier `GOPROXY`, `GONOSUMDB` / `GONOSUMCHECK` / `GOFLAGS=-mod=mod`, et ne pas modifier ces réglages dans le repo (config locale à l'environnement).

## État

Phases 1 à 4 implémentées. SQLite est derrière le build tag `sqlite`
(`go build -tags sqlite`) pour garder le binaire par défaut léger ; tester
avec `go test ./...` et `go test -tags sqlite ./...`, ou `make ci` (mêmes
vérifications que la CI GitHub Actions : `.github/workflows/ci.yml`).
Release : pousser un tag `v*` (`.github/workflows/release.yml`).

## Conventions

- Code et identifiants en anglais, commentaires concis, README en français.
- Erreurs wrappées (`fmt.Errorf("...: %w", err)`), pas de `panic` hors `main`.
- `context.Context` propagé partout où il y a du réseau ; arrêt propre sur `SIGINT`/`SIGTERM`.
- Tests table-driven, serveurs de test via `httptest` / `net.Listen("tcp", "127.0.0.1:0")`.
- `gofmt` + `go vet` avant chaque commit ; messages de commit style conventional commits.
