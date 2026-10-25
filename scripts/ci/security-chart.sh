#!/usr/bin/env bash
set -euo pipefail

chart=deploy/helm/openrails
data="$(pwd)/scripts/ci/trivy-publishers.json"
rendered="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/openrails-chart-security.XXXXXX")"
helm="${HELM:-$(scripts/ci-install-tool.sh helm)}"
version="$(awk '$1 == "require" && $2 == "github.com/open-rails/openrails" { print $3 }' server/go.mod)"
# Reviewed cross-module candidates pin an immutable Go pseudo-version before a
# stable release exists. Both forms are concrete image tags; floating names
# and arbitrary strings must still fail this release-context check.
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-0\.[0-9]{14}-[0-9a-f]{12})?$ ]]
if [[ "$version" == *-0.* ]]; then
  curl -fsS --retry 3 --max-time 60 \
    "https://proxy.golang.org/github.com/open-rails/openrails/@v/$version.info" > "$rendered/source.json"
  python3 - "$version" "$rendered/source.json" <<'PY'
import datetime, json, pathlib, re, sys

resolved = json.loads(pathlib.Path(sys.argv[2]).read_text())
candidate = re.fullmatch(r"v\d+\.\d+\.\d+-0\.(\d{14})-([0-9a-f]{12})", sys.argv[1])
origin = resolved.get("Origin", {})
at = datetime.datetime.fromisoformat(resolved["Time"].replace("Z", "+00:00"))
if (not candidate or resolved.get("Version") != sys.argv[1]
        or at.astimezone(datetime.timezone.utc).strftime("%Y%m%d%H%M%S") != candidate[1]
        or origin.get("VCS") != "git"
        or origin.get("URL") != "https://github.com/open-rails/openrails"
        or not re.fullmatch(candidate[2] + r"[0-9a-f]{28}", origin.get("Hash", ""))):
    raise SystemExit("chart source candidate did not resolve to its exact public module version and commit")
print("Auditing source candidate chart context; this does not verify a published image or release.")
PY
fi

# A source checkout intentionally uses :latest and has no install namespace.
# Audit the same versioned chart context that release packaging publishes.
"$helm" template openrails "$chart" --namespace openrails --kube-version 1.30.0 \
  --set-string "image.tag=$version" > "$rendered/release.yaml"

# Trivy's registry policy only accepts domains. Narrow those trusted domains
# to our exact publisher repositories before using that policy.
python3 - "$data" "$rendered/release.yaml" <<'PY'
import json, pathlib, re, shlex, sys

repositories = json.loads(pathlib.Path(sys.argv[1]).read_text())["openrails_image_repositories"]
images = re.findall(r'^[ \t]+image:[ \t]*(.*)$', pathlib.Path(sys.argv[2]).read_text(), re.MULTILINE)
if not images:
    raise SystemExit("release chart contains no container images")
for raw_image in images:
    tokens = shlex.split(raw_image, comments=True)
    if len(tokens) != 1:
        raise SystemExit(f"release chart has an invalid image: {raw_image}")
    image = tokens[0]
    if not any(image.startswith(repository + ":") or image.startswith(repository + "@sha256:") for repository in repositories):
        raise SystemExit(f"release chart uses an untrusted publisher: {image}")
PY

# The main filesystem pass omits this exact directory, so retain vulnerability
# and plaintext-secret coverage here; rendered resources retain all config checks.
trivy fs --scanners vuln,secret --ignore-unfixed --exit-code 1 "$chart"
trivy config --config-data "$(dirname "$data")" --exit-code 1 "$rendered"
