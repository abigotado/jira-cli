# jira-cli

Agent-first Jira Cloud CLI for Codex and Claude Code.

`jira-cli` exposes a small, versioned machine contract instead of making an
agent assemble Jira REST requests. It keeps API tokens in macOS Keychain,
requires an explicit profile for every network call, and ships one Agent Skill
that installs into both Codex and Claude Code.

> This repository is under active development. The first increment is
> intentionally read-only; Jira mutations are not part of the current command
> surface.

## Design goals

- One standalone binary is the only component that talks to Jira REST or
  macOS Keychain.
- JSON mode (the non-TTY default) uses a stable envelope and exit codes;
  explicit text, raw, and help output are exceptions.
- Multiple accounts are selected per invocation with `--profile`; there is no
  mutable account switch or stored default.
- Credentials never appear in configuration, URLs, logs, errors, skill files,
  or repository content.
- Codex and Claude Code use the same canonical `SKILL.md` and command contract.

## Install

Go 1.24.1 and the macOS SDK:

```bash
go install github.com/abigotado/jira-cli/cmd/jira-cli@latest
```

Versioned source releases are listed under
[Releases](https://github.com/abigotado/jira-cli/releases). Prebuilt binaries
and a Homebrew cask are intentionally deferred until the macOS executable can
be Developer ID signed and notarized. Linux and Windows distributions are also
deferred until those platforms have a supported credential backend.

## Build from a checkout

Go 1.24.1 and the macOS SDK are required for the native Keychain backend.

```bash
go build -o ./bin/jira-cli ./cmd/jira-cli
./bin/jira-cli version
./bin/jira-cli contract
```

The Keychain implementation calls Apple's Security.framework directly. It
does not invoke the `security` command or a password-manager subprocess.
Non-macOS and `CGO_ENABLED=0` builds fail with a typed unsupported-credential
error when a stored credential is requested.

## Profiles

A profile contains non-secret connection metadata. Its token is a separate
generic-password entry in Keychain.

```bash
jira-cli auth login \
  --profile work \
  --site https://example.atlassian.net \
  --email developer@example.invalid \
  --token-kind classic \
  --token-stdin
```

When stdin is a terminal, token input is hidden. Piped input is bounded and
must contain exactly one token line. Existing profiles require `--yes` to be
replaced.

```bash
jira-cli auth list
jira-cli auth status --profile work
jira-cli auth logout --profile work --yes
```

For scoped tokens, pass `--token-kind scoped`; `jira-cli` discovers and stores
the site's non-secret Cloud ID before reading the token. Classic credentials
are sent only to the validated tenant host. Scoped credentials are sent only
to `api.atlassian.com`.

## Read commands

```bash
jira-cli me --profile work
jira-cli projects list --profile work --limit 50
jira-cli projects get WL --profile work

jira-cli issues get WL-123 --profile work \
  --fields key,summary,status,assignee,updated
jira-cli issues search --profile work \
  --jql 'project in (WL, FL) ORDER BY updated DESC' \
  --fields key,summary,status,assignee,updated \
  --limit 50

jira-cli issues transitions WL-123 --profile work
jira-cli comments list --issue WL-123 --profile work --limit 50
```

Collection responses report whether another page exists and provide an opaque
`next_cursor`. Projects and comments use offset pagination internally; enhanced
JQL search uses Jira's `nextPageToken`. Callers do not need to mix the two.

## Agent Skill

Install the same versioned workflow for one or both local agents:

```bash
jira-cli skills install --provider codex --scope user
jira-cli skills install --provider claude --scope user
jira-cli skills install --provider all --scope user
```

User locations:

- Codex: `$HOME/.agents/skills/jira`
- Claude Code: `$HOME/.claude/skills/jira`

Project-scoped Codex installs are canonical under `.agents/skills/jira`.
Claude project installs use `.claude/skills/jira` only when the repository has
no compiled `.agents` harness. Installation uses an ownership manifest,
preflight checks, path-containment checks, and hash-safe uninstall behavior.

Invoke the skill as `$jira` in Codex or `/jira` in Claude Code. The skill never
reads Keychain itself; it shells out to `jira-cli` and branches on the machine
contract.

## Machine contract

In JSON mode stdout contains only the result envelope; JSON is the default when
stdout is not a TTY. Text, raw, and explicit help output are selected
exceptions. Logs and human diagnostics go to stderr. Run `jira-cli contract`
for the complete versioned table.

```json
{
  "ok": true,
  "v": 1,
  "data": {"key": "WL-123", "summary": "Example"},
  "meta": {"profile": "work", "site": "https://example.atlassian.net"}
}
```

Exit codes communicate the caller's next action: internal `1`, usage `2`, not
found `3`, ambiguous `4`, authentication `5`, retryable `6`, confirmation `7`,
permission/scope `8`, and conflict/stale state `9`.

## Security

- Never paste a token, Authorization header, Keychain output, or environment
  dump into an issue or agent prompt.
- `auth list` reads only the non-secret profile registry.
- HTTP redirects are refused before credentials can leave the validated
  Atlassian origin.
- Upstream response bodies are bounded and never copied verbatim into errors.
- Keychain queries disable authentication UI so an agent gets a typed failure
  instead of hanging behind an invisible prompt.

See [SECURITY.md](SECURITY.md) for private vulnerability reporting.

## Development

```bash
gofmt -l .
go vet ./...
go build ./...
go test -race ./...
python3 .agents/scripts/sync-rules.py --check
python3 -m unittest discover -s .agents/tests -p 'test_*.py'
```

The opt-in cross-artifact Keychain spike is separate from ordinary tests. It
creates, reads, and removes only its own unique non-secret sentinel and records
whether a separately built binary can reuse the entry without UI.

## License

MIT. See [LICENSE](LICENSE).
