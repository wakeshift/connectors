#!/bin/sh
# Wakeshift's launcher, bin/wakeshift in each of Wakeshift's plugins
# (github.com/wakeshift/agent-plugins). The harness runs the plugin's hooks
# through it:
#
#   bin/wakeshift hook --harness <h> ...   make sure the connector is there, then run it
#   bin/wakeshift prepare                  only make sure (a session-start hook)
#
# Making sure, which is all a warm run does (no network):
#
#   1. ~/.wakeshift/bin/wakeshift is the connector the plugin pins
#      (wakeshift.env's connector_version) or newer. If it's missing or
#      older, it's fetched from connector_url (the signed release of
#      github.com/wakeshift/connectors), checked against the SHA-256 in the
#      plugin's SHA256SUMS, and moved into place in one rename. A download
#      that doesn't match is deleted and nothing is installed.
#   2. The connector trusts the Wakeshift the plugin was built for
#      (wakeshift.env's issuer): on first use, and whenever that changes, it
#      runs `wakeshift install --harness <h> --issuer <issuer> --yes
#      --plugin`, which trusts it, adds the allow rules and removes hooks an
#      earlier install wrote to the harness's settings.
#
# Both happen under a lock (~/.wakeshift/bin/.lock), so hooks that start
# together fetch once. A hook never fails the harness's call: on any problem
# the launcher says why on stderr (never stdout, which the harness reads)
# and exits 0, so the hook does nothing until it can.
set -u

say() { printf 'wakeshift: %s\n' "$*" >&2; }

