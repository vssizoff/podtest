#!/bin/sh
set -eu
cd "$(dirname "$0")/.."

oidc_service=
if [ "${1:-}" = '--dev' ]; then
    shift
    oidc_service=dex
    set -- --env-file deploy/dev.env -f compose.yaml -f compose.dev.yaml "$@"
fi

compose() { podman-compose "$@"; }

# podman-compose 1.6 maps depends_on to Podman's --requires, including
# completed jobs. Start each stage explicitly so each job can exit normally.
if [ "${PODTEST_SKIP_BUILD:-0}" = 1 ]; then
    : # Use images built locally or provisioned before deployment.
elif [ -n "${PODTEST_NPM_REGISTRY:-}" ]; then
    compose "$@" build --build-arg "NPM_CONFIG_REGISTRY=$PODTEST_NPM_REGISTRY" backend-api nginx
else
    compose "$@" build backend-api nginx
fi
# The optional service name expands to zero or one arguments.
compose "$@" up -d --no-build --no-deps postgres nats minio $oidc_service
attempt=0
until compose "$@" exec -T postgres sh -c 'pg_isready -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 60 ]; then
        echo 'PostgreSQL did not become ready within 60 attempts' >&2
        exit 1
    fi
    sleep 1
done
compose "$@" run --rm --no-deps db-migrate
compose "$@" run --rm --no-deps openfga-migrate
compose "$@" up -d --no-build --no-deps openfga
compose "$@" run --rm --no-deps storage-init
compose "$@" run --rm --no-deps openfga-model
compose "$@" up -d --no-build --no-deps --force-recreate --wait --wait-timeout 60 backend-api backend-worker nginx
