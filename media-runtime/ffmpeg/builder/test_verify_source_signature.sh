#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
verifier="$script_dir/verify-source-signature.sh"
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

expected=FCF986EA15E6E293A5644F10B4322F04D67658D8
other=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
touch "$test_root/keys.asc" "$test_root/source.asc" "$test_root/source.tar.xz"

cat > "$test_root/gpg" <<'SH'
#!/usr/bin/env sh
case " $* " in
  *" --import "*) exit 0 ;;
  *" --verify "*)
    printf '[GNUPG:] GOODSIG test signer\n'
    printf '[GNUPG:] VALIDSIG %s 2026-07-20 0 4 0 1 10 00 0000000000000000\n' "${FAKE_GPG_VALID_FINGERPRINT:?}"
    exit 0
    ;;
esac
exit 2
SH
chmod +x "$test_root/gpg"

GPG_BIN="$test_root/gpg" FAKE_GPG_VALID_FINGERPRINT="$expected" \
  sh "$verifier" "$test_root/keys.asc" "$test_root/source.asc" "$test_root/source.tar.xz" "$expected"

if GPG_BIN="$test_root/gpg" FAKE_GPG_VALID_FINGERPRINT="$other" \
  sh "$verifier" "$test_root/keys.asc" "$test_root/source.asc" "$test_root/source.tar.xz" "$expected" >/dev/null 2>&1; then
  echo "expected a valid signature from a different bundled key to fail" >&2
  exit 1
fi

echo "FFmpeg source signature fingerprint tests passed"