case "$0" in
*/*) root=${0%/*}/.. ;;
*) root=.. ;;
esac

harness='' issuer='' connector_version='' connector_url=''
if ! [ -r "$root/wakeshift.env" ]; then
	say "the plugin has no wakeshift.env beside its bin folder; reinstall it"
	exit 0
fi
while IFS='=' read -r k v; do
	case "$k" in
	harness) harness=$v ;;
	issuer) issuer=$v ;;
	connector_version) connector_version=$v ;;
	connector_url) connector_url=$v ;;
	esac
done <"$root/wakeshift.env"
case "$harness" in
'' | *[!a-z-]*) say "wakeshift.env names no harness"; exit 0 ;;
esac
if [ -z "$issuer" ] || [ -z "$connector_url" ]; then
	say "wakeshift.env is incomplete; reinstall the plugin"
	exit 0
fi
case "$connector_version" in
'' | none) say "this plugin pins no connector yet; update it"; exit 0 ;;
esac

prepare=
if [ "${1:-}" = prepare ]; then prepare=1; fi

if [ -z "${WAKESHIFT_HOME:-}" ] && [ -z "${HOME:-}" ]; then
	say "HOME isn't set, so there's nowhere to keep the connector"
	exit 0
fi
home=${WAKESHIFT_HOME:-$HOME/.wakeshift}
bin=$home/bin/wakeshift
stamp=$home/$harness/plugin-issuer

# vge A B: version A >= B, comparing dot-separated numbers (anything from
# "+" on is the build). A version that isn't one (a development build's
# "dev") is older than any.
vge() {
	a=${1%%+*} b=${2%%+*}
	case "$a" in '' | *[!0-9.]*) return 1 ;; esac
	while [ -n "$a$b" ]; do
		x=${a%%.*} y=${b%%.*}
		case "$a" in *.*) a=${a#*.} ;; *) a= ;; esac
		case "$b" in *.*) b=${b#*.} ;; *) b= ;; esac
		case "$x$y" in *[!0-9]*) return 1 ;; esac
		[ "${x:-0}" -gt "${y:-0}" ] && return 0
		[ "${x:-0}" -lt "${y:-0}" ] && return 1
	done
	return 0
}

current() {
	[ -x "$bin" ] || return 1
	have=$("$bin" version </dev/null 2>/dev/null) || return 1
	vge "$have" "$connector_version"
}

trusted() {
	t=
	[ -r "$stamp" ] && read -r t <"$stamp"
	[ "$t" = "$issuer" ]
}

lock=$home/bin/.lock
tmp=
locked=
unlock() {
	[ -n "$tmp" ] && rm -f "$tmp"
	[ -n "$locked" ] && rm -rf "$lock"
	tmp='' locked=''
}
trap unlock EXIT
trap 'exit 0' HUP INT TERM

# take_lock: waits for another hook that's fetching (a minute at most); a
# lock whose holder is gone is taken over.
take_lock() {
	n=0
	until mkdir "$lock" 2>/dev/null; do
		pid=
		[ -r "$lock/pid" ] && read -r pid <"$lock/pid"
		if [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null; then
			rm -rf "$lock"
			continue
		fi
		n=$((n + 1))
		if [ "$n" -ge 60 ]; then
			say "another hook has been fetching the connector for a minute; this one does nothing"
			return 1
		fi
		sleep 1
	done
	locked=1
	echo $$ >"$lock/pid"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		return 1
	fi
}

fetch() {
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	case "$(uname -m)" in
	arm64 | aarch64) arch=arm64 ;;
	x86_64 | amd64) arch=amd64 ;;
	*) say "Wakeshift's connector isn't built for $(uname -m)"; return 1 ;;
	esac
	name=wakeshift-$os-$arch
	want=
	while read -r h n; do
		[ "$n" = "$name" ] && want=$h
	done <"$root/SHA256SUMS"
	case "$want" in
	????????????????????????????????????????????????????????????????) ;;
	*) say "this plugin carries no checksum for $name"; return 1 ;;
	esac
	# Only https, but for a Wakeshift on this machine (development and tests).
	case "$connector_url" in
	https://*) proto='=https' ;;
	http://127.0.0.1:* | http://localhost:* | file://*) proto='=http,https,file' ;;
	*) say "the plugin's connector_url isn't https: $connector_url"; return 1 ;;
	esac
	command -v curl >/dev/null 2>&1 || { say "fetching Wakeshift's connector needs curl"; return 1; }
	(umask 077 && mkdir -p "$home/bin") || { say "couldn't make $home/bin"; return 1; }
	tmp=$(mktemp "$home/bin/.wakeshift.XXXXXX") || { say "couldn't write to $home/bin"; return 1; }
	url=$connector_url/$name
	if ! curl -fsSL --proto "$proto" --proto-redir "$proto" --retry 2 --connect-timeout 10 --max-time 120 -o "$tmp" "$url" </dev/null; then
		say "couldn't download $url; the hook does nothing until it can"
		return 1
	fi
	got=$(sha256 "$tmp") || { say "checking the download needs sha256sum or shasum"; return 1; }
	if [ "$got" != "$want" ]; then
		say "the connector downloaded from $url didn't match the SHA-256 this plugin carries; nothing was installed"
		return 1
	fi
	chmod 755 "$tmp" || { say "couldn't install $bin"; return 1; }
	# The version it says must be the one pinned, or every hook would fetch it again.
	got=$("$tmp" version </dev/null 2>/dev/null)
	if ! vge "$got" "$connector_version"; then
		say "the connector downloaded from $url says it's version ${got:-unknown}, older than the $connector_version this plugin pins; nothing was installed"
		return 1
	fi
	mv -f "$tmp" "$bin" || { say "couldn't install $bin"; return 1; }
	tmp=
	chmod 700 "$home" 2>/dev/null
	return 0
}

trust() {
	if ! out=$("$bin" install --harness "$harness" --issuer "$issuer" --yes --plugin </dev/null 2>&1); then
		say "couldn't set up Wakeshift for $harness: $out"
		return 1
	fi
}

if ! current || ! trusted; then
	(umask 077 && mkdir -p "$home/bin") || { say "couldn't make $home/bin"; exit 0; }
	take_lock || exit 0
	# Again under the lock: another hook may have done it meanwhile.
	if ! current; then
		fetch || exit 0
	fi
	if ! trusted; then
		trust || exit 0
	fi
	unlock
fi

[ -n "$prepare" ] && exit 0
trap - EXIT HUP INT TERM
exec "$bin" "$@"
