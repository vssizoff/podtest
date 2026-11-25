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
if [ -n "${PODTEST_NPM_REGISTRY:-}" ]; then
    compose "$@" build --build-arg "NPM_CONFIG_REGISTRY=$PODTEST_NPM_REGISTRY" backend-api nginx
else
    compose "$@" build backend-api nginx
fi
# The optional service name expands to zero or one arguments.
compose "$@" up -d --no-build --no-deps postgres nats minio $oidc_service
compose "$@" run --rm --no-deps db-migrate
compose "$@" run --rm --no-deps openfga-migrate
compose "$@" up -d --no-build --no-deps openfga
compose "$@" run --rm --no-deps storage-init
compose "$@" run --rm --no-deps openfga-model
compose "$@" up -d --no-build --no-deps --force-recreate --wait --wait-timeout 60 backend-api backend-worker nginx
