#!/usr/bin/env bash
# akey server installer for a Linux VPS with systemd (amd64 / arm64).
#
#   curl -fsSL https://raw.githubusercontent.com/xxddccaa/akey/main/scripts/install-server.sh | sudo bash
#   curl -fsSL https://raw.githubusercontent.com/xxddccaa/akey/main/scripts/install-server.sh | sudo bash -s -- --host 203.0.113.7 --port 8787
#
# Options: see usage() below or run with --help. Rerunning the installer
# upgrades in place; /var/lib/akey (configs, TLS key, admin token) is kept.
#
# Everything runs inside main() at the bottom, so a truncated download
# can never execute half a script.

set -euo pipefail

readonly REPO="xxddccaa/akey"
readonly BIN=/usr/local/bin/akey
readonly SHARE_DIR=/usr/local/share/akey
readonly DIST_DIR=/usr/local/share/akey/dist
readonly DATA_DIR=/var/lib/akey
readonly UNIT=/etc/systemd/system/akey.service
readonly ASSETS=(akey-linux-amd64 akey-linux-arm64 akey-darwin-amd64 akey-darwin-arm64 akey-windows-amd64.exe akey-windows-arm64.exe)

B='' Y='' R='' N=''
if [ -t 2 ]; then
	B=$'\033[1m' Y=$'\033[1;33m' R=$'\033[1;31m' N=$'\033[0m'
