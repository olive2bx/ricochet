# shellcheck shell=bash
# ricochet-on.sh — démarre ricochet en arrière-plan et fait pointer
# http_proxy / https_proxy (et HTTP_PROXY / HTTPS_PROXY) sur lui.
#
# Le proxy configuré avant l'activation devient le proxy amont de ricochet.
#
# À SOURCER (bash ou zsh) pour modifier les variables du shell courant :
#   . ~/.ricochet/ricochet-on.sh
#
# Variables optionnelles :
#   RICOCHET_PORT     port du proxy        (3129)
#   RICOCHET_UI_PORT  port du dashboard    (3130)
#   RICOCHET_RULES    fichier de règles    (<dossier du script>/ricochet-rules.txt)
#   RICOCHET_BIN      binaire ricochet     (<dossier du script>/ricochet[.exe])
#   RICOCHET_ARGS     options supplémentaires (ex. "--mitm-hosts *.exemple.com")

if [ -n "${ZSH_VERSION:-}" ]; then
	__rc_src=$(eval 'printf %s "${(%):-%x}"')
	case ${ZSH_EVAL_CONTEXT:-} in *:file*) __rc_sourced=1 ;; *) __rc_sourced=0 ;; esac
else
	__rc_src=${BASH_SOURCE[0]:-$0}
	if [ "${BASH_SOURCE[0]:-}" != "$0" ]; then __rc_sourced=1; else __rc_sourced=0; fi
fi

if [ "$__rc_sourced" != 1 ]; then
	printf '✖ ce script doit être sourcé pour modifier les variables proxy du shell :\n    . %s\n' "$__rc_src" >&2
	unset __rc_src __rc_sourced
	exit 1
fi

