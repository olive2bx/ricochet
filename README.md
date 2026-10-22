# ricochet

Proxy HTTP/HTTPS local, en un seul binaire Go, pour déboguer des outils shell
(`curl`, `git`, `kubectl`, `helm`…).

- **Override FQDN → IP** : certains noms sont envoyés directement vers une IP
  forcée, sans modifier l'URL, le header `Host` ni le SNI (seule l'adresse TCP
  composée change).
- **Chaînage amont** : tout le reste passe par un proxy HTTP amont
  (`cntlm`, `go-ntlm`…), en HTTP clair comme en `CONNECT`, avec
  authentification Basic optionnelle.
- **Dashboard web** temps réel (embarqué, aucun CDN) : durées par phase, tri,
  filtres, détail, export HAR/JSON.
- HTTPS en **tunnel pur** par défaut, **MITM** à la demande avec une CA locale
  jamais installée dans le système.
- Règles **rechargées à chaud**, historique **persistant** optionnel (SQLite
  pur Go).

## Compilation

Aucune dépendance système, pas de CGO. Il faut **Go ≥ 1.26** (fixé dans
`go.mod` ; c'est aussi le minimum requis par `modernc.org/sqlite`). La CI teste
cette version minimale et la dernière stable, qui sert à compiler les binaires
de release. Avec un Go plus ancien, la toolchain est téléchargée
automatiquement via `GOPROXY` (`GOTOOLCHAIN=auto`) : derrière Artifactory, le
module `golang.org/toolchain` doit donc être accessible, sinon installez un Go
1.26 ou plus récent.

```sh
CGO_ENABLED=0 go build -o ricochet ./cmd/ricochet

# avec l'historique persistant SQLite (binaire plus gros : ~17 Mo au lieu de ~11 Mo)
CGO_ENABLED=0 go build -tags sqlite -o ricochet ./cmd/ricochet

# cross-compilation (linux|darwin|windows × amd64|arm64)
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ricochet.exe ./cmd/ricochet

# version affichée par --version et dans les exports HAR
go build -ldflags "-X main.version=1.0.0" -o ricochet ./cmd/ricochet
```

Dépendances directes :

| Module | Rôle |
|---|---|
| `github.com/elazarl/goproxy` | cœur du proxy, `CONNECT`, MITM |
| `github.com/fsnotify/fsnotify` | rechargement à chaud des règles (repli : polling du mtime) |
| `modernc.org/sqlite` | historique persistant, compilé uniquement avec `-tags sqlite` |

Sans le tag `sqlite`, le code SQLite n'est pas compilé (seuls les `go.mod` des
modules sont lus). Derrière un proxy Artifactory, configurez `GOPROXY` /
`GONOSUMDB` dans votre environnement, pas dans le dépôt.

## Démarrage rapide

```sh
./ricochet --rules rules.txt --upstream http://127.0.0.1:3128
# dashboard : http://127.0.0.1:8081/

curl -x http://127.0.0.1:8080 https://api.exemple.com/
HTTPS_PROXY=http://127.0.0.1:8080 HTTP_PROXY=http://127.0.0.1:8080 kubectl get pods
git -c http.proxy=http://127.0.0.1:8080 fetch
```

## Activation dans le terminal (scripts)

Pour un usage quotidien, les scripts `ricochet-on` / `ricochet-off` évitent de
lancer ricochet et de changer les variables proxy à la main.

### Installation

```sh
make install
```

Le menu demande le dossier d'installation (`~/.ricochet` par défaut) et la
variante (standard ou SQLite). La plateforme (OS et architecture) est détectée
automatiquement. Il compile le binaire et copie les quatre scripts dans ce
dossier. Sans question : `make install INSTALL_DIR=~/.ricochet VARIANT=sqlite`.

### Utilisation

| | bash / zsh (Linux, macOS, Git Bash) | PowerShell (Windows) |
|---|---|---|
| activer | `. ~/.ricochet/ricochet-on.sh` | `& "$HOME\.ricochet\ricochet-on.ps1"` |
| désactiver | `. ~/.ricochet/ricochet-off.sh` | `& "$HOME\.ricochet\ricochet-off.ps1"` |

