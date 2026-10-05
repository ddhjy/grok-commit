#!/bin/sh
# Offline end-to-end installer test. All writes stay in this temporary directory.
set -eu
test_dir=$(mktemp -d)
test_dir=$(cd "$test_dir" && pwd -P)
trap 'rm -rf "$test_dir"' EXIT
mkdir -p "$test_dir/bin" "$test_dir/release"
go build -o "$test_dir/release/grok-commit" -ldflags '-X main.version=0.2.0' ./cmd/grok-commit
os=$(go env GOOS)
arch=$(go env GOARCH)
archive=grok-commit_0.2.0_${os}_${arch}.tar.gz
tar -czf "$test_dir/release/$archive" -C "$test_dir/release" grok-commit
(cd "$test_dir/release" && shasum -a 256 "$archive" > checksums.txt)
cat > "$test_dir/bin/curl" <<'MOCK'
#!/bin/sh
set -eu
url= dest=
while [ "$#" -gt 0 ]; do
    case "$1" in
        https://*) url=$1 ;;
        -o) shift; dest=$1 ;;
    esac
    shift
done
case "$url" in
    https://api.github.com/repos/ddhjy/grok-commit/releases/latest) printf '{"tag_name":"v0.2.0"}\n' > "$dest" ;;
    https://github.com/ddhjy/grok-commit/releases/download/v0.2.0/*) cp "$INSTALL_TEST_FIXTURES/${url##*/}" "$dest" ;;
    *) exit 12 ;;
esac
MOCK
chmod +x "$test_dir/bin/curl"
export PATH="$test_dir/bin:$PATH"
export INSTALL_TEST_FIXTURES="$test_dir/release"
export GROK_COMMIT_CONFIG_DIR="$test_dir/config"
export GROK_COMMIT_STATE_DIR="$test_dir/state"
# Exercise quoting, spaces, shell metacharacters, and duplicate PATH protection.
export GROK_COMMIT_INSTALL_DIR="$test_dir/install with spaces/\$literal\`text"
export GROK_COMMIT_PROFILE_FILE="$test_dir/profile"
export GROK_COMMIT_NO_SETUP=1
export GROK_COMMIT_NO_PATH=0
export GROK_COMMIT_VERSION=
export SHELL=/bin/bash
sh ./install.sh
sh ./install.sh
binary="$GROK_COMMIT_INSTALL_DIR/grok-commit"
[ "$("$binary" version)" = 'grok-commit 0.2.0' ]
[ "$(grep -c '^export PATH=' "$GROK_COMMIT_PROFILE_FILE")" = 1 ]
sh -c '. "$1"; command -v grok-commit' sh "$GROK_COMMIT_PROFILE_FILE" | grep -Fx "$binary"
"$binary" update --status | grep -F 'Check interval: 7 days'
printf 'broken package' > "$test_dir/release/$archive"
if sh ./install.sh; then printf 'ERROR: accepted a corrupted archive\n' >&2; exit 1; fi
[ "$("$binary" version)" = 'grok-commit 0.2.0' ]
# Exercise the real rollback command using two native release builds.
go build -o "$binary.previous" -ldflags '-X main.version=0.1.0' ./cmd/grok-commit
previous_hash=$(shasum -a 256 "$binary.previous" | awk '{print $1}')
state=$(find "$GROK_COMMIT_STATE_DIR/updates" -name state.json)
printf '{"installed":"0.2.0","previous":"0.1.0","previous_sha256":"%s"}\n' "$previous_hash" > "$state"
"$binary" update --rollback
[ "$("$binary" version)" = 'grok-commit 0.1.0' ]
grep -F '"skip_version": "v0.2.0"' "$state"
printf 'Installer, integrity rejection, PATH, and rollback passed.\n'
