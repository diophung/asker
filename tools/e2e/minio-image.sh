#!/usr/bin/env bash
# Exercise the source-built image without Compose, host ports, or shared data.
set -euo pipefail

image=${1:-asker-minio:RELEASE.2025-10-15T17-29-55Z}
container_id=
cleanup() {
  if [[ -n "$container_id" ]]; then
    docker rm -f "$container_id" >/dev/null
  fi
}
trap cleanup EXIT

# Match the Helm UID and also exercise a read-only root filesystem. Data is a fresh temporary
# mount owned by that UID; no existing stack, named volume, or network is used.
container_id=$(docker run -d --network none --user 1000:1000 --read-only \
  --tmpfs /data:uid=1000,gid=1000 --tmpfs /tmp:mode=1777 \
  -e MINIO_ROOT_USER=asker-image-smoke -e MINIO_ROOT_PASSWORD=asker-image-smoke-secret \
  "$image" server /data --console-address :9001)

ready=false
for ((attempt=0; attempt<60; attempt++)); do
  if docker exec "$container_id" wget -q -O /dev/null http://127.0.0.1:9000/minio/health/ready >/dev/null 2>&1; then
    ready=true
    break
  fi
  if [[ $(docker inspect -f '{{.State.Running}}' "$container_id") != true ]]; then
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  docker logs --tail 30 "$container_id" >&2
  echo "MinIO image did not become ready" >&2
  exit 1
fi

server_version=$(docker exec "$container_id" minio --version)
client_version=$(docker exec "$container_id" mc --version)
[[ "$server_version" == *RELEASE.2025-10-15T17-29-55Z* ]]
[[ "$client_version" == *RELEASE.2025-08-13T08-35-41Z* ]]
docker exec "$container_id" wget -q -O /dev/null http://127.0.0.1:9000/minio/health/live
docker exec "$container_id" sh -ec '
  mc alias set local http://127.0.0.1:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null
  mc ready local >/dev/null
  mc mb local/image-smoke >/dev/null
  printf "%s" "asker-minio-roundtrip" | mc pipe local/image-smoke/payload.txt >/dev/null
  test "$(mc cat local/image-smoke/payload.txt)" = "asker-minio-roundtrip"
  mc stat local/image-smoke/payload.txt >/dev/null
  mc rm local/image-smoke/payload.txt >/dev/null
  if mc stat local/image-smoke/payload.txt >/dev/null 2>&1; then
    echo "Deleted object remained visible" >&2
    exit 1
  fi
  mc rb local/image-smoke >/dev/null
'
printf 'PASS: %s; UID 1000, readiness/liveness, mc S3 create/read/delete\n' "$image"
