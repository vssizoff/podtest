#!/usr/bin/env bash
set -euo pipefail
umask 077

target=${PODTEST_DEV_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/podtest-dev}
mkdir -m 0700 -p "$target" "$target/minio" "$target/state"
target=$(realpath "$target")
if [[ -e $target/invoker.json ]]; then
    printf 'Existing configuration at %s\n' "$target"
    exit 0
fi
invoker_id=$(cat /proc/sys/kernel/random/uuid)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -noenc -days 365 \
    -subj '/CN=Podtest local dev CA' -addext 'basicConstraints=critical,CA:TRUE' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' -keyout "$target/ca.key" -out "$target/ca.crt"
issue() {
    local basename=$1 common_name=$2 extension=$3
    openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -noenc -subj "/CN=$common_name" \
        -keyout "$target/$basename.key" -out "$target/$basename.csr"
    openssl x509 -req -in "$target/$basename.csr" -CA "$target/ca.crt" -CAkey "$target/ca.key" \
        -CAcreateserial -days 365 -out "$target/$basename.crt" \
        -extfile <(printf '%s\n' 'basicConstraints=critical,CA:FALSE' "$extension")
    rm -- "$target/$basename.csr"
}
issue backend 127.0.0.1 $'extendedKeyUsage=serverAuth\nsubjectAltName=IP:127.0.0.1'
issue invoker "$invoker_id" 'extendedKeyUsage=clientAuth'
issue minio/private minio $'extendedKeyUsage=serverAuth\nsubjectAltName=DNS:minio,IP:127.0.0.1'
mv -- "$target/minio/private.crt" "$target/minio/public.crt"

python3 - "$target" "$invoker_id" "${XDG_RUNTIME_DIR:?}/podman/podman.sock" <<'PY'
import json
from pathlib import Path
import subprocess
import sys

target, invoker, socket = sys.argv[1:]
version = subprocess.check_output(['podman', 'version', '--format', '{{.Client.APIVersion}}'], text=True).strip()
Path(target, 'invoker.json').write_text(json.dumps(dict(
    invoker_id=invoker, state_dir=str(Path(target, 'state')), podman_socket=socket,
    max_active_runs=1, libpod_api_version=version,
), indent=2) + '\n')
PY
printf 'Configuration at %s\n' "$target"
