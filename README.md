# grok-commit

A fast, standalone Git commit CLI powered by **Grok**. One native binary. No Python, Node.js, or Go installation needed to use it.

```text
$ grok-commit -a

Writing the commit subject with grok-4.3... ✓

→ feat: support ordered paths for shared aliases

✓ Committed in 1.234s
```

The example illustrates the output format, not a latency guarantee. Network, model load, repository size, Git hooks, signing and authentication affect elapsed time.

## Install

**macOS / Linux** (Apple Silicon, Intel, amd64, arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/ddhjy/grok-commit/main/install.sh | sh
```

**Windows PowerShell** (amd64 / arm64):

```powershell
irm https://raw.githubusercontent.com/ddhjy/grok-commit/main/install.ps1 | iex
```

The installer selects your platform, checks the release's SHA-256 checksum, installs for your user without administrator privileges, configures PATH, and runs setup. On macOS/Linux, open a new terminal after installation to pick up PATH changes. Install locations: `~/.local/bin/grok-commit` or `%LocalAppData%\Programs\grok-commit\grok-commit.exe`.

Setup reuses existing Grok credentials, or guides you through hidden API-key entry / signing in with an installed Grok CLI. It checks Git and the Grok connection without generating a subject or committing anything. A new key is saved only after Grok accepts it; if Grok rejects it, setup asks again. **You need Git and a Grok account/API key once; credentials cannot be bundled with the tool.** If setup is interrupted or you're offline, the binary stays installed and nothing is saved. Resume with:

```sh
grok-commit setup
cd your-repository
grok-commit -a
```

Running the tool interactively without credentials also opens setup before making index changes, then tells you which command to run again. In scripts/CI, missing credentials produce an actionable message without prompting. Use `setup --yes` to validate existing credentials noninteractively.

For managed provisioning, set `GROK_COMMIT_INSTALL_DIR`, `GROK_COMMIT_VERSION=v0.2.0`, `GROK_COMMIT_NO_SETUP=1`, or `GROK_COMMIT_NO_PATH=1` before running the installer. You can inspect/download the installer first or install manually from [GitHub Releases](https://github.com/ddhjy/grok-commit/releases), then run `grok-commit setup`. Release binaries are not Developer ID/Authenticode signed.

With **Go 1.26 or newer**, you can also build from source:

```sh
go install github.com/ddhjy/grok-commit/cmd/grok-commit@latest
grok-commit setup
```

## Quiet automatic updates

Official installations default to **one check every 7 days**, starting seven days after installation. After a successful normal command, a detached helper checks GitHub for a newer stable release. It downloads the matching binary, verifies its SHA-256 checksum and version, and replaces the executable. The next invocation uses it. No update prompt, daily reminder, or network wait is added to committing. Nothing wakes up while you aren't using the tool; CI does not trigger automatic checks.

- Offline, rate-limited, or failed update? Keep the working version and wait until the next scheduled check. No retry on every command or every day.
- Only stable releases from this repository are accepted. Concurrent commands share an update lock. Existing commands finish with their original executable; Windows waits briefly if it is still in use.
- The previous binary is retained. Rollback verifies it and prevents automatically reinstalling the version you just rolled back. A later release can still update normally.
- No Grok credentials, diffs, or repository information are sent to GitHub. GitHub sees ordinary release-download requests.
- Package-manager installations and development builds are not automatically replaced. Follow the package manager's update process.

```sh
grok-commit update --status          # Version, next check, last error, rollback availability
grok-commit update --interval 14d    # Check every two weeks instead (1–365 days)
grok-commit update --disable         # Opt out
grok-commit update --enable          # Opt back in
grok-commit update --check           # Check now without installing
grok-commit update                   # Install latest stable now
grok-commit update --rollback        # Restore previous binary
```

Windows manual install/rollback runs in a detached helper too; use `update --status` to see completion. `GROK_COMMIT_AUTO_UPDATE=0` disables automatic updates for the current environment. Disabling updates also stops an in-progress download from being applied when the helper next checks the setting. Manual updates remain available.

**Upgrading from v0.1.0:** run the installer once to get setup and automatic updates. Future versions then update automatically. Downgrading to a release predating this updater requires rerunning the installer to return to the update-capable version.

## Authenticate

The recommended first step is `grok-commit setup`. To replace an expired key or switch accounts, use `grok-commit setup --auth api` or `grok-commit setup --auth cli`. For manual configuration, choose either option:

1. **xAI API key (recommended for standalone use):** set `XAI_API_KEY`. The tool calls the [public xAI API](https://docs.x.ai/developers/quickstart); API usage follows your API account's billing.
2. **Existing Grok CLI login:** if no API key is configured, the tool reads the installed Grok CLI's login and version and uses its first-party endpoint. Run `grok login` and sign in first. This compatibility mode depends on the CLI's authentication format and endpoint, which may change. It does not copy or print your login token.

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

Run `grok-commit --help` for every option, or `grok-commit <command> --help` (for example `grok-commit update --help`) for one command. A mistyped option or command gets a suggested correction, and nothing is staged or sent to Grok.

Optional shell shortcut:

```sh
alias aa='grok-commit -a'
```

`--dry-run` never stages, commits or pushes, even with `-a`. Stage files yourself before previewing; afterwards it prints the exact command that commits the same staged changes with the same options. When nothing is staged, the message says why (for example, new files are included only with `-a`) and what to run. Git hooks and signing are respected by default; if a hook rejects the commit, your changes stay staged, and `--no-verify` skips commit hooks when you explicitly want that. A failed push reports that the local commit already succeeded.

Default output is concise. `--profile` prints local preparation, staging, diff, generation, commit and total timings to stderr. The generation time includes starting the background service if needed; cache hits do not contact Grok.

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

Rules are limited to 32 KiB and must resolve to a regular file inside the repository.

## Fast connections

On macOS and Linux, the first command starts a private background service. It reuses HTTP connections across commands and exits after 20 minutes idle. Use these commands to control it (`grok-commit daemon --help` explains each one):

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

Install the binary at its final location first. A login service can't read `XAI_API_KEY` from your shell, so with an API key, save it using `grok-commit setup --auth api` (or `auth --stdin`) and unset `XAI_API_KEY` before installing the service. On Linux, use the on-demand background service or run `grok-commit daemon run --keepalive` under your own service manager. Windows currently uses direct requests; automatic background connection reuse is available on macOS/Linux only.

The background service refreshes its connection with a model-list request every 25 seconds. It never watches repositories or generates subjects in the background. Requests use a private Unix socket. From v0.2.0 onward, each release uses its own service address; a replaced service drains in-flight requests and exits, so new invocations use the new code. After updating the Grok CLI itself, stop/restart the background service (or reinstall the login service) to refresh its client version. Background services from v0.1.0 exit on their usual idle timeout; reinstall a v0.1.0 login service once if you enabled one.

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
  "hedge_delay": "700ms",
  "auto_update": true,
  "update_interval": "7d"
}
```

Precedence: command flags → environment → config file → defaults. Environment variables: `GROK_COMMIT_MODEL`, `GROK_COMMIT_REASONING`, `GROK_COMMIT_AUTH`, `GROK_COMMIT_BASE_URL`, `GROK_COMMIT_TIMEOUT`, `GROK_COMMIT_HEDGE_DELAY`. `GROK_COMMIT_CACHE=0`, `GROK_COMMIT_DAEMON=0` and `GROK_COMMIT_PROFILE=1` control behavior. Directory overrides: `GROK_COMMIT_CONFIG_DIR`, `GROK_COMMIT_STATE_DIR`; `GROK_HOME` locates a non-default CLI login.

State, cached subjects and the background service's diagnostic log live under the OS user cache directory (`grok-commit`). Prompts, diffs and keys are not written to these logs. Custom HTTPS base URLs are available in API-key mode; the CLI login can only be sent to the first-party CLI endpoint. Redirects are rejected rather than forwarding authentication.

## Correctness and performance

The model must return a successful terminal `stop` event and a valid single-line subject. Partial, truncated, refused, tool-call and malformed responses do not commit. The prompt uses a captured Git tree, and the index is checked again before committing; if it changed, the command stops. Hooks may add time or reject the commit.

Measure separately: cold background-service startup, warm generation with `--no-cache`, cache hits, and hooks/signing. Include the whole command's wall time and report P50/P95 over varied diffs. A network-dependent tool cannot guarantee every call finishes under two seconds.

Release validation on macOS arm64, using Grok 4.3 through an existing CLI login:

| Scenario | Complete command wall time |
| --- | --- |
| Cold background service, no local result cache (1 sample) | 1.880 s |
| Warm connection, no local result cache (6 samples) | median 0.925 s; max / nearest-rank P95 1.005 s |
| Exact-input cache hit (1 sample) | 0.146 s |

These are small-sample measurements, not an SLA. Warm cases cover a six-file feature diff, a parser fix, documentation, tests, and Chinese project rules. All create actual commits in disposable repositories with hooks/signing disabled. Warmup is outside the warm-case timer; it is included in the cold case. Provider-side prompt caching is uncontrolled. Public API-key requests and other machines require separate measurement. [Raw measurements](docs/benchmarks/2026-10-05.json).

## Development and releases

```sh
make build
make test
make check
make package VERSION=0.2.0
```

Tests use disposable repositories and local fake HTTP servers, not real model credentials. CI runs on macOS, Linux and Windows. The race detector runs on macOS/Linux. Tagging `v*` builds six archives and publishes a GitHub Release with SHA-256 checksums. The only non-standard-library dependencies are the official Go terminal/system packages for hidden password input and Windows process locking. Installer tests run offline with real native binaries; no account credentials are required.

MIT licensed.
