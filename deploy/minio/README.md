# MinIO source image

The Compose stack builds `asker-minio:RELEASE.2025-10-15T17-29-55Z` locally.
MinIO's community distribution is now source-only; the former
`minio/minio:RELEASE.2025-09-07T16-13-09Z` reference no longer pulls from Docker Hub.
The [Dockerfile](Dockerfile) pins official server/client source commits and archive
checksums, plus builder/runtime image digests. It keeps `mc` for the GDPR drill's
object assertions and uses MinIO's HTTP readiness endpoint for container health.

Upstream sources: [MinIO security release](https://github.com/minio/minio/releases/tag/RELEASE.2025-10-15T17-29-55Z)
and [mc release](https://github.com/minio/mc/releases/tag/RELEASE.2025-08-13T08-35-41Z).
Their license notices are included in the image under `/usr/share/licenses`.
The community repositories are archived; updates to these compatibility
sources and their dependencies require an explicit review.

`make dev-up` includes this image in its serial build. To build it independently:

```sh
docker build -t asker-minio:RELEASE.2025-10-15T17-29-55Z deploy/minio
```

The Helm chart references the same local tag when `stateful.minio.deploy=true`.
For a local kind cluster named `asker`, load that built image before installing:

```sh
kind load docker-image --name asker asker-minio:RELEASE.2025-10-15T17-29-55Z
```

For another Kubernetes cluster, publish the image to your own registry and set
`stateful.minio.image` to that complete registry reference in your values file.
Use a digest to pin the published image. Build for the cluster's architecture
with `docker buildx` if it differs from the build host. The slim CI profile
disables MinIO and does not require this image.