__ricochet_on() {
	if [ -n "${ZSH_VERSION:-}" ]; then setopt local_options sh_word_split; fi
	local dir bin port ui_port rules pidfile log proxy_url upstream pid v val i
	local ping_out run_up shown_up
	local c_ok='' c_warn='' c_err='' c_dim='' c_off=''
	if [ -t 1 ]; then
		c_ok=$(printf '\033[32m') c_warn=$(printf '\033[33m') c_err=$(printf '\033[31m')
		c_dim=$(printf '\033[2m') c_off=$(printf '\033[0m')
	fi

	dir=$(cd "$(dirname "$1")" && pwd -P) || return 1
	if [ -n "${RICOCHET_BIN:-}" ]; then
		bin=$RICOCHET_BIN
	elif [ -f "$dir/ricochet.exe" ] && [ ! -f "$dir/ricochet" ]; then
		bin=$dir/ricochet.exe
	else
		bin=$dir/ricochet
	fi
	port=${RICOCHET_PORT:-3129}
	ui_port=${RICOCHET_UI_PORT:-3130}
	rules=${RICOCHET_RULES:-$dir/ricochet-rules.txt}
	pidfile=$dir/ricochet.pid
	log=$dir/ricochet.log
	proxy_url=http://127.0.0.1:$port

	if [ ! -x "$bin" ]; then
		printf '%s✖ binaire ricochet introuvable : %s%s\n' "$c_err" "$bin" "$c_off" >&2
		printf '  installez-le avec « make install » depuis le dépôt ricochet.\n' >&2
		return 1
	fi

	# Mémorise la configuration proxy d'origine (une seule fois par shell).
	if [ "${RICOCHET_ACTIVE:-}" != "$proxy_url" ]; then
		RICOCHET_SAVED_VARS=''
		for v in http_proxy https_proxy HTTP_PROXY HTTPS_PROXY; do
			if eval "[ -n \"\${$v+x}\" ]"; then
				eval "export RICOCHET_SAVED_$v=\"\$$v\""
				RICOCHET_SAVED_VARS="$RICOCHET_SAVED_VARS $v"
			fi
		done
		export RICOCHET_SAVED_VARS
	fi

	# Proxy amont : https_proxy en priorité, puis http_proxy.
	upstream=''
	for v in https_proxy HTTPS_PROXY http_proxy HTTP_PROXY; do
		eval "val=\${RICOCHET_SAVED_$v:-}"
		if [ -n "$val" ]; then upstream=$val; break; fi
	done
	case $upstream in
	*127.0.0.1:"$port"* | *localhost:"$port"*)
		printf '%s! le proxy actuel pointe déjà sur ricochet : pas de proxy amont%s\n' "$c_warn" "$c_off"
		upstream=''
		;;
	esac
	eval "val=\${RICOCHET_SAVED_http_proxy:-\${RICOCHET_SAVED_HTTP_PROXY:-}}"
	if [ -n "$val" ] && [ -n "$upstream" ] && [ "$val" != "$upstream" ]; then
		printf '%s! http_proxy et https_proxy diffèrent : ricochet utilise %s comme amont unique%s\n' \
			"$c_warn" "$(__ricochet_redact "$upstream")" "$c_off"
	fi

	# Instance déjà lancée ? On interroge son dashboard : fiable quel que soit
	# le terminal (bash, zsh, Git Bash, PowerShell) ou la façon de la lancer.
	shown_up=$(__ricochet_hostport "$upstream")
	ping_out=$("$bin" --dashboard "127.0.0.1:$ui_port" --ping 2>/dev/null)
	if [ -n "$ping_out" ]; then
		pid=$(printf '%s\n' "$ping_out" | sed -n 's/^pid=//p')
		run_up=$(printf '%s\n' "$ping_out" | sed -n 's/^upstream=//p')
		[ "$run_up" = "none (direct)" ] && run_up=''
		printf '%s● ricochet tourne déjà (pid %s)%s\n' "$c_ok" "$pid" "$c_off"
		if [ "$(cat "$pidfile" 2>/dev/null)" != "$pid" ]; then
			printf '  %slancé depuis un autre terminal : ce terminal l'"'"'utilise sans le relancer%s\n' "$c_dim" "$c_off"
		fi
		if [ "$run_up" != "$shown_up" ]; then
			printf '%s! l'"'"'instance en cours utilise l'"'"'amont « %s » et non « %s » ;%s\n' \
				"$c_warn" "${run_up:-aucun}" "${shown_up:-aucun}" "$c_off"
			printf '%s  pour changer : ricochet-off puis ricochet-on%s\n' "$c_warn" "$c_off"
		fi
		shown_up=$run_up
	elif __ricochet_running "$pidfile"; then
		# Processus présent mais dashboard muet (démarrage en cours).
		pid=$(cat "$pidfile")
		printf '%s● ricochet tourne déjà (pid %s), dashboard pas encore joignable%s\n' "$c_warn" "$pid" "$c_off"
	else
		rm -f "$pidfile"
		if [ ! -f "$rules" ]; then
			__ricochet_rules_template >"$rules" || return 1
			printf '%s+ fichier de règles créé : %s%s\n' "$c_dim" "$rules" "$c_off"
		fi
		# RICOCHET_UPSTREAM plutôt que --upstream : les identifiants éventuels
		# n'apparaissent pas dans la liste des processus.
		# shellcheck disable=SC2086 # RICOCHET_ARGS est volontairement découpé
		(RICOCHET_UPSTREAM=$upstream nohup "$bin" \
			--listen "127.0.0.1:$port" --dashboard "127.0.0.1:$ui_port" \
			--rules "$rules" ${RICOCHET_ARGS:-} >"$log" 2>&1 &
		echo $! >"$pidfile")
		pid=$(cat "$pidfile")

		# Attend le message de démarrage (5 s max).
		i=0
		while ! grep -q 'msg="ricochet started"' "$log" 2>/dev/null; do
			if ! kill -0 "$pid" 2>/dev/null || [ $i -ge 50 ]; then
				printf '%s✖ ricochet n'"'"'a pas démarré.%s\n' "$c_err" "$c_off" >&2
				# « listen tcp ...: bind: » : port pris (message système traduit selon la langue).
				if grep -qE 'listen tcp [^ ]+: bind:' "$log" 2>/dev/null; then
					printf '  le port %s ou %s est déjà utilisé par un autre programme\n' "$port" "$ui_port" >&2
					printf '  (autres ports : RICOCHET_PORT=… RICOCHET_UI_PORT=…)\n' >&2
				fi
				printf '  dernières lignes de %s :\n' "$log" >&2
				tail -n 5 "$log" >&2
				kill "$pid" 2>/dev/null
				rm -f "$pidfile"
				return 1
			fi
			sleep 0.1
			i=$((i + 1))
		done
		printf '%s▶ ricochet démarré (pid %s)%s\n' "$c_ok" "$pid" "$c_off"
	fi

	export http_proxy="$proxy_url" https_proxy="$proxy_url"
	export HTTP_PROXY="$proxy_url" HTTPS_PROXY="$proxy_url"
	export RICOCHET_ACTIVE="$proxy_url"

	val=$(grep -o 'rule_count=[0-9]*' "$log" 2>/dev/null | tail -n 1 | cut -d= -f2)
	printf '  proxy     : %s  %s(http_proxy, https_proxy, HTTP_PROXY, HTTPS_PROXY)%s\n' "$proxy_url" "$c_dim" "$c_off"
	printf '  dashboard : http://127.0.0.1:%s/\n' "$ui_port"
	if [ -n "$shown_up" ]; then
		printf '  amont     : %s\n' "$shown_up"
	else
		printf '  amont     : aucun (connexions directes)\n'
	fi
	printf '  règles    : %s%s\n' "$rules" "${val:+ ($val règle(s))}"
	printf '  logs      : %s\n' "$log"
	printf '  %sarrêt     : . %s/ricochet-off.sh%s\n' "$c_dim" "$dir" "$c_off"
}

