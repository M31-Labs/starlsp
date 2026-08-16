#!/usr/bin/env bash
#
# Install the built VS Code extension, and verify that it landed.
#
# Why this is not just `code --install-extension file.vsix`:
#
# Under WSL, `code` is a shim onto the Windows build. Given a path inside the
# WSL filesystem it hands Windows VS Code a \\wsl.localhost UNC URL, and
# unpacking the archive over that share fails intermittently:
#
#     code: 'Extract', name: 'Extract'
#     Failed Installing Extensions: file://wsl.localhost/Ubuntu/.../starlsp.vsix
#
# It is intermittent, not deterministic — the same file installs on one attempt
# and fails on the next, and one failure came with a WSL vsock error in the same
# output. The trigger is not established. So this script does not try to explain
# it; it avoids the share by staging the archive on Windows-native storage,
# retries, and then checks the extension is actually installed rather than
# trusting an exit code.
#
# Outside WSL it is a plain install with the same verification.

set -euo pipefail

EXT_ID="m31labs.starlsp"
ATTEMPTS=3

die() { printf 'install-vscode-extension: %s\n' "$*" >&2; exit 1; }
note() { printf '%s\n' "$*"; }

vsix_path="${1:-}"
[[ -n "$vsix_path" ]] || die "usage: $0 <path-to-vsix>"
[[ -f "$vsix_path" ]] || die "no such file: $vsix_path"

command -v code >/dev/null 2>&1 || die "the 'code' command is not on PATH; install the VS Code CLI"

vsix_path="$(cd "$(dirname "$vsix_path")" && pwd)/$(basename "$vsix_path")"
vsix_name="$(basename "$vsix_path")"

# Sanity-check the archive before handing it to the editor, so a packaging bug
# is not misreported as an install failure.
if command -v unzip >/dev/null 2>&1; then
  unzip -tqq "$vsix_path" >/dev/null 2>&1 || die "the archive is corrupt: $vsix_path"
fi

is_wsl() {
  [[ -n "${WSL_DISTRO_NAME:-}" ]] || grep -qi microsoft /proc/version 2>/dev/null
}

# windows_staging_dir echoes a directory on Windows-native storage, or nothing
# when one cannot be found.
windows_staging_dir() {
  local base
  for base in "${LOCALAPPDATA_WSL:-}" /mnt/c/Users/"${WIN_USER:-$USER}"/AppData/Local/Temp /mnt/c/Windows/Temp; do
    [[ -n "$base" && -d "$base" ]] || continue
    local dir="$base/starlsp-install"
    if mkdir -p "$dir" 2>/dev/null; then
      printf '%s' "$dir"
      return 0
    fi
  done
  # Fall back to any /mnt/c user profile with a Temp directory.
  local candidate
  for candidate in /mnt/c/Users/*/AppData/Local/Temp; do
    [[ -d "$candidate" ]] || continue
    local dir="$candidate/starlsp-install"
    if mkdir -p "$dir" 2>/dev/null; then
      printf '%s' "$dir"
      return 0
    fi
  done
  return 1
}

installed_version() {
  # Ask the editor rather than reading a directory name, so this works for both
  # a WSL remote and a local install.
  code --list-extensions --show-versions 2>/dev/null \
    | awk -F@ -v id="$EXT_ID" '$1 == id {print $2; exit}'
}

install_from() {
  local dir="$1" name="$2"
  ( cd "$dir" && code --install-extension "$name" --force ) 2>&1
}

want_version="$(basename "$vsix_name" .vsix)"
want_version="${want_version##*-}"

staging_dir="$(dirname "$vsix_path")"
staging_note="the extension directory"

if is_wsl; then
  if win_dir="$(windows_staging_dir)"; then
    cp -f "$vsix_path" "$win_dir/$vsix_name"
    staging_dir="$win_dir"
    staging_note="Windows-native storage (avoiding the \\\\wsl.localhost share)"
  else
    note "note: no Windows-native staging directory found; installing over the WSL share, which can fail intermittently"
  fi
fi

note "installing $vsix_name from $staging_note"

for attempt in $(seq 1 "$ATTEMPTS"); do
  if output="$(install_from "$staging_dir" "$vsix_name")"; then
    if [[ "$output" == *"successfully installed"* ]]; then
      break
    fi
  fi
  if (( attempt == ATTEMPTS )); then
    printf '%s\n' "$output" >&2
    die "install failed after $ATTEMPTS attempts"
  fi
  note "attempt $attempt failed; retrying"
  sleep 2
done

# Trust nothing: confirm the editor reports it.
got="$(installed_version || true)"
if [[ -z "$got" ]]; then
  die "the editor does not list $EXT_ID after a reportedly successful install"
fi
if [[ -n "$want_version" && "$got" != "$want_version" ]]; then
  die "installed version is $got, expected $want_version"
fi

note "installed $EXT_ID@$got"
note
note "Reload VS Code so the editor loads it:"
note "  Ctrl+Shift+P -> Developer: Reload Window"
