#!/bin/sh
# Herdr runs this as the plugin's build step (`herdr plugin install
# est7/herdr-amq-adapter`); `herdr-amq-adapter update` reinstalls through it.
#
# 1. bin/herdr-amq-adapter: the prebuilt binary of the release named by
#    herdr-plugin.toml's `version`, checked against the release's SHA256SUMS.
#    A checkout that is not that release's commit (a development checkout,
#    local changes) builds from source with Go instead.
# 2. amq and amq-bridge: when amq is not installed, the pinned AMQ release
#    is downloaded into ~/.local/bin, checked against its checksums.txt.
#    An installed amq is never replaced; one older than the tested version
#    gets a warning.
# 3. ~/.local/bin/herdr-amq-adapter links to the installed binary.
#
#   HERDR_AMQ_ADAPTER_BUILD=source         always build from source
#   HERDR_AMQ_ADAPTER_DOWNLOAD_URL=<url>   fetch <url>/v<version>/<asset>
#                                          instead of the GitHub release
#   AMQ_DOWNLOAD_URL=<url>                 fetch <url>/v<amq>/<asset> for AMQ
set -u

cd "$(dirname "$0")/.." || exit 1

AMQ_VERSION=0.81.1 # the AMQ release this adapter is tested with
AMQ_REPO=avivsinai/agent-message-queue
ADAPTER_REPO=est7/herdr-amq-adapter
BIN_DIR=${XDG_BIN_HOME:-$HOME/.local/bin}

say() { printf 'herdr-amq-adapter install: %s\n' "$*" >&2; }
die() { say "$*"; exit 1; }

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --retry 2 --connect-timeout 10 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -T 10 -O "$2" "$1"; }
else
  fetch() { return 1; }
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d ' ' -f 1; }
else
  sha256() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
fi
# checksum <sums file> <asset>: the expected digest, or nothing.
checksum() { awk -v n="$2" '$2 == n || $2 == "*" n { print $1 }' "$1"; }

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) die "unsupported OS $(uname -s): Herdr plugins run on macOS and Linux" ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) die "unsupported CPU $(uname -m)" ;;
esac

tmp=$(mktemp -d "${TMPDIR:-/tmp}/herdr-amq-adapter.XXXXXX") || die "could not create a temp folder"
trap 'rm -rf "$tmp"' EXIT

version=$(sed -n 's/^version *= *"\([^"]*\)".*/\1/p' herdr-plugin.toml | head -n 1)
[ -n "$version" ] || die "herdr-plugin.toml has no version"
tag="v$version"

build_from_source() {
  say "$1; building from source"
  command -v go >/dev/null 2>&1 ||
    die "$1, and Go is not installed to build from source instead"
  mkdir -p bin
  go build -trimpath -o bin/.herdr-amq-adapter.new ./cmd/herdr-amq-adapter || exit $?
  # Rename, never copy over: macOS kills a binary rewritten in place.
  mv -f bin/.herdr-amq-adapter.new bin/herdr-amq-adapter
  say "built bin/herdr-amq-adapter from source"
}

