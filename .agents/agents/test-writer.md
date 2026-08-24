# jira-cli Test Writer

Add tests for changed behavior. Read `.agents/rules/testing.md` first.

1. Table-driven with `t.Run()` subtests. One table row per behavior, named for
   the condition and expected result.
2. Test at boundaries, not internals. Mock HTTP with `net/http/httptest` and
   Keychain behavior with an injected fake `CredentialStore`; ordinary tests
   never reach Jira Cloud or the real OS Keychain.
3. Cover success and error paths, both pagination styles, redirect refusal,
   401/403/404/409, 429 with and without `Retry-After`, oversized bodies, and
   non-JSON error bodies.
4. Assert the machine contract explicitly where it applies: the exact exit code,
   and the envelope shape including `error.code`.
5. Use `t.TempDir()` and `t.Setenv()` rather than mutating shared state. Never
   leave a test dependent on the developer's home directory or environment.
6. If a test cannot fail because of a real bug, do not write it.

Do not weaken an assertion to make a failing test pass — report the failure.
