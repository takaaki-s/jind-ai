#!/bin/sh

set -eu

repository_url="https://github.com/takaaki-s/jind-ai"
version=${JIND_AI_VERSION:-latest}
bin_dir=${JIND_AI_INSTALL_DIR:-}
tmp_dir=
staged_binary=

usage() {
	cat <<'EOF'
Install the latest jind-ai release.

Usage: install.sh [--version VERSION] [--bin-dir DIRECTORY]

Options:
  --version VERSION     Release to install, with or without a leading v
  --bin-dir DIRECTORY   Destination directory (default: ~/.local/bin)
  -h, --help            Show this help

Environment:
  JIND_AI_VERSION       Same as --version
  JIND_AI_INSTALL_DIR   Same as --bin-dir
EOF
}

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	if [ -n "$staged_binary" ]; then
		rm -f "$staged_binary"
	fi
	if [ -n "$tmp_dir" ]; then
		rm -rf "$tmp_dir"
	fi
}

need_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

normalize_version() {
	case "$version" in
	v*) version=${version#v} ;;
	esac
	case "$version" in
	[0-9]*) ;;
	*) die "invalid release version: $version" ;;
	esac
	case "$version" in
	*[!0-9A-Za-z.+-]*) die "invalid release version: $version" ;;
	esac
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--version)
		[ "$#" -ge 2 ] || die "--version requires a value"
		version=$2
		shift 2
		;;
	--bin-dir)
		[ "$#" -ge 2 ] || die "--bin-dir requires a value"
		bin_dir=$2
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown argument: $1" ;;
	esac
done

if [ -z "$bin_dir" ]; then
	[ -n "${HOME:-}" ] || die "HOME is not set; pass --bin-dir"
	bin_dir=$HOME/.local/bin
fi

need_command curl
need_command tar
need_command awk
need_command mktemp

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "unsupported architecture: $(uname -m)" ;;
esac

if [ "$version" = latest ]; then
	latest_url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$repository_url/releases/latest")
	latest_url=${latest_url%/}
	tag=${latest_url##*/}
	case "$tag" in
	v*) version=${tag#v} ;;
	*) die "could not determine the latest release from: $latest_url" ;;
	esac
fi
normalize_version

archive="jind-ai_${version}_${os}_${arch}.tar.gz"
release_url="$repository_url/releases/download/v${version}"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/jind-ai-install.XXXXXX")
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

printf 'Downloading jind-ai %s for %s/%s...\n' "$version" "$os" "$arch"
curl -fsSL -o "$tmp_dir/$archive" "$release_url/$archive"
curl -fsSL -o "$tmp_dir/checksums.txt" "$release_url/checksums.txt"

if ! expected_checksum=$(awk -v file="$archive" '
	$2 == file {
		if (found) exit 2
		print $1
		found = 1
	}
	END { if (!found) exit 1 }
' "$tmp_dir/checksums.txt"); then
	die "checksum entry not found or duplicated for $archive"
fi

if command -v sha256sum >/dev/null 2>&1; then
	actual_checksum=$(sha256sum "$tmp_dir/$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
	actual_checksum=$(shasum -a 256 "$tmp_dir/$archive" | awk '{print $1}')
else
	die "sha256sum or shasum is required to verify the download"
fi

[ "$actual_checksum" = "$expected_checksum" ] || die "checksum verification failed for $archive"

tar -xzf "$tmp_dir/$archive" -C "$tmp_dir" jin
[ -f "$tmp_dir/jin" ] || die "release archive does not contain jin"

mkdir -p "$bin_dir"
staged_binary=$(mktemp "$bin_dir/.jin.XXXXXX")
cp "$tmp_dir/jin" "$staged_binary"
chmod 0755 "$staged_binary"
mv -f "$staged_binary" "$bin_dir/jin"
staged_binary=

printf 'Installed jin %s to %s/jin\n' "$version" "$bin_dir"
case ":${PATH:-}:" in
*":$bin_dir:"*) ;;
*) printf 'Add %s to PATH before running jin.\n' "$bin_dir" ;;
esac
printf 'Run: jin onboard --skill\n'
printf 'When upgrading a running daemon, run: jin daemon restart\n'
