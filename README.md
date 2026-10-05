# grok-commit

A fast, standalone Git commit CLI powered by **Grok**. One native binary, no Python or Node.js, no Go dependencies beyond the standard library.

```text
$ grok-commit -a

Generating (grok-4.3)... ✓

→ feat: support ordered paths for shared aliases

✓ Committed in 1.234s
```

The example illustrates the output format, not a latency guarantee. Network, model load, repository size, Git hooks, signing and authentication affect elapsed time.

## Install

Download the archive for your OS and architecture from [GitHub Releases](https://github.com/ddhjy/grok-commit/releases). Each release includes macOS and Linux `.tar.gz` files, Windows `.zip` files, and `checksums.txt`.

For example, on an Apple Silicon Mac:

```sh
curl -fLO https://github.com/ddhjy/grok-commit/releases/download/v0.1.0/grok-commit_0.1.0_darwin_arm64.tar.gz
curl -fLO https://github.com/ddhjy/grok-commit/releases/download/v0.1.0/checksums.txt
shasum -a 256 -c checksums.txt --ignore-missing
tar -xzf grok-commit_0.1.0_darwin_arm64.tar.gz
mkdir -p "$HOME/.local/bin"
install -m 755 grok-commit "$HOME/.local/bin/grok-commit"
```

Ensure `~/.local/bin` is on `PATH`. On Windows, extract the ZIP and add its directory to your user `PATH`. Git must already be installed. Release binaries are not Developer ID/Authenticode signed.

With Go 1.23 or newer:

```sh
go install github.com/ddhjy/grok-commit/cmd/grok-commit@latest
```

## Authenticate

Choose either option:

1. **xAI API key (recommended for standalone use):** set `XAI_API_KEY`. The tool calls the [public xAI API](https://docs.x.ai/developers/quickstart); API usage follows your API account's billing.
2. **Existing Grok CLI login:** if no API key is configured, the tool reads the installed Grok CLI's login and version and uses its first-party endpoint. Run `grok` and sign in first. This compatibility mode depends on the CLI's authentication format and endpoint, which may change. It does not copy or print your login token.

To save an API key for future commands or a login service, pass it on stdin:

```sh
printf '%s' "$XAI_API_KEY" | grok-commit auth --stdin
unset XAI_API_KEY
grok-commit doctor
```

Keys are stored in your user configuration directory, never in the repository. On Unix, the directory is `0700` and the credentials file is `0600`. Do not commit credentials. `--auth api` or `--auth cli` selects a mode explicitly. Expired CLI authentication must be refreshed with the official Grok CLI.

## Use

```sh
grok-commit                  # Stage tracked changes, generate, commit
grok-commit -a               # Include untracked files
grok-commit -aps             # All changes + push + historical style
grok-commit --no-stage       # Commit only what is already staged
grok-commit --dry-run        # Preview staged changes without changing the index
grok-commit --no-cache --profile
grok-commit --no-daemon      # Direct request, useful for scripts/CI
```

Optional shell shortcut:

```sh
alias aa='grok-commit -a'
```

`--dry-run` never stages, commits or pushes, even with `-a`. Stage files yourself before previewing. Git hooks and signing are respected by default; use `--no-verify` only when you explicitly want to skip commit hooks. A failed push reports that the local commit already succeeded.

Default output is concise. `--profile` prints local preparation, staging, diff, generation, commit and total timings to stderr. The generation time includes worker startup if needed; cache hits do not contact Grok.

## Model, style and rules

The default is `grok-4.3` with `reasoning_effort=none`. Other Grok models default to `low`. Model access depends on your account/endpoint; use `--model` and `--reasoning` to override. The tool never switches to a non-Grok model.

```sh
grok-commit --model grok-4.3 --reasoning none
grok-commit --model grok-4.7 --reasoning low
```

Default subjects are English conventional commits, up to 72 Unicode characters. `-s` includes the last ten subjects to match language and style. Add `.grok-commit-rules` in the repository root for project rules, for example:

```text
Write subjects in Chinese. Include a scope when changing config or CLI behavior.
```

The legacy `.bunnygit/rules/commit` path is supported when `.grok-commit-rules` is absent. The staged diff, selected history, and project rules are sent to the chosen Grok endpoint. Large diffs use an explicitly labelled, bounded summary with excerpts; not every line is included.

## Fast connections

On macOS and Linux, the first command starts a private local worker. It reuses HTTP connections across commands and exits after 20 minutes idle. Use these commands to control it:

```sh
grok-commit daemon start     # Start and prewarm; no model generation
grok-commit daemon status
grok-commit daemon warm
grok-commit daemon stop
```

On macOS, opt into automatic prewarming at login:

```sh
grok-commit daemon install
grok-commit daemon uninstall
```

Install the binary at its final location first. With an API key, save it using `auth --stdin` and unset `XAI_API_KEY` before installing the service. On Linux, use the on-demand worker or run `grok-commit daemon run --keepalive` under your own service manager. Windows currently uses direct requests; automatic background connection reuse is available on macOS/Linux only.

The worker refreshes its connection with a model-list request every 25 seconds. It never watches repositories or generates subjects in the background. Requests use a private Unix socket. After replacing the binary or updating the Grok CLI, stop/restart the worker (or reinstall the login service) to load the new version.

If generation is still pending after 700 ms, one additional Grok request starts; the first valid completed result wins. **A slow call can consume two generations.** Set `--hedge-delay 0` to disable this behavior. Cancellation stops outstanding requests, but already generated tokens may still be billed.

An exact-input cache stores only validated subjects and timestamps for seven days, up to 512 entries. Its key includes the endpoint, model, reasoning level, system prompt and full user prompt. A changed diff, history or rule invalidates the entry. `--no-cache` disables both cache reads and writes.

## Configuration

Optional `config.json` in the OS user configuration directory:

| OS | Configuration directory |
| --- | --- |
| macOS | `~/Library/Application Support/grok-commit` |
| Linux | `${XDG_CONFIG_HOME:-~/.config}/grok-commit` |
| Windows | `%AppData%\grok-commit` |

```json
{
  "model": "grok-4.3",
  "auth": "auto",
  "timeout": "30s",
  "hedge_delay": "700ms"
}
```

Precedence: command flags → environment → config file → defaults. Environment variables: `GROK_COMMIT_MODEL`, `GROK_COMMIT_REASONING`, `GROK_COMMIT_AUTH`, `GROK_COMMIT_BASE_URL`, `GROK_COMMIT_TIMEOUT`, `GROK_COMMIT_HEDGE_DELAY`. `GROK_COMMIT_CACHE=0`, `GROK_COMMIT_DAEMON=0` and `GROK_COMMIT_PROFILE=1` control behavior. Directory overrides: `GROK_COMMIT_CONFIG_DIR`, `GROK_COMMIT_STATE_DIR`; `GROK_HOME` locates a non-default CLI login.

State, cached subjects and the worker's diagnostic log live under the OS user cache directory (`grok-commit`). Prompts, diffs and keys are not written to these logs. Custom HTTPS base URLs are available in API-key mode; the CLI login can only be sent to the first-party CLI endpoint. Redirects are rejected rather than forwarding authentication.

## Correctness and performance

The model must return a successful terminal `stop` event and a valid single-line subject. Partial, truncated, refused, tool-call and malformed responses do not commit. The prompt uses a captured Git tree, and the index is checked again before committing; if it changed, the command stops. Hooks may add time or reject the commit.

Measure separately: cold worker startup, warm generation with `--no-cache`, cache hits, and hooks/signing. Include the whole command's wall time and report P50/P95 over varied diffs. A network-dependent tool cannot guarantee every call finishes under two seconds.

## Development and releases

```sh
make build
make test
make check
make package VERSION=0.1.0
```

Tests use disposable repositories and local fake HTTP servers, not real model credentials. CI runs on macOS, Linux and Windows. The race detector runs on macOS/Linux. Tagging `v*` builds six archives and publishes a GitHub Release with SHA-256 checksums. No Go module downloads are needed.

MIT licensed.
