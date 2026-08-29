#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Repack a gVisor release from upstream bzip2 to zstd and stage it into the
# GCS snapshot bucket, where atelet fetches it (the gvisor-assets/ sibling of
# hack/microvm-assets/, which stages the micro-VM set the same way).
#
# Why: atelet extracts the "gvisor" asset with Go's stdlib bzip2, which costs
# ~20s of single-threaded CPU per node on the first actor operation that lands
# there (see cmd/atelet/sandbox_assets.go extractTarArchive). The same tarball
# recompressed as zstd extracts in well under a second. Upstream publishes
# bzip2 only, so the recompression happens here, offline: download the release,
# verify it against gVisor's published sha512, transcode the compression layer
# (the tar stream inside is byte-identical), and upload next to a .sha256
# sidecar. An already-staged release (sidecar present) is reused, not
# re-uploaded, so re-running is cheap; FORCE=true re-stages.
#
# Two modes:
#
#   repack-and-stage.sh
#       Stage the release named by RELEASE for each of ARCHES and print the
#       assets block to paste into manifests/ate-install/sandboxconfig-gvisor.yaml.
#
#   repack-and-stage.sh --from-manifest FILE [--out FILE]
#       Read a SandboxConfig manifest, stage every asset that points at an
#       upstream gs://gvisor .tar.bz2 (verifying the manifest's pinned sha256
#       on top of the upstream sha512), and write the manifest with those
#       assets rewritten to the staged zstd objects (stdout unless --out).
#       The rewrite is exact string substitution of each URL and its pinned
#       sha256, not a YAML round-trip, so comments and formatting survive and
#       no YAML tooling is needed; the pinned sha256 must sit within the two
#       lines after its url. Assets that don't match the upstream pattern pass
#       through unchanged. hack/install-ate.sh uses this to serve the fast
#       variant automatically.
#
# Requires: curl, bzip2, zstd, and gcloud authenticated for the bucket's
# project.
#
# Env: RELEASE (path under gs://gvisor/releases, default nightly/2026-08-28;
#              ignored with --from-manifest, which derives it per asset),
#      ARCHES  (default "amd64 arm64"; ignored with --from-manifest),
#      BUCKET  (default ate-snapshots, same as hack/microvm-assets/stage-to-gcs.sh),
#      PROJECT_ID (optional; passed to gcloud as --project when set),
#      ZSTD_LEVEL (default 12; decompression speed does not depend on it),
#      FORCE   (true to re-stage over an existing object, default false),
#      OUT     (work dir, default ./bin/gvisor-assets under the gitignored bin/).

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"

RELEASE="${RELEASE:-nightly/2026-08-28}"
ARCHES="${ARCHES:-amd64 arm64}"
BUCKET="${BUCKET:-ate-snapshots}"
ZSTD_LEVEL="${ZSTD_LEVEL:-12}"
FORCE="${FORCE:-false}"
OUT="${OUT:-${ROOT}/bin/gvisor-assets}"

UPSTREAM="https://storage.googleapis.com/gvisor/releases"

# sha256/sha512 of a file, first field only. coreutils sha256sum on Linux,
# shasum on stock macOS; the repack is byte manipulation with no arch
# constraint (unlike microvm-assets/assemble.sh), so both hosts are fine.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}
sha512_of() {
  if command -v sha512sum >/dev/null 2>&1; then
    sha512sum "$1" | awk '{print $1}'
  else
    shasum -a 512 "$1" | awk '{print $1}'
  fi
}

# GOARCH -> the arch directory name in the gVisor release bucket.
garch_of() {
  case "$1" in
    amd64) echo "x86_64" ;;
    arm64) echo "aarch64" ;;
    *) echo "unsupported arch $1 (want amd64 or arm64)" >&2; return 1 ;;
  esac
}