En bash/zsh, les scripts doivent être **sourcés** (le `.` initial) : un script
lancé normalement ne peut pas modifier les variables du terminal. Ils le
détectent et l'indiquent. Des alias pratiques, affichés par `make install` :

```sh
alias ricochet-on='. ~/.ricochet/ricochet-on.sh'
alias ricochet-off='. ~/.ricochet/ricochet-off.sh'
```

`ricochet-on` :

1. mémorise `http_proxy`, `https_proxy`, `HTTP_PROXY` et `HTTPS_PROXY` ;
2. démarre ricochet en arrière-plan sur `127.0.0.1:3129` (dashboard sur
   `127.0.0.1:3130`) avec le proxy d'origine comme amont (`https_proxy` en
   priorité, puis `http_proxy`) et les règles de `ricochet-rules.txt` (créé
   s'il n'existe pas, rechargé à chaud) ;
3. fait pointer les quatre variables sur ricochet et affiche un résumé
   (pid, dashboard, amont avec identifiants masqués, nombre de règles, logs).

`ricochet-off` arrête le processus et restaure exactement les variables
d'origine (y compris celles qui n'étaient pas définies).

Une instance déjà lancée est détectée en interrogeant son dashboard
(`ricochet --ping`), et non par le seul fichier pid : elle est réutilisée
quel que soit le terminal qui l'a lancée (bash, zsh, Git Bash, PowerShell, ou
à la main). Si son amont diffère de celui du terminal, un avertissement
l'indique. Si le port est pris par un autre programme, le message le dit.
L'arrêt passe par `ricochet --stop` (arrêt propre, y compris sous Windows) et
coupe ricochet pour tous les terminaux.

### Options d'activation (MITM, corps, …)

Les options de ricochet se passent directement au script :

```sh
ricochet-on --mitm-hosts '*.exemple.com'             # alias bash/zsh
ricochet-on --mitm --capture-bodies 4096
ricochet-on --restart --mitm                         # relance pour appliquer
ricochet-on --help
```

```powershell
& "$HOME\.ricochet\ricochet-on.ps1" -MitmHosts '*.exemple.com' -CaptureBodies 4096
& "$HOME\.ricochet\ricochet-on.ps1" -Restart -Mitm --verbose   # options brutes acceptées aussi
Get-Help "$HOME\.ricochet\ricochet-on.ps1" -Detailed
```

| Script | PowerShell | Effet |
|---|---|---|
| `--restart` | `-Restart` | relance ricochet s'il tourne déjà (sinon les nouvelles options sont ignorées, avec un avertissement) |
| `--mitm` | `-Mitm` | déchiffre tout le HTTPS |
| `--mitm-hosts H,…` | `-MitmHosts H,…` | déchiffre seulement ces hosts |
| `--capture-bodies N` | `-CaptureBodies N` | conserve jusqu'à N octets des corps |
| `--bypass H,…` | `-Bypass H,…` | hosts qui ne passent pas par l'amont |
| `--no-keepalive` | `-NoKeepAlive` | timings DNS/connexion/TLS à chaque requête |
| toute autre option de ricochet | idem (ex. `--verbose`) | transmise telle quelle |

`--listen`, `--dashboard`, `--rules` et `--upstream` sont gérés par le script
(variables ci-dessous) et refusés ; `--upstream-auth` aussi, car il serait
visible dans la liste des processus.

Avec le MITM, la CA est créée dans le dossier d'installation (`ca/ca.pem`) et
le résumé affiche la commande à utiliser (`curl --cacert …` ou
`SSL_CERT_FILE=…`), y compris dans un autre terminal qui réutilise l'instance.

Variables facultatives : `RICOCHET_PORT`, `RICOCHET_UI_PORT`, `RICOCHET_RULES`
(chemin du fichier de règles), `RICOCHET_BIN`, `RICOCHET_ARGS` (options
supplémentaires, ex. `--mitm-hosts '*.exemple.com'`). Le pid et les logs sont
dans le dossier d'installation (`ricochet.pid`, `ricochet.log`).

