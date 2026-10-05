#!/bin/sh
# Usage: curl -fsSL https://raw.githubusercontent.com/ddhjy/grok-commit/main/install.sh | sh
# Optional: GROK_COMMIT_VERSION=v0.2.0, GROK_COMMIT_INSTALL_DIR, GROK_COMMIT_NO_SETUP=1,
# GROK_COMMIT_NO_PATH=1. Installs for the current user; no sudo required.
main() (
    set -eu
    repo=https://github.com/ddhjy/grok-commit
    fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
    command -v curl >/dev/null 2>&1 || fail 'curl is required.'
    command -v tar >/dev/null 2>&1 || fail 'tar is required.'
    case "$(uname -s)" in Darwin) platform=darwin ;; Linux) platform=linux ;; *) fail 'Use install.ps1 on Windows.' ;; esac
    case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) fail 'Supported architectures: arm64 and amd64.' ;; esac
    if command -v sha256sum >/dev/null 2>&1; then checksum=sha256sum
    elif command -v shasum >/dev/null 2>&1; then checksum=shasum
    else fail 'sha256sum or shasum is required to verify the download.'; fi
    fetch() { curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 15 --max-time 180 "$1" -o "$2"; }
    work=$(mktemp -d)
    candidate=
    trap 'rm -rf "$work"; if [ -n "$candidate" ]; then rm -f "$candidate"; fi' EXIT
    trap 'exit 130' INT TERM
    tag=${GROK_COMMIT_VERSION:-}
    if [ -z "$tag" ]; then
        fetch https://api.github.com/repos/ddhjy/grok-commit/releases/latest "$work/release.json"
        tag=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$work/release.json")
    fi
    tag=v${tag#v}
    printf '%s\n' "$tag" | LC_ALL=C grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || fail 'Could not find a stable release.'
    version=${tag#v}
    archive=grok-commit_${version}_${platform}_${arch}.tar.gz
    printf 'Installing grok-commit %s (%s/%s)...\n' "$tag" "$platform" "$arch"
    fetch "$repo/releases/download/$tag/$archive" "$work/$archive"
    fetch "$repo/releases/download/$tag/checksums.txt" "$work/checksums.txt"
    expected=$(awk -v name="$archive" '$2 == name { print $1 }' "$work/checksums.txt")
    printf '%s\n' "$expected" | LC_ALL=C grep -Eq '^[0-9a-f]{64}$' || fail 'Invalid or missing archive checksum.'
    if [ "$checksum" = shasum ]; then actual=$(shasum -a 256 "$work/$archive" | awk '{print $1}')
    else actual=$(sha256sum "$work/$archive" | awk '{print $1}'); fi
    [ "$actual" = "$expected" ] || fail 'Checksum mismatch; installation unchanged.'
    # Only extract the named executable from our verified release archive.
    tar -xzf "$work/$archive" -C "$work" grok-commit
    [ -f "$work/grok-commit" ] && [ ! -L "$work/grok-commit" ] || fail 'Invalid release executable.'
    install_dir=${GROK_COMMIT_INSTALL_DIR:-"$HOME/.local/bin"}
    mkdir -p "$install_dir"
    install_dir=$(cd "$install_dir" && pwd -P)
    candidate=$(mktemp "$install_dir/.grok-commit.XXXXXX")
    cat "$work/grok-commit" > "$candidate"
    chmod 755 "$candidate"
    [ "$("$candidate" version)" = "grok-commit $version" ] || fail 'Executable version check failed.'
    mv -f "$candidate" "$install_dir/grok-commit"
    candidate=
    "$install_dir/grok-commit" __install

    case ":$PATH:" in *":$install_dir:"*) ;; *)
        if [ "${GROK_COMMIT_NO_PATH:-0}" != 1 ]; then
            shell_name=${SHELL:-sh}
            shell_name=${shell_name##*/}
            case "$shell_name" in
                zsh) profile=${ZDOTDIR:-$HOME}/.zshrc ;;
                bash) if [ "$platform" = darwin ]; then profile=$HOME/.bash_profile; else profile=$HOME/.bashrc; fi ;;
                fish) profile=${XDG_CONFIG_HOME:-$HOME/.config}/fish/conf.d/grok-commit.fish ;;
                *) profile=$HOME/.profile ;;
            esac
            # A dedicated override is useful for managed provisioning and tests.
            profile=${GROK_COMMIT_PROFILE_FILE:-$profile}
            mkdir -p "$(dirname "$profile")"
            escaped=$(printf '%s' "$install_dir" | sed 's/[\\"$`]/\\&/g')
            if [ "$shell_name" = fish ]; then path_line="fish_add_path \"$escaped\""
            else path_line="export PATH=\"$escaped:\$PATH\""; fi
            if [ ! -f "$profile" ] || ! grep -Fqx "$path_line" "$profile"; then
                printf '\n# grok-commit\n%s\n' "$path_line" >> "$profile"
            fi
            printf 'PATH configured in %s. Open a new terminal to use grok-commit by name.\n' "$profile"
        else printf 'Add %s to PATH to use grok-commit by name.\n' "$install_dir"; fi
        ;;
    esac
    printf 'Installed: %s/grok-commit\n' "$install_dir"
    if [ "${GROK_COMMIT_NO_SETUP:-0}" = 1 ]; then
        printf 'Next: grok-commit setup\n'
    elif [ -t 1 ] && ( : </dev/tty ) 2>/dev/null; then
        "$install_dir/grok-commit" setup </dev/tty || printf 'Installed successfully. Finish later with: grok-commit setup\n'
    else
        "$install_dir/grok-commit" setup --yes || printf 'Installed successfully. Run grok-commit setup in a terminal to connect your Grok account.\n'
    fi
)
main "$@"
