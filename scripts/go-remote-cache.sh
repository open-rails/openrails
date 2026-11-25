#!/usr/bin/env bash
# Go's build cache shared through a bucket (Cloudflare R2, go-build-cache): GOCACHEPROG runs
# platacard/cacheprog, a local disk tier in front of the bucket. -trimpath makes the entries
# independent of the checkout's path, so CI runners and every worktree here share them.
#
#   scripts/go-remote-cache.sh run CMD [ARG...]   run CMD with it
#   eval "$(scripts/go-remote-cache.sh env)"      the same for this shell
#   scripts/go-remote-cache.sh ci                 .github/actions/go-remote-cache
#
# Locally the read-write key comes from Vault (VAULT_ADDR and your token) into a 0600 file
# under $XDG_RUNTIME_DIR; no secret enters the environment or the output. Without Vault, the
# key or the bucket, Go builds with its own cache. Nothing here changes the global Go env.
set -euo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
vault_path=openrails-prod/kv/infra/go-build-cache

# write_launcher DIR BIN_DIR ROOT WRITABLE: credentials JSON on stdin → DIR/gocacheprog.
write_launcher() {
  local dir=$1 bin_dir=$2 root=$3 writable=$4 bin
  bin="$("$repo/scripts/ci-install-tool.sh" cacheprog "$bin_dir")" || return 1
  mkdir -p "$dir" "$root"
  (umask 077 && python3 -I -c '
import json, shlex, sys
try:
    c = json.load(sys.stdin)
    c = c.get("data", {}).get("data", c)  # a Vault KV v2 read, or the plain document
except (ValueError, AttributeError):
    sys.exit("go-remote-cache: unreadable credentials")
keys = ("endpoint", "bucket", "access_key_id", "secret_access_key")
if not all(c.get(k) for k in keys):
    sys.exit("go-remote-cache: the credentials lack " + ", ".join(k for k in keys if not c.get(k)))
root, writable, out = sys.argv[1], sys.argv[2] == "true", sys.argv[3]
settings = {
    "CACHEPROG_REMOTE_STORAGE_TYPE": "s3",
    "CACHEPROG_S3_ENDPOINT": c["endpoint"],
    "CACHEPROG_S3_REGION": "auto",
    "CACHEPROG_S3_FORCE_PATH_STYLE": "true",
    "CACHEPROG_S3_BUCKET": c["bucket"],
    "CACHEPROG_S3_PREFIX": "v1",  # a new prefix starts an empty cache
    "CACHEPROG_S3_ACCESS_KEY_ID": c["access_key_id"],
    "CACHEPROG_S3_ACCESS_KEY_SECRET": c["secret_access_key"],
    "CACHEPROG_ROOT_DIRECTORY": root,
    "CACHEPROG_REMOTE_GET_TIMEOUT": "15s",
    "CACHEPROG_REMOTE_PUT_TIMEOUT": "60s",
    "CACHEPROG_MAX_BACKGROUND_WAIT": "60s",
}
if not writable:
    # Read-only: no object is large enough to upload; the disk tier still keeps them.
    settings["CACHEPROG_MIN_REMOTE_PUT_SIZE"] = str(2**63 - 1)
with open(out, "w") as f:
    f.writelines(f"{k}={shlex.quote(v)}\n" for k, v in settings.items())
' "$root" "$writable" "$dir/settings") || return 1
  printf '#!/usr/bin/env bash\nset -a\n. %q\nCACHEPROG_LOG_LEVEL=${CACHEPROG_LOG_LEVEL:-WARN}\nexec %q direct\n' \
    "$dir/settings" "$bin" > "$dir/gocacheprog"
  chmod 700 "$dir/gocacheprog"
}

# trimpath: the effective GOFLAGS (the environment's or go env's) with -trimpath.
trimpath() {
  local flags
  flags="$(go env GOFLAGS)"
  case " $flags " in
    *" -trimpath "*) printf '%s' "$flags" ;;
    *) printf '%s' "${flags:+$flags }-trimpath" ;;
  esac
}

