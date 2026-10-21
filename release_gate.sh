#!/bin/sh
#
# Release gate: the vX.Y.Z tag being released must name the image the
# deployment manifests pin.
#
# release.yml published whatever tag was pushed: `version="${GITHUB_REF_NAME}"`
# was the only source of the published reference, and no step compared it with
# the manifests (or with main_test.go's releaseImage constant). Pushing v0.2.0
# from a tree that still pinned v0.1.0 therefore passed vet and the full test
# suite - TestManifestsPinVersionedReleaseImage compares the manifests with
# releaseImage, not with the tag - and published
# ghcr.io/redis-master-label/redis-master-label:v0.2.0, an image no manifest in
# the repository references. Only the README's manual bump kept tag and pin
# aligned, and a tag pushed from a commit that never merged bypassed CI
# entirely. This script is the check that runs before anything is built.
#
# Usage: release_gate.sh <tag> <expected-image> [manifest-dir]
#   tag             the pushed release tag, for example v0.2.0
#   expected-image  the registry-qualified image the workflow publishes,
#                   for example ghcr.io/redis-master-label/redis-master-label
#   manifest-dir    directory holding the deployment manifests (default:
#                   manifests)
#
# Exit status: 0 when <expected-image>:<tag> is exactly what the manifests pin,
# 1 when the tag cannot be released (mismatch, no pin, unreadable pin), 2 when
# the arguments are unusable. POSIX sh only, so it runs on any release runner.
set -eu

script_name="${0##*/}"

die() {
  echo "$script_name: $*" >&2
  exit 1
}

usage_die() {
  echo "$script_name: usage: $script_name <tag> <expected-image> [manifest-dir]" >&2
  exit 2
}

[ "$#" -ge 2 ] || usage_die

tag="$1"
expected="$2"
manifest_dir="${3:-manifests}"

case "$expected" in
  "" | *:* | *@*)
    echo "$script_name: expected image '$expected' must be a registry-qualified name without a tag or digest; the tag being released is '$tag'" >&2
    exit 2
    ;;
esac

# The pin the tag has to match is a strict vX.Y.Z release tag: a floating or
# partial tag cannot be compared with a pinned reference.
if ! printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
  die "release tag '$tag' is not vMAJOR.MINOR.PATCH, so it cannot name a pinned release image"
fi

[ -d "$manifest_dir" ] || die "manifest directory '$manifest_dir' does not exist"

has_manifest=0
for file in "$manifest_dir"/*.yaml "$manifest_dir"/*.yml; do
  if [ -f "$file" ]; then
    has_manifest=1
  fi
done
[ "$has_manifest" -eq 1 ] || die "manifest directory '$manifest_dir' holds no YAML manifests"

# Every image reference the manifests declare. The match is anchored at the
# start of the line (after indentation) so a commented-out `# image:` line never
# counts: a manifest that comments the pin out pins nothing.
refs="$(grep -hE '^[[:space:]]*image:' "$manifest_dir"/*.yaml "$manifest_dir"/*.yml 2>/dev/null \
  | sed -E 's/^[[:space:]]*image:[[:space:]]*//; s/[[:space:]]+$//' || true)"

want="$expected:$tag"
matches=""
count=0
while IFS= read -r ref; do
  [ -n "$ref" ] || continue
  ref="${ref#\"}"
  ref="${ref%\"}"
  ref="${ref#\'}"
  ref="${ref%\'}"
  case "$ref" in
    "$expected" | "$expected":*)
      matches="$matches$ref
"
      count=$((count + 1))
      ;;
  esac
done <<EOF
$refs
EOF

if [ "$count" -eq 0 ]; then
  die "no manifest in '$manifest_dir' references $expected: publishing $want would release an image nothing deploys (pin $want in the manifests first)"
fi

mismatch=0
while IFS= read -r ref; do
  [ -n "$ref" ] || continue
  if [ "$ref" != "$want" ]; then
    echo "$script_name: tag $tag does not match the pinned release image $ref" >&2
    mismatch=1
  fi
done <<EOF
$matches
EOF

if [ "$mismatch" -ne 0 ]; then
  die "bump main_test.go's releaseImage and every manifest to $want in the commit the tag points at, then tag again"
fi

echo "$script_name: tag $tag matches the pinned release image $want ($count manifest reference(s))"
