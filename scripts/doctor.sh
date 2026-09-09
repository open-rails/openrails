#!/usr/bin/env bash
# Diagnose the local prerequisites used by the documented build/test tasks.
# Keep this file compatible with macOS Bash 3.2 so an old shell can report its
# own actionable failure instead of dying on unsupported syntax.
set -uo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
failures=0
docker_ok=false
compose_ok=false

pass() {
    printf 'PASS %-22s %s\n' "$1" "$2"
}

fail() {
    failures=$((failures + 1))
    printf 'FAIL %-22s %s; fix: %s\n' "$1" "$2" "$3"
}

version_ge() {
    awk -v have="$1" -v want="$2" 'BEGIN {
        split(have, h, "."); split(want, w, ".")
        for (i = 1; i <= 3; i++) {
            h[i] += 0; w[i] += 0
            if (h[i] > w[i]) exit 0
            if (h[i] < w[i]) exit 1
        }
        exit 0
    }'
}

env_file_value() {
    key="$1"
    [ -f "$root/.env" ] || return 0
    awk -v key="$key" '
        index($0, key "=") == 1 {
            value = substr($0, length(key) + 2)
            sub(/^[[:space:]]*/, "", value)
            sub(/[[:space:]]*$/, "", value)
            if (value ~ /^".*"$/ || value ~ /^'"'"'.*'"'"'$/) {
                value = substr(value, 2, length(value) - 2)
            }
            print value
            exit
        }
    ' "$root/.env"
}

effective_value() {
    key="$1"
    fallback="$2"
    value="${!key:-}"
    [ -n "$value" ] || value="$(env_file_value "$key")"
    printf '%s\n' "${value:-$fallback}"
}

port_is_listening() {
    port="$1"
    if command -v lsof >/dev/null 2>&1; then
        lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1
        return
    fi
    (exec 3<>"/dev/tcp/127.0.0.1/$port") >/dev/null 2>&1
}

port_holder() {
    port="$1"
    if command -v lsof >/dev/null 2>&1; then
        lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null |
            awk 'NR == 2 { printf "%s pid=%s", $1, $2 }'
        return
    fi
    printf 'unknown holder (lsof is not installed)'
}

compose_owns_port() {
    service="$1"
    target_port="$2"
    host_port="$3"
    [ "$compose_ok" = true ] || return 1
    docker compose -f "$root/docker-compose.yaml" port "$service" "$target_port" 2>/dev/null |
        grep -Eq ":${host_port}$"
}

check_port() {
    label="$1"
    variable="$2"
    fallback="$3"
    service="$4"
    target_port="$5"
    port="$(effective_value "$variable" "$fallback")"
    case "$port" in
        ''|*[!0-9]*)
            fail "$label" "$variable is '$port'" "set $variable to a numeric TCP port"
            return
            ;;
    esac
    if ! port_is_listening "$port"; then
        pass "$label" "$port is free"
    elif compose_owns_port "$service" "$target_port" "$port"; then
        pass "$label" "$port is held by Compose service $service"
    else
        holder="$(port_holder "$port")"
        fail "$label" "$port is held by ${holder:-an unknown process}" "stop that listener or set $variable to a free port"
    fi
}

if [ -n "${BASH_VERSION:-}" ] && version_ge "$BASH_VERSION" "4.4"; then
    pass "bash" "$BASH_VERSION"
else
    fail "bash" "${BASH_VERSION:-not running under bash}" "install Bash 4.4+ (macOS: brew install bash) and put it before /bin in PATH"
fi

required_go="$(awk '$1 == "go" { print $2; exit }' "$root/go.mod")"
if command -v go >/dev/null 2>&1; then
    actual_go="$(go env GOVERSION 2>/dev/null || true)"
    if [ "$actual_go" = "go$required_go" ]; then
        pass "go" "$actual_go matches go.mod"
    else
        fail "go" "found ${actual_go:-unknown}, need go$required_go" "install Go $required_go or invoke tasks with GOTOOLCHAIN=go$required_go"
    fi
else
    fail "go" "not found" "install Go $required_go"
fi

if command -v task >/dev/null 2>&1; then
    pass "task" "$(task --version 2>/dev/null || printf 'installed')"
else
    fail "task" "not found" "install go-task (https://taskfile.dev/installation/)"
fi

if command -v docker >/dev/null 2>&1; then
    if docker info >/dev/null 2>&1; then
        docker_ok=true
        pass "docker daemon" "reachable"
    else
        fail "docker daemon" "docker is installed but the daemon is unreachable" "start Docker Desktop/Colima and select its Docker context"
    fi
else
    fail "docker daemon" "docker not found" "install Docker with the Compose plugin"
fi

if [ "$docker_ok" = true ] && docker compose version >/dev/null 2>&1; then
    compose_ok=true
    pass "docker compose" "$(docker compose version --short 2>/dev/null || docker compose version 2>/dev/null)"
else
    fail "docker compose" "the 'docker compose' interface is unavailable" "install the Docker Compose v2 plugin"
fi

if command -v node >/dev/null 2>&1; then
    node_version="$(node -p 'process.versions.node' 2>/dev/null || true)"
    if version_ge "$node_version" "22.12.0"; then
        pass "node" "v$node_version"
    else
        fail "node" "found v${node_version:-unknown}, need >=22.12.0" "install Node 22.12+"
    fi