L'amont est transmis via la variable `RICOCHET_UPSTREAM` et non en argument :
les identifiants d'un proxy `http://user:pass@hôte:port` n'apparaissent ni
dans la liste des processus, ni dans les logs, ni dans le dashboard.

Sous Windows, si la stratégie d'exécution bloque les scripts :
`Set-ExecutionPolicy -Scope Process Bypass` (pour la session courante
uniquement).

## Options

| Flag | Défaut | Description |
|---|---|---|
| `--listen` | `127.0.0.1:8080` | adresse d'écoute du proxy |
| `--dashboard` | `127.0.0.1:8081` | adresse du dashboard (`""` pour le désactiver) |
| `--rules` | *(aucun)* | fichier de règles FQDN → IP, rechargé à chaud |
| `--upstream` | *(direct)* | proxy amont `http://host:port` (ou env `RICOCHET_UPSTREAM`) |
| `--upstream-auth` | | `user:pass` pour l'amont (ou env `RICOCHET_UPSTREAM_AUTH`) |
| `--bypass` | | hosts qui ignorent l'amont : `a.com,*.b.com` |
| `--mitm` | `false` | déchiffre toutes les connexions HTTPS |
| `--mitm-hosts` | | déchiffre seulement ces hosts : `*.exemple.com,api.foo.com` |
| `--ca-dir` | `./.ricochet-ca` | dossier de la CA MITM (créée au premier lancement) |
| `--mask-headers` | `Authorization,Proxy-Authorization,Cookie,Set-Cookie` | headers masqués dans le dashboard (`""` = rien) |
| `--capture-bodies` | `0` | conserve jusqu'à N octets de chaque corps (HTTP clair et MITM) |
| `--history` | `5000` | taille de l'historique en mémoire |
| `--db` | | fichier SQLite d'historique persistant (binaire `-tags sqlite`) |
| `--db-max-rows` | `100000` | nombre max d'événements gardés en base |
| `--no-keepalive` | `false` | pas de réutilisation de connexion : DNS/connexion/TLS mesurés à chaque requête |
| `--verbose` | `false` | logs détaillés (décision de routage par requête) |
| `--version` | | affiche la version |
| `--ping` | | indique si un ricochet répond sur `--dashboard` (code 0, affiche `pid=…`, `upstream=…`, `mitm=…`, `ca=…`) ou non (code 1) |
| `--stop` | | arrête proprement le ricochet dont le dashboard est `--dashboard` (code 0) ; code 1 s'il ne tourne pas |

Les credentials amont ne sont jamais affichés : ni dans les logs, ni dans le
dashboard, ni dans les exports (le header `Proxy-Authorization` est ajouté
par ricochet vers l'amont, il n'apparaît pas dans les événements).

## Fichier de règles

```
# commentaire
10.1.2.3             api.exemple.com
10.1.2.4             *.interne.exemple.com     # sous-domaines uniquement
10.1.2.5:8443        svc.exemple.com           # override du port
2001:db8::1          v6.exemple.com
[2001:db8::2]:9443   v6port.exemple.com
10.1.2.6             a.exemple.com b.exemple.com   # plusieurs hosts par ligne
```

- Un match exact est prioritaire sur un wildcard ; entre wildcards, le plus
  spécifique gagne. `*.domaine` ne couvre pas `domaine` lui-même.
- Insensible à la casse. Un host défini deux fois est une erreur.
- **Rechargement à chaud** (en général < 1 s) : le dossier du fichier est
  surveillé, les sauvegardes atomiques des éditeurs sont gérées. Les
  connexions en cours ne sont pas coupées ; les connexions keep-alive inactives
  sont fermées pour que les nouvelles requêtes suivent les nouvelles règles.
- **Fichier invalide** : les règles précédentes sont conservées, l'erreur est
  loggée et affichée en rouge dans l'en-tête du dashboard.