fi
say() { printf '%sakey:%s %s\n' "$B" "$N" "$*"; }
warn() { printf '%sakey: warning:%s %s\n' "$Y" "$N" "$*" >&2; }
die() {
	printf '%sakey: error:%s %s\n' "$R" "$N" "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
akey server installer (Linux + systemd, amd64/arm64). Run as root:
  curl -fsSL https://raw.githubusercontent.com/xxddccaa/akey/main/scripts/install-server.sh | sudo bash -s -- [options]

Options:
  --host IP[,IP]     public IP/hostname for the TLS cert and printed URLs
                     (default: the existing service's, else auto-detected)
  --port N           listen port (default: the existing service's, else 8787)
  --version vX.Y.Z   release to install (default: latest)
  --mirror URL       GitHub download mirror prefix, e.g. https://ghproxy.example/
                     (or AKEY_GITHUB_MIRROR); its SHA256SUMS is cross-checked
                     against github.com when reachable
  --no-start         only install files: no systemd; prints the `akey serve` command
  --uninstall        stop + remove the service, /usr/local/bin/akey and /usr/local/share/akey
  --purge            with --uninstall: also delete /var/lib/akey (all configs, the TLS
                     key = the clients' pin, the admin token); asks for confirmation
  --yes              don't ask (for --purge without a terminal)

Environment:
  AKEY_GITHUB_MIRROR  same as --mirror
  AKEY_RELEASE_BASE   URL/ holding akey-<os>-<arch>[.exe] + SHA256SUMS; replaces the
                      GitHub release download base (self-hosting, tests)

Rerun to upgrade; /var/lib/akey is kept.
EOF
}

# ---------------------------------------------------------------- helpers

valid_ipv4() {
	[[ $1 =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
	local o
	for o in "${BASH_REMATCH[@]:1}"; do
		((10#$o <= 255)) || return 1
	done
}

has_systemd() { [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; }

# fetch URL DEST
fetch() {
	curl -fsSL --proto '=https,http' --retry 3 --retry-delay 2 --connect-timeout 15 --max-time 600 -o "$2" "$1"
}

# github_url URL: apply the mirror prefix to a github.com URL.
github_url() { printf '%s%s' "$MIRROR" "$1"; }

# tag_from_url URL: print vX.Y.Z from .../releases/tag/vX.Y.Z
tag_from_url() {
	local u=${1%/}
	case $u in
	*/releases/tag/*) printf '%s' "${u##*/}" ;;
	*) return 1 ;;
	esac
}

resolve_latest() {
	local url eff tag
	for url in "$(github_url "https://github.com/$REPO/releases/latest")" "https://github.com/$REPO/releases/latest"; do
		eff=$(curl -fsSL --connect-timeout 10 --max-time 30 -o /dev/null -w '%{url_effective}' "$url" 2>/dev/null) || continue
		if tag=$(tag_from_url "$eff") && valid_tag "$tag"; then
			printf '%s' "$tag"
			return 0
		fi
	done
	# Fallback: the API (no jq needed).
	tag=$(curl -fsSL --connect-timeout 10 --max-time 30 "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null |
		sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1) || true
	if [ -n "$tag" ] && valid_tag "$tag"; then
		printf '%s' "$tag"
		return 0
	fi
	return 1
}

valid_tag() { [[ $1 =~ ^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$ ]]; }

# IPs / hostnames, comma-separated (IPv6 literals may be bracketed).
valid_hosts() {
	local re='^[]A-Za-z0-9.:,[-]{1,255}$'
	[[ $1 =~ $re ]] && [[ $1 != *,,* ]] && [[ $1 != ,* ]]
}

# sum_for NAME SUMSFILE: expected lowercase hex for NAME.
sum_for() {
	awk -v n="$1" '{ f = $2; sub(/^\*/, "", f); if (f == n) { print tolower($1); exit } }' "$2"
}

detect_host() {
	local ip url
	for url in https://api.ipify.org https://ifconfig.me; do
		ip=$(curl -fsS4 --max-time 5 "$url" 2>/dev/null | tr -d '[:space:]') || ip=
		if valid_ipv4 "$ip"; then
			HOST=$ip
			HOST_SOURCE="public IP reported by ${url#https://}"
			return 0
		fi
	done
	if command -v ip >/dev/null 2>&1; then
		ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit } }') || ip=
		if valid_ipv4 "$ip"; then
			HOST=$ip
			HOST_SOURCE="local interface address (ip route) - may be a private IP behind NAT"
			return 0
		fi
	fi
	ip=$(hostname -I 2>/dev/null | awk '{ print $1 }') || ip=
	if valid_ipv4 "$ip"; then
		HOST=$ip
		HOST_SOURCE="local interface address (hostname -I) - may be a private IP behind NAT"
		return 0
	fi
	return 1
}

# Settings of an already installed service, so a plain rerun keeps them.
existing_setting() { # existing_setting host|port
	[ -f "$UNIT" ] || return 1
	local line
	line=$(grep -m1 '^ExecStart=' "$UNIT") || return 1
	case $1 in
	host) sed -n 's/.*[[:space:]]--host[[:space:]]\{1,\}\([^[:space:]]\{1,\}\).*/\1/p' <<<"$line" ;;
	port) sed -n 's/.*[[:space:]]--listen[[:space:]]\{1,\}[^[:space:]]*:\([0-9]\{1,5\}\).*/\1/p' <<<"$line" ;;
	esac
}

write_unit() { # write_unit PORT HOST
	local port=$1 host=$2 caps="CapabilityBoundingSet=
AmbientCapabilities="
	if ((port < 1024)); then
		caps="CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE"
	fi
	local tmp="$UNIT.tmp.$$"
	cat >"$tmp" <<EOF
# Written by akey's scripts/install-server.sh; rerun it to change --host/--port
# or upgrade (/var/lib/akey is kept). Template: deploy/akey.service.
[Unit]
Description=akey - encrypted agent config server (API + web panel)
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN serve --listen :$port --data $DATA_DIR --dist $DIST_DIR --host $host
Restart=on-failure
RestartSec=3

# Unprivileged, throwaway user; /var/lib/akey is its private state dir.
DynamicUser=yes
StateDirectory=akey
StateDirectoryMode=0700
UMask=0077

# Hardening
NoNewPrivileges=yes
$caps
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
LimitNOFILE=8192

[Install]
WantedBy=multi-user.target
EOF
	chmod 0644 "$tmp"
	mv -f "$tmp" "$UNIT"
}

open_firewall() { # open_firewall PORT
	local port=$1 opened=
	if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		if ufw allow "$port/tcp" >/dev/null; then
			say "ufw: allowed $port/tcp"
			opened=1
		else
			warn "ufw allow $port/tcp failed"
		fi
	fi
	if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		if firewall-cmd --permanent --add-port="$port/tcp" >/dev/null && firewall-cmd --reload >/dev/null; then
			say "firewalld: allowed $port/tcp"
			opened=1
		else
			warn "firewall-cmd --add-port=$port/tcp failed"
		fi
	fi
	if [ -z "$opened" ] && command -v iptables >/dev/null 2>&1 &&
		iptables -S INPUT 2>/dev/null | grep -Eq '^-P INPUT (DROP|REJECT)|-j REJECT|-j DROP'; then
		warn "iptables INPUT has DROP/REJECT rules (e.g. Oracle Cloud images). If the port stays closed, run:"
		warn "  iptables -I INPUT -p tcp --dport $port -j ACCEPT   (and persist it, e.g. netfilter-persistent save)"
	fi
}

wait_ready() { # wait_ready PORT
	local i
	for ((i = 0; i < 20; i++)); do
		if curl -fsSk --noproxy '*' --max-time 2 -o /dev/null "https://127.0.0.1:$1/v1/params" 2>/dev/null; then
			return 0
		fi
		sleep 0.5
	done
	return 1
}

# ---------------------------------------------------------------- uninstall

uninstall() {
	# Confirm a purge before touching anything.
	if [ -n "$PURGE" ]; then
		warn "--purge deletes $DATA_DIR: every stored config (all versions), the TLS key (the pin every"
		warn "client uses) and the admin token. This cannot be undone."
		if [ -z "$YES" ]; then
			local answer=''
			(: </dev/tty) 2>/dev/null || die "no terminal to confirm --purge; rerun with --uninstall --purge --yes"
			printf 'Type "purge" to delete %s: ' "$DATA_DIR" >/dev/tty
			read -r answer </dev/tty || answer=''
			[ "$answer" = purge ] || die "not confirmed; nothing was removed"
		fi
	fi
	if has_systemd; then
		systemctl disable --now akey >/dev/null 2>&1 || true
	fi
	if [ -f "$UNIT" ]; then
		rm -f "$UNIT"
		say "removed $UNIT"
	fi
	if has_systemd; then
		systemctl daemon-reload || true
		systemctl reset-failed akey >/dev/null 2>&1 || true
	fi
	rm -f "$BIN"
	rm -rf "$SHARE_DIR"
	say "removed $BIN and $SHARE_DIR"
	if [ -z "$PURGE" ]; then
		say "kept $DATA_DIR (encrypted configs, TLS key/pin, admin token); a reinstall reuses it."
		say "to delete it too: ... | sudo bash -s -- --uninstall --purge"
		return 0
	fi
	rm -rf "$DATA_DIR" /var/lib/private/akey
	say "deleted $DATA_DIR"
	say "firewall rules (if any were added) were left in place"
}

# ---------------------------------------------------------------- main

main() {
	HOST='' PORT='' VERSION='' MIRROR=${AKEY_GITHUB_MIRROR:-} NO_START='' UNINSTALL='' PURGE='' YES='' HOST_SOURCE=''
	while [ $# -gt 0 ]; do
		case $1 in
		--host) [ $# -ge 2 ] || die "--host needs a value"; HOST=$2; HOST_SOURCE="--host"; shift 2 ;;
		--host=*) HOST=${1#*=}; HOST_SOURCE="--host"; shift ;;
		--port) [ $# -ge 2 ] || die "--port needs a value"; PORT=$2; shift 2 ;;
		--port=*) PORT=${1#*=}; shift ;;
		--version) [ $# -ge 2 ] || die "--version needs a value"; VERSION=$2; shift 2 ;;
		--version=*) VERSION=${1#*=}; shift ;;
		--mirror) [ $# -ge 2 ] || die "--mirror needs a value"; MIRROR=$2; shift 2 ;;
		--mirror=*) MIRROR=${1#*=}; shift ;;
		--no-start) NO_START=1; shift ;;
		--uninstall) UNINSTALL=1; shift ;;
		--purge) PURGE=1; shift ;;
		--yes | -y) YES=1; shift ;;
		-h | --help) usage; exit 0 ;;
		*) die "unknown option: $1 (see --help)" ;;
		esac
	done

	[ "$(id -u)" -eq 0 ] || die "run as root: curl -fsSL .../install-server.sh | sudo bash -s -- ..."
	[ "$(uname -s)" = Linux ] || die "this installer is for Linux servers (got $(uname -s))"
	if [ -n "$PURGE" ] && [ -z "$UNINSTALL" ]; then
		die "--purge only works together with --uninstall"
	fi
	if [ -n "$UNINSTALL" ]; then
		uninstall
		return 0
	fi

	local arch
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported CPU architecture: $(uname -m) (need x86_64 or aarch64)" ;;
	esac
	local tool
	for tool in curl sha256sum awk sed install mktemp; do
		command -v "$tool" >/dev/null 2>&1 || die "$tool is required (e.g. apt-get install -y curl coreutils)"
	done
	if [ -z "$NO_START" ] && ! has_systemd; then
		die "systemd is not running here; use --no-start to only install the files and run \`akey serve\` yourself"
	fi

	# Port / host: flags, else the existing service's, else defaults.
	local reused=
	if [ -z "$PORT" ]; then
		PORT=$(existing_setting port 2>/dev/null) || PORT=
		[ -n "$PORT" ] && reused=1
		PORT=${PORT:-8787}
	fi
	if ! [[ $PORT =~ ^[0-9]{1,5}$ ]] || ((10#$PORT < 1 || 10#$PORT > 65535)); then
		die "invalid --port: $PORT"
	fi
	PORT=$((10#$PORT))
	if [ -z "$HOST" ]; then
		HOST=$(existing_setting host 2>/dev/null) || HOST=
		if [ -n "$HOST" ] && valid_hosts "$HOST"; then
			HOST_SOURCE="kept from the existing $UNIT"
		else
			HOST=
			say "detecting this server's public IP ..."
			detect_host || die "could not detect the public IP; pass --host <ip>"
		fi
	fi
	valid_hosts "$HOST" || die "invalid --host: $HOST (want IP or hostname, comma-separated)"

	# Download base.
	if [ -n "$MIRROR" ]; then
		[[ $MIRROR =~ ^https?://[^[:space:]\'\"]+$ ]] || die "invalid --mirror: $MIRROR"
		MIRROR=${MIRROR%/}/
	fi
	local base
	if [ -n "${AKEY_RELEASE_BASE:-}" ]; then
		[[ $AKEY_RELEASE_BASE =~ ^https?://[^[:space:]]+$ ]] || die "invalid AKEY_RELEASE_BASE: $AKEY_RELEASE_BASE"
		base=${AKEY_RELEASE_BASE%/}/
		VERSION=${VERSION:-custom}
		valid_tag "$VERSION" || die "invalid --version: $VERSION"
		say "using AKEY_RELEASE_BASE=$base (version label $VERSION)"
	else
		if [ -z "$VERSION" ]; then
			say "looking up the latest release ..."
			VERSION=$(resolve_latest) || die "could not determine the latest release; pass --version vX.Y.Z (and/or --mirror URL)"
		fi
		[[ $VERSION == v* ]] || [[ ! $VERSION =~ ^[0-9] ]] || VERSION=v$VERSION
		valid_tag "$VERSION" || die "invalid --version: $VERSION"
		base=$(github_url "https://github.com/$REPO/releases/download/$VERSION/")
	fi

	# Download + verify everything before touching the system.
	local tmp
	tmp=$(mktemp -d)
	# shellcheck disable=SC2064 # expand $tmp now
	trap "rm -rf '$tmp'" EXIT
	say "downloading akey $VERSION from $base"
	fetch "${base}SHA256SUMS" "$tmp/SHA256SUMS" || die "download failed: ${base}SHA256SUMS"
	if [ -n "$MIRROR" ] && [ -z "${AKEY_RELEASE_BASE:-}" ]; then
		if fetch "https://github.com/$REPO/releases/download/$VERSION/SHA256SUMS" "$tmp/SHA256SUMS.github" 2>/dev/null; then
			cmp -s "$tmp/SHA256SUMS" "$tmp/SHA256SUMS.github" || die "the mirror's SHA256SUMS differs from github.com's - not installing"
			say "mirror checksums match github.com"
		else
			warn "could not reach github.com to cross-check the mirror's SHA256SUMS; trusting the mirror"
		fi
	fi
	local f want got
	for f in "${ASSETS[@]}"; do
		printf '  %s\n' "$f"
		fetch "$base$f" "$tmp/$f" || die "download failed: $base$f"
		want=$(sum_for "$f" "$tmp/SHA256SUMS")
		[ -n "$want" ] || die "no checksum for $f in SHA256SUMS"
		got=$(sha256sum "$tmp/$f" | awk '{ print tolower($1) }')
		[ "$got" = "$want" ] || die "checksum mismatch for $f (got $got, want $want) - nothing was installed"
	done
	say "all ${#ASSETS[@]} binaries match SHA256SUMS"

	# Install: client binaries (served under /dl/) then the server binary;
	# both replaced by rename so a running service never sees a partial file.
	mkdir -p "$SHARE_DIR"
	chmod 0755 "$SHARE_DIR"
	local stage="$SHARE_DIR/.dist.new.$$"
	rm -rf "$stage"
	mkdir -m 0755 "$stage"
	for f in "${ASSETS[@]}" SHA256SUMS; do
		install -m 0644 "$tmp/$f" "$stage/$f"
	done
	rm -rf "$SHARE_DIR/.dist.old"
	[ -e "$DIST_DIR" ] && mv "$DIST_DIR" "$SHARE_DIR/.dist.old"
	mv "$stage" "$DIST_DIR"
	rm -rf "$SHARE_DIR/.dist.old"
	install -m 0755 "$tmp/akey-linux-$arch" "$BIN.new.$$"
	mv -f "$BIN.new.$$" "$BIN"
	local installed
	installed=$("$BIN" version 2>&1) || die "installed $BIN does not run: $installed"
	say "installed $BIN ($installed) and client binaries in $DIST_DIR"
	if [ -n "$reused" ]; then
		say "port $PORT kept from the existing service (change with --port N)"
	fi
	say "host: $HOST ($HOST_SOURCE); change it by rerunning with --host <ip-or-name>"

	open_firewall "$PORT"

	local serve_cmd="$BIN serve --listen :$PORT --data $DATA_DIR --dist $DIST_DIR --host $HOST"
	local info_cmd="$BIN info --data $DATA_DIR --host $HOST --listen :$PORT"
	if [ -n "$NO_START" ]; then
		say "--no-start: systemd not touched. Start the server with:"
		printf '\n  %s\n\n' "$serve_cmd"
		say "then print the panel URL, admin token and client commands with:"
		printf '\n  %s\n\n' "$info_cmd"
		say "remember to open TCP port $PORT in your cloud provider's security group / firewall"
		return 0
	fi

	write_unit "$PORT" "$HOST"
	systemctl daemon-reload
	systemctl enable akey >/dev/null 2>&1
	systemctl restart akey
	say "systemd service akey (re)started; waiting for it to answer on port $PORT ..."
	if ! wait_ready "$PORT"; then
		journalctl -u akey -n 40 --no-pager >&2 || true
		die "akey did not answer on https://127.0.0.1:$PORT/ (see the log above; systemctl status akey)"
	fi
	echo
	"$BIN" info --data "$DATA_DIR" --host "$HOST" --listen ":$PORT" || die "akey info failed"
	echo
	say "IMPORTANT: also open TCP port $PORT in your cloud provider's security group / firewall panel."
	say "upgrade: rerun this installer (data in $DATA_DIR is kept)"
	say "logs: journalctl -u akey -f    reprint this info: sudo $info_cmd"
}

main "$@"
