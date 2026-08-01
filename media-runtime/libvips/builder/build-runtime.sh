#!/usr/bin/env sh
set -eu

platform=${TIMICH_LIBVIPS_PLATFORM:-linux_amd64}
output_dir=${TIMICH_LIBVIPS_OUTPUT:-/out}
apk_packages=${TIMICH_LIBVIPS_APK_PACKAGES:-"ca-certificates vips-tools vips-heif pax-utils"}

apk add --no-cache $apk_packages

vips_path=$(command -v vips)
vips_version=$("$vips_path" --version 2>/dev/null | head -n 1 || true)
if [ -z "$vips_version" ]; then
  echo "unable to read libvips version" >&2
  exit 1
fi

rm -rf "${output_dir:?}/"*
mkdir -p \
  "${output_dir}/bin" \
  "${output_dir}/lib" \
  "${output_dir}/share" \
  "${output_dir}/LICENSES" \
  "${output_dir}/THIRD_PARTY_NOTICES"

cp "$vips_path" "${output_dir}/bin/vips.real"
chmod +x "${output_dir}/bin/vips.real"

if [ -f /lib/ld-musl-x86_64.so.1 ]; then
  cp /lib/ld-musl-x86_64.so.1 "${output_dir}/lib/ld-musl-x86_64.so.1"
else
  echo "missing Alpine musl loader /lib/ld-musl-x86_64.so.1" >&2
  exit 1
fi

copy_library() {
  path=$1
  if [ -f "$path" ]; then
    cp -L "$path" "${output_dir}/lib/$(basename "$path")"
  fi
}

collect_libraries() {
  target=$1
  ldd "$target" 2>/dev/null | awk '
    {
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^\//) {
          print $i
        }
      }
    }
  '
}

library_list=$(mktemp)
trap 'rm -f "$library_list"' EXIT INT TERM

collect_libraries "$vips_path" >> "$library_list"
if ls /usr/lib/vips-modules-*/*.so >/dev/null 2>&1; then
  find /usr/lib/vips-modules-* -type f -name '*.so' -print | while IFS= read -r module; do
    collect_libraries "$module" >> "$library_list"
  done
fi

sort -u "$library_list" | while IFS= read -r library; do
  copy_library "$library"
done

if ls /usr/lib/vips-modules-* >/dev/null 2>&1; then
  cp -a /usr/lib/vips-modules-* "${output_dir}/lib/"
fi

for share_path in /usr/share/vips /usr/share/locale; do
  if [ -d "$share_path" ]; then
    cp -a "$share_path" "${output_dir}/share/"
  fi
done

cat > "${output_dir}/bin/vips" <<'EOF'
#!/usr/bin/env sh
set -eu

self=$0
case "$self" in
  /*) ;;
  *) self=$(command -v -- "$self") ;;
esac

bin_dir=$(CDPATH= cd -- "$(dirname -- "$self")" && pwd)
runtime_root=$(CDPATH= cd -- "$bin_dir/.." && pwd)
lib_dir="$runtime_root/lib"
share_dir="$runtime_root/share"
loader="$lib_dir/ld-musl-x86_64.so.1"
vips_real="$bin_dir/vips.real"

module_path=""
for module_dir in "$lib_dir"/vips-modules-*; do
  if [ -d "$module_dir" ]; then
    if [ -z "$module_path" ]; then
      module_path="$module_dir"
    else
      module_path="$module_path:$module_dir"
    fi
  fi
done

export LD_LIBRARY_PATH="$lib_dir${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
if [ -n "$module_path" ]; then
  export VIPS_MODULE_PATH="$module_path${VIPS_MODULE_PATH:+:$VIPS_MODULE_PATH}"
fi
if [ -d "$share_dir" ]; then
  export XDG_DATA_DIRS="$share_dir${XDG_DATA_DIRS:+:$XDG_DATA_DIRS}"
fi

exec "$loader" --library-path "$lib_dir" "$vips_real" "$@"
EOF
chmod +x "${output_dir}/bin/vips"

apk info -vv > "${output_dir}/APK-PACKAGES.txt"
cp /lib/apk/db/installed "${output_dir}/APK-INSTALLED.txt"

cat > "${output_dir}/THIRD_PARTY_NOTICES/libvips.txt" <<'EOF'
Timich libvips runtime

This runtime is assembled from Alpine Linux packages for Timich local image
thumbnail decoding through the Rust media helper. It is intended for native
bundle validation and includes the musl loader plus linked shared libraries so
the wrapper script can run without installing Alpine libraries on the host.

Review APK-PACKAGES.txt and APK-INSTALLED.txt before release publication. The
current vips-heif package path can include HEVC-related codec libraries; that
license posture must be accepted or replaced by a stricter decode-focused build
before claiming final release readiness.
EOF

built_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '%s\n' \
  '{' \
  '  "schemaVersion": 1,' \
  "  \"platform\": \"${platform}\"," \
  "  \"vipsVersion\": \"${vips_version}\"," \
  "  \"apkPackages\": \"${apk_packages}\"," \
  "  \"builtAt\": \"${built_at}\"," \
  '  "profile": "alpine-vips-heif-runtime",' \
  '  "licenseReviewRequired": true' \
  '}' > "${output_dir}/BUILDINFO.json"

"${output_dir}/bin/vips" --version >/dev/null
classes_output=$(mktemp)
"${output_dir}/bin/vips" list classes > "$classes_output"
grep -Eq 'heifload|VipsForeignLoadHeif' "$classes_output"
rm -f "$classes_output"

if [ -n "${TIMICH_OUTPUT_UID:-}" ] && [ -n "${TIMICH_OUTPUT_GID:-}" ]; then
  chown -R "${TIMICH_OUTPUT_UID}:${TIMICH_OUTPUT_GID}" "$output_dir" || true
fi
