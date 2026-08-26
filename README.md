# jira-cli

Agent-first Jira Cloud CLI for Codex and Claude Code.

`jira-cli` exposes a small, versioned machine contract instead of making an
agent assemble Jira REST requests. It keeps API tokens in macOS Keychain,
requires an explicit profile for every network call, and ships one Agent Skill
that installs into both Codex and Claude Code.

> This repository is under active development. Mutations are deliberately
> narrow: create/edit/transition/comment only, with a local project allowlist,
> dry-run, explicit confirmation, and no delete, admin, bulk, or raw-JSON path.

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

### Homebrew

On macOS, install the source-building Formula from the public tap:

```bash
brew install abigotado/tap/jira-agent-cli
jira-cli version
```

The Formula is named `jira-agent-cli` to avoid a collision with the unrelated
`jira-cli` Formula in Homebrew Core. It installs the `jira-cli` executable,
builds it locally with CGO enabled, and uses the native Security.framework
Keychain backend. It does not install an unsigned prebuilt executable or remove
macOS quarantine metadata.

### Go

Go 1.24.1 and the macOS SDK are required:

```bash
go install github.com/abigotado/jira-cli/cmd/jira-cli@latest
```

Versioned source releases are listed under
[Releases](https://github.com/abigotado/jira-cli/releases). Prebuilt binaries
and a Homebrew cask remain intentionally deferred until the macOS executable
can be Developer ID signed and notarized. Linux and Windows distributions are
also deferred until those platforms have a supported credential backend.

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

For the complete read-and-write command surface, a scoped Jira token needs the
existing classic read scopes `read:jira-work` and `read:jira-user`, plus this
granular least-privilege write union required by Jira's individual endpoints:

```text
read:issue:jira
read:comment:jira
read:comment.property:jira
read:group:jira
read:project:jira
read:project-role:jira
read:user:jira
read:avatar:jira
write:issue:jira
write:issue.property:jira
write:comment:jira
write:comment.property:jira
write:attachment:jira
```

Jira's [create-issue endpoint](https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issues/#api-rest-api-3-issue-post)
requires `write:attachment:jira` even though `jira-cli` has no attachment
command. The classic `read:jira-work` scope also covers the create-screen
metadata preflight. A fully granular read configuration needs
`read:issue-meta:jira`, `read:avatar:jira`, and
`read:field-configuration:jira` for that endpoint. Tokens cannot be modified
after creation; create a replacement token when adding scopes, verify it with
`auth login`, and revoke the old token only after the replacement works. Avoid
the broader classic `write:jira-work` scope when this granular union is
available.

### Write allowlist

Authentication alone never enables writes. Bind an explicit local project
allowlist to the profile's exact site, lowercase email, token kind, and Cloud
ID. Review the dry-run before applying it:

```bash
jira-cli auth allow-projects set --profile work \
  --project WL --project FL --dry-run
jira-cli auth allow-projects set --profile work \
  --project WL --project FL --yes
jira-cli auth allow-projects show --profile work
```

The non-secret policy is stored separately in an atomic locked 0600
`write-policies.json`. It survives logout and becomes usable again only when
the same identity logs back into that profile name; a different site or
account makes it fail closed as stale. Remove it before logout, or after the
same profile is restored, with
`auth allow-projects clear --profile work --yes`.

This allowlist is a local target-selection rail, not Jira's authorization
boundary. Jira project permissions on the token's account remain authoritative
and should be limited to the intended projects. The CLI checks the issue's
project immediately before and after a mutation; because Jira exposes no atomic
project precondition for these endpoints, a concurrent issue move is reported
as `WRITE_OUTCOME_UNKNOWN` and must be reconciled.

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

## Write commands

Discover exact numeric IDs with read commands, then preview locally. A dry-run
does not read Keychain or contact Jira:

```bash
jira-cli issues types --project WL --profile work --limit 50
jira-cli issues create --profile work --project WL \
  --issue-type-id 10001 --summary 'Bounded summary' \
  --description 'Plain-text description' --dry-run
jira-cli issues create --profile work --project WL \
  --issue-type-id 10001 --summary 'Bounded summary' \
  --description 'Plain-text description' --yes

jira-cli issues edit WL-123 --profile work \
  --summary 'Replacement summary' --dry-run
jira-cli issues edit WL-123 --profile work \
  --summary 'Replacement summary' --yes

jira-cli issues transitions WL-123 --profile work
jira-cli issues transition WL-123 --profile work \
  --transition-id 31 --dry-run
jira-cli issues transition WL-123 --profile work \
  --transition-id 31 --yes

jira-cli comments add --issue WL-123 --profile work \
  --body 'Bounded plain-text comment' --dry-run
jira-cli comments add --issue WL-123 --profile work \
  --body 'Bounded plain-text comment' --yes
```

`issues types` exposes only standard issue types. Subtask creation is not
supported because this bounded CLI intentionally has no parent-issue write
contract.

Before an actual mutation, `jira-cli` checks the identity-bound allowlist and
re-reads exact canonical project, issue, issue-type, and transition identities
as applicable. Issue creation also fully reads Jira's create-screen metadata
before the POST. If the selected project and type require a field that the
bounded CLI cannot supply and Jira has no default for it, creation fails closed
with `CREATE_FIELDS_UNSUPPORTED`; choose another standard type, change the Jira
screen configuration, or create the issue in Jira. Do not work around this with
raw fields or direct REST calls.

`--dry-run` remains deliberately local and reports
`remote_checks:"not_performed"`, so it cannot prove that current Jira screen
metadata accepts the payload. Actual writes use numeric Jira IDs, are attempted
once, and never echo summary, description, or comment bodies in receipts. Exit
9 with `WRITE_OUTCOME_UNKNOWN` means the request may have succeeded; re-read
Jira to reconcile it and do not repeat the write automatically.

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
- Every mutation requires an exact identity-bound local project allowlist,
  `--yes`, and bounded typed fields; `--dry-run` is entirely local.
- Mutations have no generic request, raw JSON, delete, admin, or bulk escape
  hatch and are never retried automatically.

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