# local_setup: the read-write launcher, its key from Vault refreshed daily; prints its path.
local_setup() {
  local state=${XDG_RUNTIME_DIR:-$HOME/.cache}/go-remote-cache cache=$HOME/.cache/go-remote-cache
  mkdir -p "$state" "$cache" && chmod 700 "$state"
  if [[ ! -x $state/gocacheprog || -n $(find "$state/settings" -mmin +1440 2>/dev/null) ]]; then
    if ! vault kv get -format=json "$vault_path/read-write" 2>/dev/null |
      write_launcher "$state" "$cache/bin" "$cache/objects" true; then
      echo "go-remote-cache: no key from Vault ($vault_path/read-write)" >&2
      [[ -x $state/gocacheprog ]] || return 1
    fi
  fi
  # The disk tier forgets what no build read for 5 days, as Go's own cache does; once a day.
  if [[ -z $(find "$cache/pruned" -mmin -1440 2>/dev/null) ]]; then
    touch "$cache/pruned"
    nohup nice -n 19 find "$cache/objects" -name '*-meta' -atime +5 \
      -exec sh -c 'for m; do rm -f "$m" "${m%-meta}-data"; done' sh {} + >/dev/null 2>&1 &
  fi
  printf '%s\n' "$state/gocacheprog"
}

# ci: the action's step. Any failure leaves Go's own cache (actions/cache) in charge.
ci() {
  local dir=$RUNNER_TEMP/go-remote-cache creds=${GO_REMOTE_CACHE_CREDENTIALS:-} writable endpoint
  if [[ -z $creds ]]; then
    echo "Remote Go cache off: this run has no credentials."
    return
  fi
  # Masks the keys; fails on anything but a JSON object.
  if ! printf '%s' "$creds" | python3 -I -c '
import json, sys
try:
    c = json.load(sys.stdin)
except ValueError:
    sys.exit(1)
if not isinstance(c, dict):
    sys.exit(1)
for k in ("access_key_id", "secret_access_key"):
    if isinstance(c.get(k), str) and c[k]:
        print("::add-mask::" + c[k])'; then
    echo "::warning::Remote Go cache credentials are not a JSON object; building with Go's own cache."
    return
  fi
  read -r writable endpoint < <(printf '%s' "$creds" | python3 -I -c '
import json, sys
c = json.load(sys.stdin)
print(str(c.get("writable") is True).lower(), c.get("endpoint") or "-")')
  if ! curl -s -o /dev/null --max-time 10 "$endpoint"; then
    echo "::warning::Remote Go cache unreachable; building with Go's own cache."
    return
  fi
  if ! printf '%s' "$creds" | write_launcher "$dir" "$dir/bin" "$dir/objects" "$writable"; then
    echo "::warning::Remote Go cache setup failed; building with Go's own cache."
    return
  fi
  printf 'GOCACHEPROG=%s\nGOFLAGS=%s\n' "$dir/gocacheprog" "$(trimpath)" >> "$GITHUB_ENV"
  echo "Remote Go cache on, $([[ $writable == true ]] && echo read-write || echo read-only)."
}

case "${1:-}" in
  run)
    shift
    (($#)) || { echo "usage: $0 run CMD [ARG...]" >&2; exit 2; }
    if prog="$(local_setup)"; then
      GOFLAGS="$(trimpath)"
      export GOCACHEPROG="$prog" GOFLAGS
    fi
    exec "$@"
    ;;
  env)
    prog="$(local_setup)" || exit 0
    printf 'export GOCACHEPROG=%q GOFLAGS=%q\n' "$prog" "$(trimpath)"
    ;;
  ci) ci ;;
  *) echo "usage: $0 run CMD [ARG...] | env | ci" >&2; exit 2 ;;
esac