Exemple : [`testdata/rules.example.txt`](testdata/rules.example.txt).

## Routage

| Situation | Destination |
|---|---|
| règle trouvée | `ip:port` de la règle (port d'origine conservé sauf override), l'amont est ignoré |
| host dans `--bypass` | connexion directe |
| `--upstream` défini | proxy amont (`CONNECT host:port` pour HTTPS) |
| sinon | connexion directe |

La même résolution s'applique à tous les chemins : HTTP clair, tunnel
`CONNECT` et MITM.

## HTTPS : tunnel ou MITM

Par défaut, les connexions HTTPS sont relayées sans déchiffrement. Le
dashboard montre alors : host, port, route, durée de connexion, attente du
`CONNECT` amont, délai entre le premier octet du client et le premier octet du
serveur (TTFB approché), octets montants/descendants, durée totale et erreur
éventuelle.

Avec `--mitm` ou `--mitm-hosts`, ricochet génère une CA locale dans `--ca-dir`
(clé en `0600`) et présente aux clients des certificats signés par elle. **La
CA n'est jamais installée dans le magasin système** : on la fournit commande
par commande.

```sh
./ricochet --mitm-hosts '*.exemple.com'

curl --cacert ./.ricochet-ca/ca.pem -x http://127.0.0.1:8080 https://api.exemple.com/
SSL_CERT_FILE=./.ricochet-ca/ca.pem curl -x http://127.0.0.1:8080 https://api.exemple.com/
```

`SSL_CERT_FILE` / `--cacert` **remplacent** le magasin de confiance : si
certains hosts restent en tunnel (cas de `--mitm-hosts`), leurs vrais
certificats ne seront plus reconnus. Il faut alors un bundle combiné, par
exemple :

```sh
cat /etc/ssl/certs/ca-certificates.crt ./.ricochet-ca/ca.pem > /tmp/bundle.pem
SSL_CERT_FILE=/tmp/bundle.pem git fetch
```

Côté serveur, ricochet vérifie les certificats avec le magasin système (Go
respecte `SSL_CERT_FILE` sous Linux). Les requêtes déchiffrées remontent avec
méthode, chemin, statut, headers (sensibles masqués) et timings détaillés.

## Dashboard

`http://127.0.0.1:8081/` (loopback par défaut, avertissement au démarrage
sinon) :

- tableau temps réel (SSE) : heure, type, host, méthode/chemin, statut, route,
  durée, octets ; requêtes en cours affichées avec leur durée qui avance ;
- tri sur chaque colonne, filtres texte / route / type / erreurs / durée min. ;
- panneau de détail : **barres de timing par phase** (attente, DNS, connexion,
  TLS, TTFB, transfert), règle appliquée, headers, corps capturés ;
- badges override (violet) / amont (bleu) / direct ; pause et vidage ;
- export **HAR 1.2** (ouvrable dans les DevTools Chrome/Firefox) et JSON.

Phases mesurées (`net/http/httptrace`, horloge monotone) :

| Phase | HTTP clair / MITM | Tunnel |
|---|---|---|
| Attente | file d'attente du pool + handshake `CONNECT` amont | handshake `CONNECT` amont |
| DNS | résolution | résolution |
| Connexion | TCP | TCP |
| TLS | handshake vers le serveur | — (chiffré de bout en bout) |
| TTFB | connexion obtenue → premier octet de réponse | 1er octet client → 1er octet serveur |
| Transfert | reste du total | durée de vie restante du tunnel |

Une connexion keep-alive réutilisée n'a pas de DNS/connexion/TLS (signalé dans
le détail) ; `--no-keepalive` force des connexions neuves.

### API

