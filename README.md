# ricochet

Proxy HTTP/HTTPS local, en un seul binaire Go, pour déboguer des outils shell
(`curl`, `git`, `kubectl`, `helm`…).

- **Override FQDN → IP** : certains noms sont envoyés directement vers une IP
  forcée, sans modifier l'URL, le header `Host` ni le SNI (seule l'adresse TCP
  composée change).
- **Chaînage amont** : tout le reste passe par un proxy HTTP amont
  (`cntlm`, `go-ntlm`…), en HTTP clair comme en `CONNECT`.
- HTTPS en **tunnel pur** (pas de déchiffrement).

> État : phase 1 (routage). Dashboard, timings et MITM arrivent dans les
> phases suivantes (voir `CLAUDE.md`).

## Compilation

```sh
CGO_ENABLED=0 go build -o ricochet ./cmd/ricochet
# cross-compilation, par exemple :
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ricochet.exe ./cmd/ricochet
```

Seule dépendance directe : `github.com/elazarl/goproxy`.

## Utilisation

```sh
./ricochet --rules rules.txt --upstream http://127.0.0.1:3128

curl -x http://127.0.0.1:8080 https://api.exemple.com/
HTTPS_PROXY=http://127.0.0.1:8080 HTTP_PROXY=http://127.0.0.1:8080 kubectl get pods
```

| Flag | Défaut | Description |
|---|---|---|
| `--listen` | `127.0.0.1:8080` | adresse d'écoute du proxy |
| `--rules` | *(aucun)* | fichier de règles FQDN → IP |
| `--upstream` | *(direct)* | proxy amont `http://host:port` |
| `--upstream-auth` | | `user:pass` pour l'amont (ou env `RICOCHET_UPSTREAM_AUTH`) |
| `--bypass` | | hosts qui ignorent l'amont : `a.com,*.b.com` |
| `--verbose` | `false` | logs détaillés (décision de routage par requête) |

Les credentials amont ne sont jamais affichés dans les logs.

## Fichier de règles

```
# commentaire
10.1.2.3             api.exemple.com
10.1.2.4             *.interne.exemple.com     # sous-domaines uniquement
10.1.2.5:8443        svc.exemple.com           # override du port
2001:db8::1          v6.exemple.com
[2001:db8::2]:9443   v6port.exemple.com
```

- Un match exact est prioritaire sur un wildcard ; entre wildcards, le plus
  spécifique gagne. `*.domaine` ne couvre pas `domaine` lui-même.
- Insensible à la casse ; plusieurs hosts possibles par ligne.
- Un host défini deux fois est une erreur.

## Routage

| Situation | Destination |
|---|---|
| règle trouvée | `ip:port` de la règle (port d'origine conservé sauf override), l'amont est ignoré |
| host dans `--bypass` | connexion directe |
| `--upstream` défini | proxy amont (`CONNECT host:port` pour HTTPS) |
| sinon | connexion directe |

Les connexions HTTP clair sont réutilisées (keep-alive) par host demandé :
deux hosts overridés différents n'utilisent jamais la même connexion. Une
connexion déjà ouverte garde l'IP résolue au moment de son ouverture.

## Tests

```sh
go vet ./... && go test ./...
```
