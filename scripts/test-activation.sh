#!/usr/bin/env bash
# Test fonctionnel de ricochet-on.sh / ricochet-off.sh.
#   scripts/test-activation.sh [bash|zsh]
# Compile ricochet dans un dossier temporaire, puis vérifie dans le shell
# demandé : démarrage, variables proxy, amont, masquage des identifiants,
# idempotence, arrêt et restauration.
set -eu

shell=${1:-bash}
repo=$(cd "$(dirname "$0")/.." && pwd -P)
tmp=$(mktemp -d)
cleanup() {
	if [ -f "$tmp/ricochet.pid" ]; then kill "$(cat "$tmp/ricochet.pid")" 2>/dev/null || true; fi
	rm -rf "$tmp"
}
trap cleanup EXIT

(cd "$repo" && go build -o "$tmp/ricochet" ./cmd/ricochet)
cp "$repo/scripts/ricochet-on.sh" "$repo/scripts/ricochet-off.sh" "$tmp/"

# Lancé directement (non sourcé) : refus explicite.
if "$shell" "$tmp/ricochet-on.sh" 2>"$tmp/err"; then
	echo "FAIL: ricochet-on.sh exécuté sans être sourcé aurait dû échouer"
	exit 1
fi
grep -q 'doit être sourcé' "$tmp/err" || {
	echo "FAIL: message attendu absent"
	cat "$tmp/err"
	exit 1
}

cat >"$tmp/driver" <<'EOF'
fail() { echo "FAIL: $*"; exit 1; }
# gone PID : attend (5 s max) que le processus ait terminé ; un zombie pas
# encore récupéré par son parent compte comme terminé.
gone() {
	i=0
	while kill -0 "$1" 2>/dev/null; do
		case $(ps -o stat= -p "$1" 2>/dev/null) in Z*) return 0 ;; esac
		i=$((i + 1))
		[ $i -lt 50 ] || return 1
		sleep 0.1
	done
}
# Chemin résolu (macOS : /var -> /private/var), comme le fait ricochet-on.
dir=$(cd "$1" && pwd -P)
# Sinon bash transmet les arguments de ce script à ricochet-on, sourcé sans arguments.
set --
export RICOCHET_PORT=39129 RICOCHET_UI_PORT=39130
export http_proxy='http://alice:s3cret@127.0.0.1:9' https_proxy='http://alice:s3cret@127.0.0.1:9'
unset HTTP_PROXY HTTPS_PROXY no_proxy NO_PROXY

. "$dir/ricochet-on.sh" || fail "ricochet-on a échoué"
want=http://127.0.0.1:39129
for v in http_proxy https_proxy HTTP_PROXY HTTPS_PROXY; do
	eval "val=\${$v:-}"
	[ "$val" = "$want" ] || fail "$v=$val, attendu $want"
done
[ -f "$dir/ricochet-rules.txt" ] || fail "fichier de règles non créé"
pid=$(cat "$dir/ricochet.pid")
kill -0 "$pid" || fail "processus absent"

info=$(curl -fsS --noproxy '*' http://127.0.0.1:39130/api/info) || fail "dashboard injoignable"
case $info in *'"amont":"127.0.0.1:9"'*) ;; *) fail "amont inattendu: $info" ;; esac
case $info in *s3cret*) fail "identifiants dans le dashboard" ;; esac
grep -q s3cret "$dir/ricochet.log" && fail "identifiants dans les logs"
ps -o args= -p "$pid" | grep -q s3cret && fail "identifiants dans la liste des processus"

# Deuxième activation : idempotente, l'amont mémorisé est conservé.
out=$(. "$dir/ricochet-on.sh") || fail "seconde activation"
case $out in *'tourne déjà'*) ;; *) fail "message « tourne déjà » absent: $out" ;; esac
. "$dir/ricochet-on.sh" >/dev/null || fail "seconde activation (shell courant)"
[ "$RICOCHET_SAVED_https_proxy" = 'http://alice:s3cret@127.0.0.1:9' ] || fail "amont mémorisé écrasé"
[ "$(cat "$dir/ricochet.pid")" = "$pid" ] || fail "second processus lancé"

. "$dir/ricochet-off.sh" || fail "ricochet-off a échoué"
[ "$http_proxy" = 'http://alice:s3cret@127.0.0.1:9' ] || fail "http_proxy non restauré: $http_proxy"
[ "$https_proxy" = 'http://alice:s3cret@127.0.0.1:9' ] || fail "https_proxy non restauré"
[ -z "${HTTP_PROXY+x}" ] || fail "HTTP_PROXY aurait dû être supprimée"
[ -z "${RICOCHET_SAVED_VARS+x}" ] || fail "état non nettoyé"
gone "$pid" || fail "processus toujours actif"
[ ! -f "$dir/ricochet.pid" ] || fail "pidfile non supprimé"

