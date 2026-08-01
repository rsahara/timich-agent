#!/usr/bin/env sh
set -eu

key_bundle=${1:-}
signature=${2:-}
artifact=${3:-}
expected_fingerprint=$(printf '%s' "${4:-}" | tr '[:lower:]' '[:upper:]')
gpg_bin=${GPG_BIN:-gpg}

fail() {
  echo "FFmpeg source signature verification failed: $*" >&2
  exit 2
}

for path in "$key_bundle" "$signature" "$artifact"; do
  [ -f "$path" ] || fail "required input is missing: $path"
done
printf '%s\n' "$expected_fingerprint" | grep -Eq '^[0-9A-F]{40}$' || fail "expected fingerprint must be 40 hexadecimal characters"
command -v "$gpg_bin" >/dev/null 2>&1 || fail "gpg is required"

gpg_home=$(mktemp -d)
trap 'rm -rf "$gpg_home"' EXIT INT TERM
chmod 700 "$gpg_home"

"$gpg_bin" --batch --homedir "$gpg_home" --import "$key_bundle" >/dev/null 2>&1 || fail "could not import the pinned key bundle"
if ! verify_status=$("$gpg_bin" --batch --homedir "$gpg_home" --status-fd=1 --verify "$signature" "$artifact" 2>&1); then
  printf '%s\n' "$verify_status" >&2
  fail "signature is invalid"
fi
valid_fingerprint=$(printf '%s\n' "$verify_status" | awk '$1 == "[GNUPG:]" && $2 == "VALIDSIG" { print toupper($3); exit }')
[ -n "$valid_fingerprint" ] || fail "gpg did not report a VALIDSIG signer"
[ "$valid_fingerprint" = "$expected_fingerprint" ] || fail "signature was made by $valid_fingerprint instead of $expected_fingerprint"