# The prebuilt binary matches only the release commit. Anything else (a
# development checkout, local changes) builds what it has.
use_release() {
  [ "${HERDR_AMQ_ADAPTER_BUILD:-}" = source ] && { reason="HERDR_AMQ_ADAPTER_BUILD=source is set"; return 1; }
  # `herdr plugin install` always takes the release binary of the manifest's
  # version, whatever commit it cloned.
  case "$PWD" in
    */plugins/.tmp-install-*/*) return 0 ;;
  esac
  if [ -d .git ] || [ -f .git ]; then
    if [ -n "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]; then
      reason="this checkout has uncommitted changes"; return 1
    fi
    head=$(git rev-parse HEAD 2>/dev/null)
    release=$(git rev-parse -q --verify "refs/tags/$tag^{commit}" 2>/dev/null)
    if [ -z "$release" ]; then
      release=$(GIT_TERMINAL_PROMPT=0 git ls-remote origin "refs/tags/$tag" "refs/tags/$tag^{}" 2>/dev/null |
        awk '{ sha = $1 } $2 ~ /\^\{\}$/ { peeled = $1 } END { print (peeled != "" ? peeled : sha) }')
    fi
    [ -n "$release" ] || { reason="there is no $tag tag here or on origin"; return 1; }
    [ "$head" = "$release" ] || { reason="this checkout is not the $tag release commit"; return 1; }
  fi
  return 0
}

install_adapter() {
  if ! use_release; then
    if [ "${HERDR_AMQ_ADAPTER_BUILD:-}" = source ] || command -v go >/dev/null 2>&1; then
      build_from_source "$reason"
      return
    fi
    say "$reason, but Go is not installed; taking the $tag release binary"
  fi
  asset="herdr-amq-adapter_${os}_${arch}"
  if [ -n "${HERDR_AMQ_ADAPTER_DOWNLOAD_URL:-}" ]; then
    base="${HERDR_AMQ_ADAPTER_DOWNLOAD_URL%/}/$tag"
  else
    base="https://github.com/$ADAPTER_REPO/releases/download/$tag"
  fi
  say "downloading $asset $tag"
  fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || { build_from_source "could not download $base/SHA256SUMS"; return; }
  expected=$(checksum "$tmp/SHA256SUMS" "$asset")
  [ -n "$expected" ] || { build_from_source "the $tag release has no $asset"; return; }
  fetch "$base/$asset" "$tmp/$asset" || { build_from_source "could not download $base/$asset"; return; }
  actual=$(sha256 "$tmp/$asset")
  [ "$actual" = "$expected" ] || die "$asset does not match the release's SHA256SUMS (got $actual, want $expected); refusing it"
  chmod 755 "$tmp/$asset"
  reported=$("$tmp/$asset" version 2>/dev/null)
  [ "$reported" = "$version" ] || die "the downloaded $asset reports version '${reported:-nothing}', want $version; refusing it"
  mkdir -p bin
  mv -f "$tmp/$asset" bin/herdr-amq-adapter || die "could not move the binary into bin/"
  say "installed the prebuilt $asset $tag"
}

# older A B: A is an older dotted version than B.
older() {
  [ "$1" = "$2" ] && return 1
  first=$(printf '%s\n%s\n' "$1" "$2" | sort -t . -k1,1n -k2,2n -k3,3n | head -n 1)
  [ "$first" = "$1" ]
}

find_amq() {
  for c in "${AMQ_BIN:-}" "$(command -v amq 2>/dev/null)" "$BIN_DIR/amq" /opt/homebrew/bin/amq /usr/local/bin/amq; do
    [ -n "$c" ] && [ -x "$c" ] && { printf '%s\n' "$c"; return 0; }
  done
  return 1
}

# fetch_amq_tool <name> <version>: extract <name> from its AMQ release
# archive into BIN_DIR, verified against the release's checksums.txt.
fetch_amq_tool() {
  name=$1 v=$2
  archive="${name}_${v}_${os}_${arch}.tar.gz"
  if [ -n "${AMQ_DOWNLOAD_URL:-}" ]; then
    base="${AMQ_DOWNLOAD_URL%/}/v$v"
  else
    base="https://github.com/$AMQ_REPO/releases/download/v$v"
  fi
  [ -s "$tmp/amq-checksums-$v" ] || fetch "$base/checksums.txt" "$tmp/amq-checksums-$v" ||
    die "could not download $base/checksums.txt; install AMQ $v yourself (https://github.com/$AMQ_REPO/releases)"
  want=$(checksum "$tmp/amq-checksums-$v" "$archive")
  [ -n "$want" ] || die "AMQ $v has no $archive"
  fetch "$base/$archive" "$tmp/$archive" || die "could not download $base/$archive"
  got=$(sha256 "$tmp/$archive")
  [ "$got" = "$want" ] || die "$archive does not match AMQ's checksums.txt (got $got, want $want); refusing it"
  mkdir -p "$tmp/$name.x" "$BIN_DIR"
  tar -xzf "$tmp/$archive" -C "$tmp/$name.x" "$name" || die "$archive has no $name"
  chmod 755 "$tmp/$name.x/$name"
  mv -f "$tmp/$name.x/$name" "$BIN_DIR/$name" || die "could not install $BIN_DIR/$name"
  say "installed $name $v to $BIN_DIR/$name"
}

install_amq() {
  if amq=$(find_amq); then
    have=$("$amq" --version 2>/dev/null | head -n 1 | sed 's/^[^0-9]*//')
    if [ -n "$have" ] && older "$have" "$AMQ_VERSION"; then
      say "warning: $amq is AMQ $have; this adapter is tested with $AMQ_VERSION. Upgrade it (brew upgrade amq, or its release page) and run the reconcile action"
    fi
  else
    have=$AMQ_VERSION
    fetch_amq_tool amq "$AMQ_VERSION"
  fi
  # amq-bridge (cross-machine mail) must come from the same AMQ release.
  if ! command -v amq-bridge >/dev/null 2>&1 && [ ! -x "$BIN_DIR/amq-bridge" ] && [ -z "${AMQ_BRIDGE_BIN:-}" ]; then
    fetch_amq_tool amq-bridge "${have:-$AMQ_VERSION}"
  fi
}

# The binary's final path is known only after Herdr moves the install
# checkout into place; `herdr-amq-adapter configure` (re)links the command
# then. Here, link it only for a checkout that stays where it is.
link_command() {
  case "$PWD" in
    */plugins/.tmp-install-*/*) return ;;
  esac
  link="$BIN_DIR/herdr-amq-adapter"
  if [ -e "$link" ] && [ ! -L "$link" ]; then
    say "left $link alone: it is not a link"
    return
  fi
  mkdir -p "$BIN_DIR" && ln -sfn "$PWD/bin/herdr-amq-adapter" "$link" && say "linked $link"
}

install_adapter
install_amq
link_command
say "done. Next, once: herdr plugin action invoke est7.amq-adapter.configure"