# Arrêt sans ricochet actif : message informatif, pas d'erreur.
. "$dir/ricochet-off.sh" | grep "n'était pas" >/dev/null || fail "message d'arrêt à vide absent"

# Instance lancée hors de ce terminal (pas de pidfile) : réutilisée, pas relancée.
"$dir/ricochet" --listen 127.0.0.1:39129 --dashboard 127.0.0.1:39130 --upstream http://127.0.0.1:9 >"$dir/other.log" 2>&1 &
other=$!
i=0
until "$dir/ricochet" --dashboard 127.0.0.1:39130 --ping >/dev/null; do
	i=$((i + 1)); [ $i -lt 50 ] || fail "instance externe non démarrée"; sleep 0.1
done
out=$(. "$dir/ricochet-on.sh" 2>&1) || fail "activation avec instance externe: $out"
case $out in *"tourne déjà (pid $other)"*) ;; *) fail "instance externe non détectée: $out" ;; esac
case $out in *'autre terminal'*) ;; *) fail "message « autre terminal » absent: $out" ;; esac
case $out in *'démarré'*) fail "une seconde instance a été lancée: $out" ;; esac
. "$dir/ricochet-on.sh" >/dev/null 2>&1 || fail "activation (shell courant)"
[ "$http_proxy" = "$want" ] || fail "http_proxy non positionné avec instance externe"
out=$(. "$dir/ricochet-off.sh") || fail "arrêt instance externe"
case $out in *"arrêté (pid $other)"*) ;; *) fail "instance externe non arrêtée: $out" ;; esac
. "$dir/ricochet-off.sh" >/dev/null
gone "$other" || fail "instance externe toujours active"
grep -q 'shutdown requested through the dashboard API' "$dir/other.log" || fail "arrêt non propre"

# Port déjà occupé par un autre programme (ici un ricochet sans dashboard).
"$dir/ricochet" --listen 127.0.0.1:39129 --dashboard '' >/dev/null 2>&1 &
squatter=$!
sleep 0.5
out=$(. "$dir/ricochet-on.sh" 2>&1) && fail "activation aurait dû échouer (port occupé)"
kill "$squatter"
case $out in *'déjà utilisé par un autre programme'*) ;; *) fail "message port occupé absent: $out" ;; esac
[ "$http_proxy" = 'http://alice:s3cret@127.0.0.1:9' ] || fail "variables modifiées malgré l'échec"

# Options : transmises à ricochet, CA dans le dossier d'installation.
out=$(. "$dir/ricochet-on.sh" --listen 1.2.3.4:1 2>&1) && fail "--listen aurait dû être refusé"
case $out in *'géré par le script'*) ;; *) fail "refus --listen: $out" ;; esac
out=$(. "$dir/ricochet-on.sh" stray-value 2>&1) && fail "valeur seule acceptée"
case $out in *'argument inattendu'*) ;; *) fail "refus valeur seule: $out" ;; esac
. "$dir/ricochet-on.sh" --mitm-hosts '*.example.com' --capture-bodies=512 >/dev/null 2>&1 || fail "activation avec options"
st=$("$dir/ricochet" --dashboard 127.0.0.1:39130 --ping) || fail "ping après activation avec options"
case $st in *'mitm=*.example.com'*) ;; *) fail "mitm non transmis: $st" ;; esac
case $st in *"ca=$dir/ca/ca.pem"*) ;; *) fail "CA hors du dossier d'installation: $st" ;; esac
[ -f "$dir/ca/ca.pem" ] || fail "CA non créée"
out=$(. "$dir/ricochet-on.sh" --mitm 2>&1) || fail "seconde activation avec options"
case $out in *'options ignorées'*'--restart --mitm'*) ;; *) fail "avertissement options ignorées absent: $out" ;; esac
first=$(cat "$dir/ricochet.pid")
. "$dir/ricochet-on.sh" --restart --mitm >/dev/null 2>&1 || fail "--restart"
[ "$(cat "$dir/ricochet.pid")" != "$first" ] || fail "--restart n'a pas relancé ricochet"
st=$("$dir/ricochet" --dashboard 127.0.0.1:39130 --ping) || fail "ping après --restart"
case $st in *'mitm=all'*) ;; *) fail "--restart n'a pas appliqué --mitm: $st" ;; esac
. "$dir/ricochet-off.sh" >/dev/null
[ "$http_proxy" = 'http://alice:s3cret@127.0.0.1:9' ] || fail "restauration après --restart"
echo "OK ($0)"
EOF

"$shell" "$tmp/driver" "$tmp"
