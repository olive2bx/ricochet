# shellcheck shell=bash
# ricochet-off.sh — arrête ricochet et restaure la configuration proxy
# mémorisée par ricochet-on.sh.
#
# À SOURCER (bash ou zsh) pour modifier les variables du shell courant :
#   . ~/.ricochet/ricochet-off.sh

if [ -n "${ZSH_VERSION:-}" ]; then
	__rc_src=$(eval 'printf %s "${(%):-%x}"')
	case ${ZSH_EVAL_CONTEXT:-} in *:file*) __rc_sourced=1 ;; *) __rc_sourced=0 ;; esac
else
	__rc_src=${BASH_SOURCE[0]:-$0}
	if [ "${BASH_SOURCE[0]:-}" != "$0" ]; then __rc_sourced=1; else __rc_sourced=0; fi
fi

if [ "$__rc_sourced" != 1 ]; then
	printf '✖ ce script doit être sourcé pour restaurer les variables proxy du shell :\n    . %s\n' "$__rc_src" >&2
	unset __rc_src __rc_sourced
	exit 1
fi

__ricochet_off() {
	if [ -n "${ZSH_VERSION:-}" ]; then setopt local_options sh_word_split; fi
	local dir pidfile port proxy_url pid comm i v val restored=''
	local c_ok='' c_warn='' c_dim='' c_off=''
	if [ -t 1 ]; then
		c_ok=$(printf '\033[32m') c_warn=$(printf '\033[33m')
		c_dim=$(printf '\033[2m') c_off=$(printf '\033[0m')
	fi

	dir=$(cd "$(dirname "$1")" && pwd -P) || return 1
	pidfile=$dir/ricochet.pid
	port=${RICOCHET_PORT:-3129}
	proxy_url=${RICOCHET_ACTIVE:-http://127.0.0.1:$port}

	# Arrêt du processus (SIGTERM, puis SIGKILL après 5 s).
	pid=$(cat "$pidfile" 2>/dev/null)
	comm=''
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
		comm=$(ps -p "$pid" -o comm= 2>/dev/null) || comm=ricochet
	fi
	case $comm in
	*ricochet*)
		kill "$pid" 2>/dev/null
		i=0
		while kill -0 "$pid" 2>/dev/null && [ $i -lt 50 ]; do
			sleep 0.1
			i=$((i + 1))
		done
		if kill -0 "$pid" 2>/dev/null; then
			kill -9 "$pid" 2>/dev/null
			printf '%s! ricochet ne répondait pas : arrêt forcé%s\n' "$c_warn" "$c_off"
		fi
		printf '%s■ ricochet arrêté (pid %s)%s\n' "$c_ok" "$pid" "$c_off"
		;;
	*)
		printf '%s● ricochet n'"'"'était pas en cours d'"'"'exécution%s\n' "$c_dim" "$c_off"
		;;
	esac
	rm -f "$pidfile"

	# Restauration des variables proxy.
	if [ -n "${RICOCHET_SAVED_VARS+x}" ]; then
		unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY
		for v in $RICOCHET_SAVED_VARS; do
			eval "export $v=\"\$RICOCHET_SAVED_$v\""
			unset "RICOCHET_SAVED_$v"
		done
		unset RICOCHET_SAVED_VARS
		restored=1
	else
		# Shell non activé par ricochet-on : on retire seulement ce qui pointe sur ricochet.
		for v in http_proxy https_proxy HTTP_PROXY HTTPS_PROXY; do
			eval "val=\${$v:-}"
			if [ "$val" = "$proxy_url" ]; then unset "$v"; restored=1; fi
		done
	fi
	unset RICOCHET_ACTIVE

	if [ -n "$restored" ]; then
		printf '  configuration proxy restaurée :\n'
		for v in http_proxy https_proxy HTTP_PROXY HTTPS_PROXY; do
			if eval "[ -n \"\${$v+x}\" ]"; then
				eval "val=\$$v"
				printf '    %-11s = %s\n' "$v" "$(printf '%s' "$val" | sed -E 's#(://)[^/@]*@#\1***@#')"
			else
				printf '    %-11s %s(non définie)%s\n' "$v" "$c_dim" "$c_off"
			fi
		done
	else
		printf '  %svariables proxy inchangées%s\n' "$c_dim" "$c_off"
	fi
	printf '  %sles autres terminaux activés doivent aussi lancer ricochet-off%s\n' "$c_dim" "$c_off"
}

__ricochet_off "$__rc_src"
__rc_status=$?
unset -f __ricochet_off
unset __rc_src __rc_sourced
eval "unset __rc_status; return $__rc_status"
