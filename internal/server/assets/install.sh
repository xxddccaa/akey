#!/bin/sh
# akey installer (rendered by `akey serve`).
# Usage: curl -fsSk --pinnedpubkey 'sha256//...' https://HOST:PORT/i | sh [-s -- --only codex]
set -eu

SERVER='{{sq .Server}}'
PIN='{{sq .Pin}}'

say() { printf 'akey: %s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }

fetch() { # fetch URL DEST
	if [ -n "$PIN" ]; then
		curl -fsS -k --pinnedpubkey "$PIN" -o "$2" "$1"
	else
		curl -fsS -o "$2" "$1"
	fi
}

main() {
	command -v curl >/dev/null 2>&1 || die "curl is required"

	case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "unsupported OS: $(uname -s) (on Windows use the PowerShell installer: $SERVER/i.ps1)" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture: $(uname -m)" ;;
	esac

	name="akey-$os-$arch"
	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t akey)
	trap 'rm -rf "$tmp"' EXIT
	trap 'rm -rf "$tmp"; exit 130' INT TERM

	say "downloading $name from $SERVER"
	fetch "$SERVER/dl/$name" "$tmp/$name" || die "download of $name failed"
	fetch "$SERVER/dl/SHA256SUMS" "$tmp/SHA256SUMS" || die "download of SHA256SUMS failed"

	want=$(awk -v n="$name" '{ f = $2; sub(/^\*/, "", f); if (f == n) { print tolower($1); exit } }' "$tmp/SHA256SUMS")
	[ -n "$want" ] || die "no checksum for $name in SHA256SUMS"
	if command -v sha256sum >/dev/null 2>&1; then
		got=$(sha256sum "$tmp/$name" | awk '{ print tolower($1) }')
	elif command -v shasum >/dev/null 2>&1; then
		got=$(shasum -a 256 "$tmp/$name" | awk '{ print tolower($1) }')
	else
		die "need sha256sum or shasum to verify the download"
	fi
	[ "$got" = "$want" ] || die "checksum mismatch for $name (got $got, want $want)"

	BIN_DIR="${AKEY_BIN_DIR:-$HOME/.local/bin}"
	mkdir -p "$BIN_DIR"
	BIN="$BIN_DIR/akey"
	cp "$tmp/$name" "$BIN_DIR/.akey.new.$$"
	chmod 755 "$BIN_DIR/.akey.new.$$"
	mv -f "$BIN_DIR/.akey.new.$$" "$BIN"
	say "installed $BIN"

	case ":${PATH:-}:" in
	*":$BIN_DIR:"*) ;;
	*) say "note: $BIN_DIR is not on your PATH; add this to your shell profile: export PATH=\"$BIN_DIR:\$PATH\"" ;;
	esac

	# stdin is the curl pipe, so read the passphrase from the terminal when
	# there is one; otherwise (automation) AKEY_PASSPHRASE is used.
	if (: </dev/tty) 2>/dev/null; then
		"$BIN" pull --server "$SERVER" --pin "$PIN" "$@" </dev/tty
	else
		"$BIN" pull --server "$SERVER" --pin "$PIN" "$@"
	fi
}

main "$@"
