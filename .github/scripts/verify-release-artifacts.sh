#!/usr/bin/env bash
# Verify that every artifact of a core-provider release can be pulled by its tag (#360).
#
# A GitHub release is a public claim that a version EXISTS. It used to be published at tag time,
# about 30 minutes before its images did. This check runs after the image build and the chart
# publish, and the release is only published once it passes.
#
# Usage: verify-release-artifacts.sh <tag> [<expected-commit-sha>]
#   With a commit, each image's org.opencontainers.image.revision must equal it, so an image left
#   over from an earlier build under the same tag is not mistaken for this one.
#
# Retries cover registry propagation only. Every artifact is reported, and the exit is non-zero
# if any is missing after the last attempt.
set -euo pipefail

tag="${1:?usage: verify-release-artifacts.sh <tag> [<commit>]}"
want_rev="${2:-}"
owner="${REGISTRY_OWNER:-krateo-platformops}"
images=(core-provider composition-dynamic-controller chart-inspector)
charts=(core-provider core-provider-crds core-provider-target)
attempts="${ATTEMPTS:-6}"
delay="${DELAY_SECONDS:-30}"

check_image() {
  local img="ghcr.io/${owner}/$1:${tag}" rev
  if ! docker buildx imagetools inspect "$img" >/dev/null 2>&1; then
    echo "MISSING image $img"
    return 1
  fi
  if [ -n "$want_rev" ]; then
    rev=$(docker buildx imagetools inspect "$img" --format '{{json .Image}}' |
      jq -r 'if has("linux/amd64") then .["linux/amd64"] else . end | .config.Labels["org.opencontainers.image.revision"] // ""')
    if [ "$rev" != "$want_rev" ]; then
      echo "WRONG   image $img revision=${rev:-<none>} want=$want_rev"
      return 1
    fi
  fi
  echo "ok      image $img${want_rev:+ revision=$want_rev}"
}

check_chart() {
  local ref="oci://ghcr.io/${owner}/charts/$1" got
  got=$(helm show chart "$ref" --version "$tag" 2>/dev/null | awk '/^version:/{print $2}') || true
  if [ "$got" != "$tag" ]; then
    echo "MISSING chart $ref --version $tag"
    return 1
  fi
  echo "ok      chart $ref $tag"
}

for ((i = 1; i <= attempts; i++)); do
  failed=0
  for m in "${images[@]}"; do check_image "$m" || failed=$((failed + 1)); done
  for c in "${charts[@]}"; do check_chart "$c" || failed=$((failed + 1)); done
  total=$(( ${#images[@]} + ${#charts[@]} ))
  echo "attempt $i/$attempts: $((total - failed))/$total artifacts verified for $tag"
  [ "$failed" -eq 0 ] && exit 0
  [ "$i" -lt "$attempts" ] && sleep "$delay"
done
echo "::error::$failed artifact(s) of $tag are not pullable; the release is NOT published"
exit 1