| Endpoint | Description |
|---|---|
| `GET /api/requests?limit=&host=&min_ms=` | historique (sans les corps) |
| `GET /api/requests/{id}` | un événement complet (avec corps) |
| `GET /events` | flux SSE (`event: event` / `event: clear`) |
| `GET /api/rules` | règles actives, date de chargement, dernière erreur |
| `GET /api/status` | identifie l'instance : `app`, `pid`, `version`, `proxy`, `upstream`, `mitm`, `ca` |
| `POST /api/shutdown` | arrêt propre ; exige le header `X-Ricochet-Control: 1` (impossible depuis une page web) |
| `GET /api/export?format=har\|json` | export de l'historique |
| `POST /api/clear` | vide l'historique (mémoire et SQLite) |

Le dashboard refuse les requêtes dont le `Host` n'est pas une adresse loopback
(protection contre le DNS rebinding) et les `POST` d'une autre origine.

## Makefile

Le `Makefile` ne fait qu'appeler l'outil `go` (les commandes `go` ci-dessus
restent utilisables sans `make`, par exemple sous Windows). `make help` liste
les cibles :

| Cible | Rôle |
|---|---|
| `make` / `make build` | binaire `./ricochet` (sans SQLite) |
| `make build-sqlite` | binaire avec l'historique SQLite |
| `make run ARGS="--rules rules.txt"` | compile et lance |
| `make test` / `make test-sqlite` / `make test-race` | tests (`test-race` nécessite CGO) |
| `make fmt` / `make vet` / `make lint` / `make vuln` | gofmt, go vet, staticcheck, govulncheck |
| `make ci` | toutes les vérifications de la CI (format, `go mod tidy`, vet, staticcheck, tests) |
| `make dist` | binaires de toutes les plateformes, avec et sans SQLite, + `SHA256SUMS` dans `dist/` |
| `make install` | menu interactif : installe le binaire et les scripts `ricochet-on` / `ricochet-off` |
| `make test-scripts` | teste les scripts (bash, zsh et pwsh s'ils sont présents, shellcheck) |

La version embarquée vient de `git describe` (surcharge : `make build VERSION=1.2.3`).
staticcheck et govulncheck sont lancés via `go run` à une version figée :
rien à installer, mais ils doivent être téléchargeables via `GOPROXY`.

## CI/CD (GitHub Actions)

- **`ci.yml`**, sur chaque PR et push sur `main` :
  - lint : gofmt, `go mod tidy`, `go vet`, staticcheck ;
  - tests sur Linux, macOS et Windows, avec la version Go de `go.mod` et la
    dernière stable, avec et sans `-tags sqlite`, et le détecteur de courses
    hors Windows ;
  - govulncheck ;
  - cross-compilation de toutes les cibles, binaires publiés en artefact.
- **`release.yml`** : vérifications, puis release GitHub avec les 12 binaires
  (6 plateformes × avec/sans SQLite) et `SHA256SUMS`. Trois façons de la
  déclencher :
  - depuis un terminal, en poussant un tag :

    ```sh
    git tag v0.1.0 && git push origin v0.1.0
    ```
  - depuis l'interface GitHub (utilisable sur mobile) : **Actions → Release →
    Run workflow**, saisir la version (`v0.1.0`) ; le tag est créé sur la
    branche choisie (`main` par défaut) ;
  - depuis la page **Releases** : une release publiée à la main (qui crée le
    tag) reçoit automatiquement les binaires.
- **Dependabot** : mises à jour hebdomadaires des modules Go et mensuelles des
  actions.

## Tests

```sh
go vet ./... && go test ./...
go test -tags sqlite ./...    # inclut l'historique SQLite
```

Les tests couvrent les règles (exact, wildcard, priorité, casse, port, IPv6,
fichier invalide, rechargement à chaud), le routeur et des scénarios de bout
en bout avec des serveurs locaux : override HTTP et HTTPS (vérification du
`Host` et du SNI), chaînage via un faux proxy amont qui enregistre les
`CONNECT` et l'authentification, MITM, mesures, capture des corps, dashboard
(API, SSE, protections) et export HAR.

Les scripts d'activation ont leurs propres tests (`make test-scripts`) :
démarrage, variables, amont, masquage des identifiants, idempotence, arrêt et
restauration, en bash, zsh, PowerShell 7 et Windows PowerShell 5.1 (CI).