else
    fail "node" "not found" "install Node 22.12+"
fi

if command -v pnpm >/dev/null 2>&1; then
    pass "pnpm" "$(pnpm --version 2>/dev/null || printf 'installed')"
else
    fail "pnpm" "not found" "install pnpm (corepack enable pnpm)"
fi

check_port "postgres port" "POSTGRES_HOST_PORT" "5434" "postgres" "5432"
check_port "garnet port" "GARNET_HOST_PORT" "6380" "garnet" "6379"
check_port "openrails port" "OPENRAILS_HOST_PORT" "3053" "openrails" "3053"
check_port "issuer port" "AUTHKIT_ISSUER_HOST_PORT" "8081" "issuer" "8080"

if command -v psql >/dev/null 2>&1; then
    pass "postgres client" "psql is available"
elif [ "$compose_ok" = true ]; then
    pass "postgres client" "host psql absent; scripts use the Compose postgres client fallback"
else
    fail "postgres client" "neither psql nor a Compose fallback is available" "install PostgreSQL client tools or Docker Compose"
fi

lint_pin="$(awk -F= '$1 == "GOLANGCI_LINT_VERSION" { print $2; exit }' "$root/tools/versions.conf")"
if command -v golangci-lint >/dev/null 2>&1; then
    lint_output="$(golangci-lint version 2>/dev/null || true)"
    case "$lint_output" in
        *" version ${lint_pin#v} "*) pass "golangci-lint" "$lint_pin" ;;
        *) fail "golangci-lint" "installed version does not match $lint_pin" "go install github.com/golangci-lint/v2/cmd/golangci-lint@$lint_pin" ;;
    esac
else
    fail "golangci-lint" "not found (required $lint_pin)" "go install github.com/golangci-lint/v2/cmd/golangci-lint@$lint_pin"
fi

if [ ! -f "$root/.env" ]; then
    fail ".env" "missing" "cp .env.example .env"
else
    env_errors=""
    postgres_port="$(effective_value "POSTGRES_HOST_PORT" "5434")"
    db_url="$(env_file_value "DB_URL")"
    db_port="$(env_file_value "DB_PORT")"
    provider_mode="$(env_file_value "PROVIDER_WRITE_MODE")"
    case "$db_url" in
        *'${POSTGRES_HOST_PORT}'*) ;;
        *)
            url_port="$(printf '%s\n' "$db_url" | sed -nE 's#^[a-zA-Z0-9+.-]+://[^/]*:([0-9]+)/.*#\1#p')"
            [ "$url_port" = "$postgres_port" ] || env_errors="DB_URL port ${url_port:-missing} != $postgres_port; "
            ;;
    esac
    case "$db_port" in
        '${POSTGRES_HOST_PORT}') ;;
        "$postgres_port") ;;
        *) env_errors="${env_errors}DB_PORT ${db_port:-missing} != $postgres_port; " ;;
    esac
    [ -n "$provider_mode" ] || env_errors="${env_errors}PROVIDER_WRITE_MODE is empty; "
    if [ -z "$env_errors" ]; then
        pass ".env" "database port and PROVIDER_WRITE_MODE are consistent"
    else
        fail ".env" "$env_errors" "align DB_URL/DB_PORT with POSTGRES_HOST_PORT and set PROVIDER_WRITE_MODE"
    fi
fi

docker_host="${DOCKER_HOST:-}"
if [ -z "$docker_host" ] && [ "$docker_ok" = true ]; then
    docker_host="$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)"
fi
case "$docker_host" in
    *colima*)
        if [ -n "${DOCKER_HOST:-}" ] && [ "${TESTCONTAINERS_RYUK_DISABLED:-}" = "true" ]; then
            pass "testcontainers" "Colima socket configured with Ryuk disabled"
        else
            fail "testcontainers" "Colima detected at $docker_host" "export DOCKER_HOST='$docker_host' and TESTCONTAINERS_RYUK_DISABLED=true"
        fi
        ;;
    *) pass "testcontainers" "Docker is not using a Colima socket" ;;
esac

tracked_hooks=0
for source_hook in "$root"/scripts/hooks/*; do
    [ -f "$source_hook" ] || continue
    tracked_hooks=$((tracked_hooks + 1))
    hook="$(basename "$source_hook")"
    hook_path="$(git -C "$root" rev-parse --git-path "hooks/$hook" 2>/dev/null || true)"
    case "$hook_path" in
        /*) ;;
        *) hook_path="$root/$hook_path" ;;
    esac
    if [ -L "$hook_path" ] && [ "$(readlink "$hook_path" 2>/dev/null || true)" = "$source_hook" ] && [ -x "$source_hook" ]; then
        pass "git hook $hook" "installed from scripts/hooks/$hook"
    else
        fail "git hook $hook" "not installed from scripts/hooks/$hook" "run ./scripts/install-git-hooks.sh"
    fi
done
[ "$tracked_hooks" -gt 0 ] || fail "git hooks" "no tracked hooks found" "restore scripts/hooks from git"

if [ "$failures" -gt 0 ]; then
    printf '\ndoctor: %s check(s) failed\n' "$failures" >&2
    exit 1
fi
printf '\ndoctor: all checks passed\n'
