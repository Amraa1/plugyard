#!/bin/sh
# Installs the latest release of plugyard on Linux or macOS, or updates it.
#
#   curl -fsSL https://raw.githubusercontent.com/Amraa1/plugyard/main/install.sh | sh
#
# It goes into ~/.local/bin unless PLUGYARD_INSTALL_DIR names another folder.
# PLUGYARD_RELEASES_URL names another copy of the releases, such as a mirror.
set -eu

repo="${PLUGYARD_RELEASES_URL:-https://github.com/Amraa1/plugyard}"
repo="${repo%/}"
dir="${PLUGYARD_INSTALL_DIR:-$HOME/.local/bin}"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "plugyard has no build for $(uname -s). On Windows, use install.ps1." >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "plugyard has no build for $(uname -m) processors." >&2; exit 1 ;;
esac

asset="plugyard_${os}_${arch}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $asset"
curl -fsSL "$repo/releases/latest/download/$asset" -o "$tmp/$asset"
curl -fsSL "$repo/releases/latest/download/checksums.txt" -o "$tmp/checksums.txt"

# The download must match the checksum the release lists for it.
want="$(awk -v name="$asset" '$2 == name { print $1 }' "$tmp/checksums.txt")"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/$asset" | awk '{ print $1 }')"
else
  got="$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')"
fi
if [ -z "$want" ] || [ "$want" != "$got" ]; then
  echo "$asset does not match its checksum; nothing was installed." >&2
  exit 1
fi

tar -xzf "$tmp/$asset" -C "$tmp" plugyard
mkdir -p "$dir"
# Moving the new program into place works even while the old one runs.
chmod 755 "$tmp/plugyard"
mv -f "$tmp/plugyard" "$dir/plugyard"

echo "Installed $("$dir/plugyard" version | head -n 1 | awk '{ print "plugyard " $2 }') in $dir"
case ":$PATH:" in
  *":$dir:"*) echo "Try it: plugyard init my-site" ;;
  *)
    echo "$dir is not on your PATH. Add this line to your shell's startup file:"
    echo "  export PATH=\"$dir:\$PATH\""
    ;;
esac
