#!/bin/sh
# Usage: curl -fsSL https://raw.githubusercontent.com/ddhjy/grok-commit/main/install.sh | sh
# Optional: GROK_COMMIT_VERSION=v0.2.0, GROK_COMMIT_INSTALL_DIR, GROK_COMMIT_NO_SETUP=1,
# GROK_COMMIT_NO_PATH=1. Installs for the current user; no sudo required.
main() (
    set -eu
    repo=https://github.com/ddhjy/grok-commit
    from_source='go install github.com/ddhjy/grok-commit/cmd/grok-commit@latest'
    fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
    command -v curl >/dev/null 2>&1 || fail 'The installer needs curl to download grok-commit. Install curl, then run the installer again.'
    command -v tar >/dev/null 2>&1 || fail 'The installer needs tar to unpack grok-commit. Install tar, then run the installer again.'
    case "$(uname -s)" in
        Darwin) platform=darwin platform_name=macOS ;;
        Linux) platform=linux platform_name=Linux ;;
        MINGW*|MSYS*|CYGWIN*) fail 'On Windows, install grok-commit from PowerShell instead:
  irm https://raw.githubusercontent.com/ddhjy/grok-commit/main/install.ps1 | iex' ;;
        *) fail "This installer supports macOS and Linux, not $(uname -s). To build grok-commit from source instead, run: $from_source" ;;
    esac
    case "$(uname -m)" in
        arm64|aarch64) arch=arm64 ;;
        x86_64|amd64) arch=amd64 ;;
        *) fail "grok-commit is built for arm64 and amd64 (x86_64) processors, not $(uname -m). To build it from source instead, run: $from_source" ;;
    esac
    if command -v sha256sum >/dev/null 2>&1; then checksum=sha256sum
    elif command -v shasum >/dev/null 2>&1; then checksum=shasum
    else fail 'The installer needs sha256sum or shasum to verify the download. Install either one, then run the installer again.'; fi
    fetch() { curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 15 --max-time 180 "$1" -o "$2"; }
    work=$(mktemp -d)
    candidate=
    trap 'rm -rf "$work"; if [ -n "$candidate" ]; then rm -f "$candidate"; fi' EXIT
    trap 'exit 130' INT TERM
    stable='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
    if [ -n "${GROK_COMMIT_VERSION:-}" ]; then
        tag=v${GROK_COMMIT_VERSION#v}
        printf '%s\n' "$tag" | LC_ALL=C grep -Eq "$stable" ||
            fail "GROK_COMMIT_VERSION=$GROK_COMMIT_VERSION isn't a release version. Use one such as v0.2.0, or leave it unset to install the latest release."
    else
        fetch https://api.github.com/repos/ddhjy/grok-commit/releases/latest "$work/release.json" ||
            fail "Couldn't look up the latest release on GitHub. Check your internet connection, then try again."
        tag=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$work/release.json")
        tag=v${tag#v}
        printf '%s\n' "$tag" | LC_ALL=C grep -Eq "$stable" ||
            fail "GitHub didn't report a stable release of grok-commit, so nothing was installed. Try again later."
    fi
    version=${tag#v}
    archive=grok-commit_${version}_${platform}_${arch}.tar.gz
    printf 'Installing grok-commit %s for %s (%s)...\n' "$version" "$platform_name" "$arch"
    fetch "$repo/releases/download/$tag/$archive" "$work/$archive" ||
        fail "Couldn't download grok-commit $version. Check your internet connection, and that this version exists at $repo/releases."
    fetch "$repo/releases/download/$tag/checksums.txt" "$work/checksums.txt" ||
        fail "Couldn't download the checksums for grok-commit $version, so nothing was installed. Try again."
    expected=$(awk -v name="$archive" '$2 == name { print $1 }' "$work/checksums.txt")
    printf '%s\n' "$expected" | LC_ALL=C grep -Eq '^[0-9a-f]{64}$' ||
        fail "The release's checksum file doesn't list $archive, so nothing was installed."
    if [ "$checksum" = shasum ]; then actual=$(shasum -a 256 "$work/$archive" | awk '{print $1}')
    else actual=$(sha256sum "$work/$archive" | awk '{print $1}'); fi
    [ "$actual" = "$expected" ] || fail "The download didn't match its published checksum, so nothing was installed. Try again."
    # Only extract the named executable from our verified release archive.
    tar -xzf "$work/$archive" -C "$work" grok-commit || fail "Couldn't unpack the release package (see the message above), so nothing was installed."
    [ -f "$work/grok-commit" ] && [ ! -L "$work/grok-commit" ] ||
        fail "The release package doesn't contain a valid grok-commit program, so nothing was installed."
    install_dir=${GROK_COMMIT_INSTALL_DIR:-"$HOME/.local/bin"}
    mkdir -p "$install_dir" || fail "Couldn't create $install_dir. Choose another folder with GROK_COMMIT_INSTALL_DIR, then run the installer again."
    install_dir=$(cd "$install_dir" && pwd -P)
    candidate=$(mktemp "$install_dir/.grok-commit.XXXXXX") ||
        fail "Couldn't write to $install_dir. Check its permissions, or choose another folder with GROK_COMMIT_INSTALL_DIR."
    cat "$work/grok-commit" > "$candidate"
    chmod 755 "$candidate"
    [ "$("$candidate" version)" = "grok-commit $version" ] || fail "The downloaded grok-commit didn't pass its version check, so nothing was installed."
    mv -f "$candidate" "$install_dir/grok-commit"
    candidate=
    updates=ok
    "$install_dir/grok-commit" __install || updates=failed
    printf '✓ Installed grok-commit %s at %s/grok-commit\n' "$version" "$install_dir"
    if [ "$updates" = failed ]; then
        printf "Automatic updates couldn't be set up (see the message above). To try again, run: grok-commit update --enable\n"
    fi

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
                printf 'Added %s to your PATH in %s.\n' "$install_dir" "$profile"
            fi
            printf 'Open a new terminal to use the grok-commit command.\n'
        else printf 'To use the grok-commit command, add %s to your PATH.\n' "$install_dir"; fi
        ;;
    esac
    if [ "${GROK_COMMIT_NO_SETUP:-0}" = 1 ]; then
        printf 'Next, connect to Grok: grok-commit setup\n'
    elif [ -t 1 ] && ( : </dev/tty ) 2>/dev/null; then
        printf '\n'
        "$install_dir/grok-commit" setup </dev/tty || :
    else
        "$install_dir/grok-commit" setup --yes || :
    fi
)
main "$@"
