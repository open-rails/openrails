#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

if [ -e .env ] || [ -L .env ]; then
    echo "setup: preserving existing .env"
else
    cp .env.example .env
    echo "setup: created .env from .env.example"
fi

# Doctor verifies tracked hooks, so install clone-owned hooks before asking it
# to validate the completed setup.
./scripts/install-git-hooks.sh
bash ./scripts/doctor.sh
go mod download
docker volume create go_mod_cache_persistent >/dev/null

cat <<'EOF'

Setup complete. Next commands:
  task docker-up
  task test
  task run
EOF
