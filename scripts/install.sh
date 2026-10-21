#!/bin/sh
# Installation interactive de ricochet et de ses scripts d'activation.
# Appelé par « make install ». Sans question si INSTALL_DIR et VARIANT sont
# fournis (ex. make install INSTALL_DIR=~/.ricochet VARIANT=sqlite).
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd -P)
version=${VERSION:-$(git -C "$repo" describe --tags --always --dirty 2>/dev/null || echo dev)}

if [ -t 1 ]; then
	bold=$(printf '\033[1m') green=$(printf '\033[32m') yellow=$(printf '\033[33m')
	red=$(printf '\033[31m') dim=$(printf '\033[2m') off=$(printf '\033[0m')
else
	bold='' green='' yellow='' red='' dim='' off=''
fi

die() {
	printf '%s✖ %s%s\n' "$red" "$*" "$off" >&2
	exit 1
}

command -v go >/dev/null 2>&1 || die "l'outil go est introuvable dans le PATH"

# Plateforme détectée automatiquement (celle de la machine qui compile).
goos=$(go env GOHOSTOS)
goarch=$(go env GOHOSTARCH)
ext=''
[ "$goos" = windows ] && ext=.exe

interactive=1
if [ -n "${INSTALL_DIR:-}" ] && [ -n "${VARIANT:-}" ]; then interactive=0; fi
if [ $interactive = 1 ] && [ ! -t 0 ]; then
	die "pas de terminal interactif : fournir INSTALL_DIR et VARIANT (make install INSTALL_DIR=... VARIANT=standard|sqlite)"
fi

# ask "question" "défaut" : la réponse (ou le défaut) est dans $answer.
ask() {
	printf '%s%s%s [%s] : ' "$bold" "$1" "$off" "$2"
	read -r answer || answer=''
	answer=${answer:-$2}
}

printf '\n%s=== Installation de ricochet %s ===%s\n' "$bold" "$version" "$off"
printf 'Plateforme détectée : %s%s/%s%s\n\n' "$green" "$goos" "$goarch" "$off"

# 1. Dossier d'installation.
default_dir=${INSTALL_DIR:-$HOME/.ricochet}
if [ $interactive = 1 ]; then
	ask "Dossier d'installation" "$default_dir"
	dir=$answer
else
	dir=$default_dir
fi
case $dir in "~"*) dir=$HOME${dir#"~"} ;; esac

# 2. Variante.
variant=${VARIANT:-}
if [ $interactive = 1 ]; then
	printf '\n%sVariante :%s\n' "$bold" "$off"
	printf '  1) standard  %s— binaire léger (~11 Mo), historique en mémoire%s\n' "$dim" "$off"
	printf '  2) sqlite    %s— historique persistant avec --db (~17 Mo)%s\n' "$dim" "$off"
	ask "Choix" "1"
	case $answer in
	1 | standard) variant=standard ;;
	2 | sqlite) variant=sqlite ;;
	*) die "choix invalide : $answer" ;;
	esac
fi
case $variant in
standard) tags='' ;;
sqlite) tags=sqlite ;;
*) die "variante inconnue : $variant (standard ou sqlite)" ;;
esac

bin=$dir/ricochet$ext
printf '\n%sRécapitulatif :%s\n' "$bold" "$off"
printf '  binaire   : %s (%s, %s/%s)\n' "$bin" "$variant" "$goos" "$goarch"
printf '  scripts   : ricochet-on/off .sh et .ps1\n'
printf '  règles    : %s/ricochet-rules.txt %s(conservé s'"'"'il existe)%s\n' "$dir" "$dim" "$off"
if [ $interactive = 1 ]; then
	ask "Continuer ? (o/n)" "o"
	case $answer in o | O | oui | y | Y | yes) ;; *) die "installation annulée" ;; esac
fi

# Un binaire en cours d'exécution ne peut pas être remplacé sous Windows.
if [ -f "$dir/ricochet.pid" ] && kill -0 "$(cat "$dir/ricochet.pid")" 2>/dev/null; then
	die "ricochet tourne depuis $dir : lancez d'abord ricochet-off"
fi

mkdir -p "$dir"
printf '\n%s→ compilation…%s\n' "$dim" "$off"
(cd "$repo" && CGO_ENABLED=0 go build -trimpath -tags "$tags" \
	-ldflags "-s -w -X main.version=$version" -o "$bin" ./cmd/ricochet) || die "échec de la compilation"

for f in ricochet-on.sh ricochet-off.sh ricochet-on.ps1 ricochet-off.ps1; do
	cp "$repo/scripts/$f" "$dir/$f"
done
chmod +x "$bin"

printf '%s✔ ricochet installé dans %s%s\n\n' "$green" "$dir" "$off"
if [ "$goos" = windows ]; then
	# Chemin Windows pour PowerShell (Git Bash fournit cygpath).
	wdir=$dir
	command -v cygpath >/dev/null 2>&1 && wdir=$(cygpath -w "$dir")
	printf 'PowerShell :\n'
	printf '  & "%s\\ricochet-on.ps1"     # activer\n' "$wdir"
	printf '  & "%s\\ricochet-off.ps1"    # désactiver\n\n' "$wdir"
	# shellcheck disable=SC2016 # $PROFILE est à afficher tel quel
	printf '%sRaccourcis à ajouter à $PROFILE :%s\n' "$dim" "$off"
	printf '  function ricochet-on  { & "%s\\ricochet-on.ps1" }\n' "$wdir"
	printf '  function ricochet-off { & "%s\\ricochet-off.ps1" }\n\n' "$wdir"
	printf 'Git Bash :\n'
fi
printf '  . %s/ricochet-on.sh     # activer\n' "$dir"
printf '  . %s/ricochet-off.sh    # désactiver\n\n' "$dir"
printf '%sAlias à ajouter à ~/.bashrc ou ~/.zshrc :%s\n' "$dim" "$off"
printf "  alias ricochet-on='. %s/ricochet-on.sh'\n" "$dir"
printf "  alias ricochet-off='. %s/ricochet-off.sh'\n" "$dir"
printf '\n%sRègles : éditez %s/ricochet-rules.txt (rechargé à chaud).%s\n' "$yellow" "$dir" "$off"
