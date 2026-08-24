---
name: write-tests
description: Add or update jira-cli Go tests for changed behavior. Use after implementing a change, or when coverage is missing for a command, the Jira client, profile or credential handling, or the exit-code and envelope contract.
---

# Write jira-cli Tests

1. Read `.agents/rules/testing.md` and the nearest existing test file, then
   follow its shape.
2. Delegate to `test-writer`.
3. Cover, at minimum: success and error paths, empty and single-element
   results, both pagination styles, 401/403/404/409, 429 with and without
   `Retry-After`, redirect refusal, oversized bodies, and non-JSON errors.
4. Mock at boundaries only — `httptest` for HTTP, an injected fake
   `CredentialStore` for Keychain behavior, and `t.TempDir()` for files.
   Never reach the real Jira API or an existing Keychain item.
5. Run `go test -race ./...` and report the result.

Do not weaken an assertion to make a failing test pass — report the failure. If
a test cannot fail because of a real bug, do not write it.