# stage_one RELEASE GARCH [EXPECTED_SHA256]
# Ensure gs://$BUCKET/gvisor-assets/RELEASE/GARCH/gvisor.tar.zst exists (with
# its .sha256 sidecar), staging it from upstream if absent, and print
# "URL SHA256" on stdout. All progress goes to stderr so callers can capture
# stdout. EXPECTED_SHA256, when given, is the caller's pin for the upstream
# .tar.bz2 (e.g. from the SandboxConfig manifest) and is verified in addition
# to the sha512 gVisor publishes.
stage_one() {
  local release="$1" garch="$2" expected_sha256="${3:-}"
  local src_url="${UPSTREAM}/${release}/${garch}/gvisor.tar.bz2"
  local dest="gs://${BUCKET}/gvisor-assets/${release}/${garch}/gvisor.tar.zst"
  local bz2="${OUT}/gvisor-${garch}.tar.bz2"
  local zst="${OUT}/gvisor-${garch}.tar.zst"

  # Pass --project only when PROJECT_ID is set (mirrors
  # hack/microvm-assets/stage-to-gcs.sh); otherwise gcloud uses its active
  # config project.
  if [ "${FORCE}" != "true" ]; then
    local staged_sha
    staged_sha="$(gcloud storage cat ${PROJECT_ID:+--project="${PROJECT_ID}"} "${dest}.sha256" 2>/dev/null || true)"
    if printf '%s' "${staged_sha}" | grep -Eq '^[a-f0-9]{64}$'; then
      echo ">> [${garch}] Already staged at ${dest}; reusing (FORCE=true to re-stage)" >&2
      echo "${dest} ${staged_sha}"
      return
    fi
  fi

  echo ">> [${garch}] Downloading ${src_url} ..." >&2
  curl -fSL -o "${bz2}" "${src_url}"

  # Verify against the sha512 gVisor publishes next to the tarball, so the
  # repacked artifact's chain of custody starts at the upstream release, not
  # at whoever ran this script.
  local upstream_sha512 local_sha512
  upstream_sha512="$(curl -fsSL "${src_url}.sha512" | awk '{print $1}')"
  local_sha512="$(sha512_of "${bz2}")"
  if [ "${upstream_sha512}" != "${local_sha512}" ]; then
    echo "sha512 mismatch for ${src_url}: upstream ${upstream_sha512}, downloaded ${local_sha512}" >&2
    return 1
  fi
  if [ -n "${expected_sha256}" ]; then
    local local_sha256
    local_sha256="$(sha256_of "${bz2}")"
    if [ "${expected_sha256}" != "${local_sha256}" ]; then
      echo "sha256 mismatch for ${src_url}: manifest pins ${expected_sha256}, downloaded ${local_sha256}" >&2
      return 1
    fi
  fi
  echo ">> [${garch}] Verified against ${src_url}.sha512" >&2

  # Transcode the compression layer only. The tar stream inside is untouched,
  # so nothing depends on this host's tar or umask.
  echo ">> [${garch}] Recompressing bzip2 -> zstd (level ${ZSTD_LEVEL}) ..." >&2
  bzip2 -dc "${bz2}" | zstd -q -f -T0 "-${ZSTD_LEVEL}" -o "${zst}"
  rm -f "${bz2}"

  local sha256
  sha256="$(sha256_of "${zst}")"

  echo ">> [${garch}] Uploading to ${dest} ..." >&2
  gcloud storage cp ${PROJECT_ID:+--project="${PROJECT_ID}"} "${zst}" "${dest}" >&2
  # The sidecar is what marks the object staged: it is written last, so a
  # partial upload is retried rather than reused.
  printf '%s' "${sha256}" | gcloud storage cp ${PROJECT_ID:+--project="${PROJECT_ID}"} - "${dest}.sha256" >&2
  rm -f "${zst}"

  echo "${dest} ${sha256}"
}

stage_from_manifest() {
  local manifest="$1" out="$2"
  # sed programs accumulate one url and one sha256 substitution per staged
  # asset; applied in one pass at the end.
  local sed_args=()

  local url line new_url new_sha expected
  while read -r url; do
    if [[ ! "${url}" =~ ^gs://gvisor/releases/(.+)/(x86_64|aarch64)/gvisor\.tar\.bz2$ ]]; then
      echo ">> ${url} is not an upstream gs://gvisor .tar.bz2 layout; leaving as-is" >&2
      continue
    fi
    local release="${BASH_REMATCH[1]}" garch="${BASH_REMATCH[2]}"

    # The manifest's own pin for this tarball: the first 64-hex string within
    # two lines after the url (the layout of sandboxconfig-gvisor.yaml, and
    # what the sha256-after-url convention of AssetFile produces). Required —
    # it is both the verification input and the string the rewrite replaces.
    expected="$(grep -A2 -F "${url}" "${manifest}" | grep -oE '\b[a-f0-9]{64}\b' | head -1 || true)"
    if [ -z "${expected}" ]; then
      echo "no sha256 found within two lines after ${url} in ${manifest}" >&2
      return 1
    fi

    line="$(stage_one "${release}" "${garch}" "${expected}")"
    new_url="${line% *}"
    new_sha="${line#* }"
    sed_args+=(-e "s|${url}|${new_url}|g" -e "s|${expected}|${new_sha}|g")
  done < <(grep -oE 'gs://gvisor/releases/[^"'\'' ]+/gvisor\.tar\.bz2' "${manifest}" | sort -u)

  if [ "${#sed_args[@]}" -eq 0 ]; then
    cat "${manifest}" > "${out}"
    return
  fi
  sed "${sed_args[@]}" "${manifest}" > "${out}"
}

mkdir -p "${OUT}"

if [ "${1:-}" = "--from-manifest" ]; then
  MANIFEST="${2:?--from-manifest needs a SandboxConfig manifest path}"
  OUT_FILE="/dev/stdout"
  if [ "${3:-}" = "--out" ]; then
    OUT_FILE="${4:?--out needs a path}"
  fi
  stage_from_manifest "${MANIFEST}" "${OUT_FILE}"
  exit 0
fi

# Manual mode: stage RELEASE for each of ARCHES and print the paste-ready
# assets block.
ASSETS_BLOCK=""
# ARCHES is a space-separated list, so the word-split is the point.
# shellcheck disable=SC2086
for arch in ${ARCHES}; do
  garch="$(garch_of "${arch}")"
  line="$(stage_one "${RELEASE}" "${garch}")"
  url="${line% *}"
  sha256="${line#* }"
  ASSETS_BLOCK="${ASSETS_BLOCK}    ${arch}:
      gvisor:
        url: \"${url}\"
        sha256: \"${sha256}\"
"
done

echo
echo ">> Done. Paste into manifests/ate-install/sandboxconfig-gvisor.yaml"
echo ">> (or your own SandboxConfig) under spec:"
echo
echo "  assets:"
printf '%s' "${ASSETS_BLOCK}"
