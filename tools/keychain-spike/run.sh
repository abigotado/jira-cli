#!/bin/sh
set -eu

spike_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
spike_tmp=$(mktemp -d "${TMPDIR:-/tmp}/jira-keychain-spike.XXXXXX")
spike_id=$(uuidgen | tr '[:upper:]' '[:lower:]')
spike_account="keychain-spike-${spike_id}"
spike_sentinel="non-secret-sentinel-${spike_id}"
spike_created=0

run_bounded() {
	spike_seconds=$1
	shift
	perl -e 'alarm shift; exec @ARGV or exit 127' "$spike_seconds" "$@"
}

cleanup() {
	if [ "$spike_created" -eq 1 ] && [ -x "$spike_tmp/writer" ]; then
		run_bounded 5 "$spike_tmp/writer" --mode delete --account "$spike_account" >/dev/null 2>&1 || true
	fi
	rm -rf -- "$spike_tmp"
}
trap cleanup EXIT HUP INT TERM

cd "$spike_root"
run_bounded 30 go build -trimpath -ldflags '-X main.artifactID=writer' -o "$spike_tmp/writer" ./tools/keychain-spike
run_bounded 30 go build -trimpath -ldflags '-X main.artifactID=reader' -o "$spike_tmp/reader" ./tools/keychain-spike

writer_hash=$(shasum -a 256 "$spike_tmp/writer" | awk '{print $1}')
reader_hash=$(shasum -a 256 "$spike_tmp/reader" | awk '{print $1}')
if [ "$writer_hash" = "$reader_hash" ]; then
  exit 1
fi

run_bounded 5 "$spike_tmp/writer" --mode write --account "$spike_account" --sentinel "$spike_sentinel"
spike_created=1
run_bounded 5 "$spike_tmp/reader" --mode read --account "$spike_account" --sentinel "$spike_sentinel"
run_bounded 5 "$spike_tmp/writer" --mode delete --account "$spike_account"
spike_created=0
