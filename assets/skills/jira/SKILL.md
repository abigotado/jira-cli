---
name: jira
description: Read Jira Cloud projects, issues, transitions, and comments through the jira-cli machine contract. Use for Jira requests involving WL-* or FL-* issue keys, project inventories, JQL searches, issue details, or comments; this initial skill is read-only.
---

# Read Jira with jira-cli

Use `jira-cli` as the only Jira access boundary. It emits one JSON envelope on
stdout and uses its process exit code to select the recovery action. Parse the
envelope, branch on the exit code, and follow `hint`. stderr is diagnostic and
must never be parsed as data.

The initial command surface is read-only. Do not attempt to create, update,
transition, comment on, or delete Jira data, even when the user asks. Explain
that the installed version supports reads only. Do not bypass the CLI with
direct REST requests.

## Required profile

Pass an explicit `--profile NAME` on every command that contacts Jira. Never
infer an account from an email address, site URL, environment variable, or a
previous command. When the user has not named a profile, use `jira-cli auth
list` to list profile metadata, then ask them to choose. Even when only one
profile exists, do not select it on their behalf or set a default.

Never inspect the Keychain, run `security`, read token-bearing environment
variables, print request headers, or ask the user to paste a token into chat.
On authentication failure, ask the user to run the login command themselves.

## Exact issue reads

Treat an uppercase key matching `WL-<number>` or `FL-<number>` as an exact
identifier. Read it directly:

```text
jira-cli issues get WL-123 --profile work --fields key,summary,status,assignee,updated
```

Do not turn an exact key into JQL, fuzzy search, or a project listing first.
Use `issues search` only for a real collection query.

## Bounded output

Request the smallest useful field set. Add long fields such as `description`
only when the task needs them. Every collection call must have an explicit,
bounded `--limit`; normally start at 25 and never request more than 100 in one
call. Follow `meta.next_cursor` with `--cursor` only while more results are
needed. Stop when the requested answer is complete or no cursor is present.

For a collection query, prefer narrow JQL with a deterministic ordering:

```text
jira-cli issues search --profile work \
  --jql 'project in (WL, FL) ORDER BY updated DESC' \
  --fields key,summary,status,updated --limit 25
```

Quote JQL as one shell argument. Treat cursors as opaque and never edit them.

## Recovery

Exit 0 means the envelope is usable. For every other exit, read `error.code`
and `hint`; do not guess from prose or repeat the same command unchanged.
Read [reference/contract.md](reference/contract.md) when an exit needs handling.
Read [reference/commands.md](reference/commands.md) when selecting flags or
pagination behavior.

Use `jira-cli contract` when the installed binary's contract version differs
from the reference. `jira-cli --help` is human-readable and is not an envelope.
