# Keychain cross-artifact spike

This opt-in macOS-only spike builds two different binary hashes and verifies
that the second artifact can read a single item written by the first. It never
enumerates Keychain items and it never prints the sentinel value.

The script creates a unique, explicitly non-secret sentinel under service
`jira-cli` and account `keychain-spike-<random UUID>`. Each helper invocation is
bounded to five seconds and an exit trap attempts deletion before removing its
temporary binaries.

Run only from an interactive terminal when explicitly testing macOS Keychain
artifact identity behavior:

```sh
./tools/keychain-spike/run.sh
```

Exit status zero means the cross-artifact read and cleanup completed. The tool
prints no credential or sentinel material. A nonzero status means the check did
not complete; the unique account name avoids colliding with real profiles.
