#!/usr/bin/env bash
# Pulls a Docker official image (e.g. postgres:18-alpine) for CI without Docker
# Hub, whose anonymous pull limit runners share: Google's Docker Hub mirror
# first, ECR Public's copy as the fallback, both with ci-retry.sh's backoff.
# Both serve Docker Hub's digests.
#
# Prints the pulled reference on stdout; everything else goes to stderr, so:
#   image="$(scripts/ci-pull-image.sh postgres:18-alpine)"
#   docker run --pull never ... "$image"
set -euo pipefail

image="${1:?usage: ci-pull-image.sh <name:tag>}"
if [ "${2:-}" != --once ]; then
    exec "$(dirname "$0")/ci-retry.sh" "$0" "$image" --once
fi

for registry in mirror.gcr.io/library public.ecr.aws/docker/library; do
    if docker pull --quiet "$registry/$image" >&2; then
        echo "$registry/$image"
        exit 0
    fi
done
exit 1
