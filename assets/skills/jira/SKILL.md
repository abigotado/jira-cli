---
name: jira
description: Read and safely mutate Jira Cloud through the jira-cli machine contract. Use for Jira requests involving exact issue keys, project inventories, JQL searches, issue details, create/edit/transition actions, or comments.
---

# Use Jira safely with jira-cli

Use `jira-cli` as the only Jira access boundary. It emits one JSON envelope on
stdout and uses its process exit code to select the recovery action. Parse the
envelope, branch on the exit code, and follow `hint`. stderr is diagnostic and
must never be parsed as data.

Never bypass the CLI with direct REST requests. There is no supported delete,
admin, bulk, attachment, arbitrary-field, or raw-JSON write path.

Before a requested mutation, run `jira-cli version` and inspect the relevant
`jira-cli ... --help`. If the installed binary lacks the documented write
command, explain that it must be upgraded; do not emulate it with REST, curl,
another Jira CLI, or browser automation.

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

Treat an uppercase `PROJECT-<number>` key as an exact identifier. Read it
directly:

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

## Mutations

Every mutation requires all of these rails:

1. The user explicitly chooses the profile and intended action.
2. `auth allow-projects show --profile NAME` reports a bound policy containing
   the exact target project. Never broaden or replace the allowlist unless the
   user explicitly asks to change that local security policy.
3. Discover exact numeric issue-type or transition IDs with `issues types` or
   `issues transitions`; never guess an ID from a name. Subtask creation and
   parent assignment are unsupported; `issues types` returns standard types
   only.
4. Run the exact intended command with `--dry-run` first. Dry-run is local and
   its receipt must have `dry_run:true`, `applied:false`, and
   `remote_checks:"not_performed"`.
5. Show the bounded receipt to the user and obtain explicit confirmation for
   that exact mutation. Only then repeat it with `--yes` and without
   `--dry-run`.

Supported forms are:

```text
jira-cli issues types --project WL --profile work --limit 50
jira-cli issues create --project WL --issue-type-id 10001 --summary TEXT [--description TEXT] [--label LABEL]... --profile work --dry-run
jira-cli issues edit WL-123 [--summary TEXT] [--description TEXT | --clear-description] --profile work --dry-run
jira-cli issues transition WL-123 --transition-id 31 --profile work --dry-run
jira-cli comments add --issue WL-123 --body TEXT --profile work --dry-run
```

Keep summaries, descriptions, labels, and comments out of `--fields`:
mutation receipts intentionally never echo their contents. `--label` is an
exact repeatable flag: use it at most 100 times, preserve the intended order
and case, and do not supply whitespace, control characters, duplicates, or
values longer than 255 characters. Never add `--yes` merely to silence exit 7;
it represents the user's approval after reviewing dry-run.

An actual write performs remote identity preflights and uses bounded numeric
IDs, then verifies the issue project again after success. Issue creation also
fully checks Jira's create-screen field metadata before POST. On
`CREATE_FIELDS_UNSUPPORTED`, follow the hint: provide the supported description
or labels when requested, choose or configure a standard issue type whose
other required fields have Jira defaults, or create the issue in Jira. Never
bypass the bounded command with raw fields, direct REST, another CLI, or
browser automation.

The local allowlist is a target-selection rail; Jira account permissions are
the hard authorization boundary. A write is attempted once. On exit 9 with
`WRITE_OUTCOME_UNKNOWN`, including a concurrent project move, do not retry the
mutation: use read commands to reconcile whether it applied, then report the
uncertainty to the user. For any other nonzero exit, follow the envelope's
`hint` without broadening permissions or changing the target.

## Recovery

Exit 0 means the envelope is usable. For every other exit, read `error.code`
and `hint`; do not guess from prose or repeat the same command unchanged.
`LOCAL_LOCK_BUSY` means the contended operation did not run and credential
access has not begun; wait for the other jira-cli process to finish, then
retry.
Read [reference/contract.md](reference/contract.md) when an exit needs handling.
Read [reference/commands.md](reference/commands.md) when selecting flags or
pagination behavior.

Use `jira-cli contract` when the installed binary's contract version differs
from the reference. Use the installed command's `--help` when its binary
version is newer or older than this skill. Help is human-readable and is not
an envelope.
