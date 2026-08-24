# Credentials and request security

`jira-cli` holds a Jira Cloud API token. Treat it as a live credential with all
permissions granted to its Atlassian account.

## Profiles and storage

- Every network invocation requires `--profile NAME`. There is no active,
  switched, or stored default profile.
- The profile registry contains non-secret metadata only: name, Jira site,
  email, token kind, Cloud ID, and optional expiry.
- The token is stored as one generic-password item per profile in macOS
  Keychain. It never enters the registry, a cache, a log, or the repository.
- Keychain access uses Security.framework `SecItem` APIs directly. Never use
  `security`, `go-keyring`, `os/exec`, or another subprocess credential path.
- Keychain queries must set `kSecUseAuthenticationUIFail`. An agent must receive
  a typed failure instead of hanging behind an invisible GUI prompt.
- `auth list` reads the registry only. It never probes every Keychain item.

## Input and transactions

- Accept a token through bounded stdin only. Reject empty, multiline, NUL, and
  oversized input. Never accept it as a command-line flag.
- Validate the site and discover Cloud ID before reading the token.
- Verify credentials with `/myself` before persistence.
- Replacing an existing profile requires `--yes`.
- Save the credential, then atomically update the locked registry; compensate
  with credential deletion if the registry write fails.
- Logout deletes the exact credential first, then its registry entry.
- Registry corruption and permission errors fail closed.

## HTTP boundary

- Classic profiles allow only `https://<tenant>.atlassian.net` with no
  userinfo, port, path, query, fragment, IP address, or localhost.
- Scoped profiles always send credentials to the fixed
  `https://api.atlassian.com/ex/jira/<cloudId>` base.
- Redirects are disabled. Dynamic path segments are escaped. Response bodies
  are bounded and raw upstream bodies never appear in user-facing errors.
- Basic authorization is constructed in memory immediately before the request.
  Never print, log, inspect, or persist the header.
- Only explicitly marked read operations may retry. Enhanced JQL search is a
  read despite using POST; future write POSTs are never implicitly retryable.

## Review triggers

Changes to `internal/auth`, `internal/profile`, request construction, logging,
or error formatting require an explicit sentinel-redaction review. Search the
diff for `token`, `Authorization`, `security`, `os/exec`, request URLs, and
unbounded body reads. Tests may use obvious fake sentinels but must assert they
are absent from every output and error stream.
