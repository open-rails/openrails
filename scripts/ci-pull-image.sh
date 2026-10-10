#!/usr/bin/env bash
# Pulls a Docker Hub image (postgres:18-alpine, hashicorp/vault:2.1.1) for CI
# without Docker Hub, whose anonymous pull limit runners share: Google's Docker
# Hub mirror first, ECR Public's copy as the fallback (an official image under
# docker/library, any other under its own namespace), both with ci-retry.sh's
# backoff. Both serve Docker Hub's digests.
#
# Prints the pulled reference on stdout; everything else goes to stderr, so:
#   image="$(scripts/ci-pull-image.sh postgres:18-alpine)"
#   docker run --pull never ... "$image"
set -euo pipefail

image="${1:?usage: ci-pull-image.sh <name:tag>}"
if [ "${2:-}" != --once ]; then
    exec "$(dirname "$0")/ci-retry.sh" "$0" "$image" --once
fi

case "$image" in
*/*) refs=("mirror.gcr.io/$image" "public.ecr.aws/$image") ;;
*) refs=("mirror.gcr.io/library/$image" "public.ecr.aws/docker/library/$image") ;;
esac
for ref in "${refs[@]}"; do
    if docker pull --quiet "$ref" >&2; then
        echo "$ref"
        exit 0
    fi
done
exit 1
