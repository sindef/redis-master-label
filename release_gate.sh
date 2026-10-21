#!/bin/sh
# Release gate run by .github/workflows/release.yml before anything is built.
#
# The reviewed release_hygiene defect: the release workflow published whatever a
# vX.Y.Z tag named. `version="${GITHUB_REF_NAME}"` was the only source of the
# published reference and no step compared it with the image the manifests pin,
# so pushing v0.2.0 while releaseImage and the manifests still pinned v0.1.0
# passed `go vet` and the whole test suite (the pin still matched itself) and
# published ghcr.io/<repo>:v0.2.0, which no manifest in the repository
# references. The README's three-file bump was the only thing keeping tag and pin
# aligned, and a tag pushed from a commit that never merged bypassed CI
# entirely.
#
# Two gates close that hole:
#
#   release_gate.sh tag <release-tag> <image> [manifests-dir]
#       Every image reference in the manifests under manifests-dir that names
#       <image> must pin <release-tag>. Fails when no manifest references
#       <image> at all (the release would publish an image nothing deploys) and
#       when a pinned tag differs from the tag being released.
#
#   release_gate.sh main <commit> [main-ref]
#       <commit> must be reachable from main-ref, so a tag pushed from a commit
#       that was never merged cannot publish a release.
#
# Only POSIX sh and grep/sed/tr are used: the workflow runs it with bash, and the
# Go tests run it with sh against fixtures.

set -eu

usage() {
    echo "usage: release_gate.sh tag <release-tag> <image> [manifests-dir]" >&2
    echo "       release_gate.sh main <commit> [main-ref]" >&2
}

mode=${1:-}
case "$mode" in
tag)
    tag=${2:-}
    image=${3:-}
    dir=${4:-manifests}
    if [ -z "$tag" ] || [ -z "$image" ]; then
        usage
        exit 2
    fi
    if [ ! -d "$dir" ]; then
        echo "release gate: $dir is missing, so the pinned release image cannot be read from the repository" >&2
        exit 1
    fi

    # Every image reference the manifests pin that names the image being
    # published, one per line. `.` is escaped so the repository part matches
    # literally instead of as a regular expression.
    escaped=$(printf '%s\n' "$image" | sed 's/[][\\.*^$/]/\\&/g')
    refs=$(grep -rhoE '^[[:space:]]*image:[[:space:]]*[^[:space:]]+' "$dir" 2>/dev/null |
        sed -E 's/^[[:space:]]*image:[[:space:]]*//' |
        tr -d '\r"' |
        grep -E "^${escaped}:" || true)

    if [ -z "$refs" ]; then
        echo "release gate: no manifest under $dir pins $image, so releasing $tag would publish an image nothing deploys; pin ${image}:${tag} in the manifests in the same change as the tag" >&2
        exit 1
    fi

    status=0
    for ref in $refs; do
        pinned=${ref#"$image":}
        if [ "$pinned" != "$tag" ]; then
            echo "release gate: $dir pins $ref, but the tag being released is $tag; either bump the pin (manifests and main_test.go releaseImage) to $tag and tag that commit, or release $pinned" >&2
            status=1
        fi
    done
    if [ "$status" -ne 0 ]; then
        exit 1
    fi

    echo "release gate: every manifest reference to $image pins $tag"
    ;;
main)
    commit=${2:-}
    ref=${3:-origin/main}
    if [ -z "$commit" ]; then
        usage
        exit 2
    fi
    if ! git rev-parse --verify --quiet "${commit}^{commit}" >/dev/null; then
        echo "release gate: commit $commit is not in this checkout, so the released commit cannot be read" >&2
        exit 1
    fi
    if ! git rev-parse --verify --quiet "${ref}^{commit}" >/dev/null; then
        echo "release gate: $ref is not in this checkout, so reachability from main cannot be checked; fetch main first (actions/checkout with fetch-depth: 0, then git fetch origin main)" >&2
        exit 1
    fi
    if ! git merge-base --is-ancestor "$commit" "$ref"; then
        echo "release gate: commit $commit is not reachable from $ref, so the tag was pushed from a commit that never merged; merge it to main and tag the merged commit" >&2
        exit 1
    fi

    echo "release gate: commit $commit is reachable from $ref"
    ;;
*)
    usage
    exit 2
    ;;
esac