# __ricochet_running PIDFILE : vrai si le processus du fichier tourne encore.
__ricochet_running() {
	local pid comm
	[ -f "$1" ] || return 1
	pid=$(cat "$1" 2>/dev/null)
	if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then return 1; fi
	# Évite un faux positif si le pid a été réutilisé (ps -o absent : on accepte).
	comm=$(ps -p "$pid" -o comm= 2>/dev/null) || return 0
	case $comm in *ricochet*) return 0 ;; *) return 1 ;; esac
}

# hôte:port d'une URL de proxy, sans identifiants (port 80 par défaut, comme
# ricochet) ; vide si l'URL est vide.
__ricochet_hostport() {
	local hp
	[ -n "$1" ] || return 0
	hp=$(printf '%s' "$1" | sed -E 's#^[A-Za-z][A-Za-z0-9+.-]*://##; s#^[^@/]*@##; s#/.*$##')
	case $hp in *:*) ;; *) hp=$hp:80 ;; esac
	printf '%s' "$hp"
}

# Masque les identifiants d'une URL de proxy.
__ricochet_redact() {
	printf '%s' "$1" | sed -E 's#(://)[^/@]*@#\1***@#'
}

__ricochet_rules_template() {
	cat <<'EOF'
# Règles ricochet : FQDN → IP (rechargées à chaud, sans redémarrer).
#
#   <ip>[:port]        <host> [<host>...]
#
# Exemples :
# 10.1.2.3             api.exemple.com
# 10.1.2.4             *.interne.exemple.com     # sous-domaines uniquement
# 10.1.2.5:8443        svc.exemple.com           # override du port
# 2001:db8::1          v6.exemple.com
EOF
}

__ricochet_on "$__rc_src"
__rc_status=$?
unset -f __ricochet_on __ricochet_running __ricochet_redact __ricochet_hostport __ricochet_rules_template
unset __rc_src __rc_sourced
eval "unset __rc_status; return $__rc_status"
