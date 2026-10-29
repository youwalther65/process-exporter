# Building and pushing the multi-arch image

How the `process-exporter` images used by the Auto Mode swap experiment are built: one tag, one
OCI image index, an amd64 and an arm64 image behind it. The target registry is the experiment
account's private ECR, `618668373247.dkr.ecr.eu-west-1.amazonaws.com/process-exporter`, which the
Auto Mode node role can pull from.

## Commands

```bash
# 1. the code to build
git checkout master && git pull
TAG=$(git rev-parse --short HEAD)

# 2. log in to the private ECR (the only login needed)
REPO=618668373247.dkr.ecr.eu-west-1.amazonaws.com/process-exporter
aws ecr get-login-password --region eu-west-1 \
  | docker login --username AWS --password-stdin 618668373247.dkr.ecr.eu-west-1.amazonaws.com

# 3. a buildx builder, once per build host
docker buildx create --use --name pe-builder 2>/dev/null || docker buildx use pe-builder

# 4. build both architectures and push
docker buildx build --platform linux/amd64,linux/arm64 \
  -t "$REPO:$TAG" -t "$REPO:latest" --push .

# 5. the digest to pin in the DaemonSet
docker buildx imagetools inspect "$REPO:$TAG" --format '{{json .Manifest.Digest}}'
```

Pin the **index** digest that step 5 prints, not one of the per-platform digests. A node pulling
the index picks the image for its own architecture.

The build stage pulls `golang:1.26` from Docker Hub anonymously. If Docker Hub rate-limits the
host (`toomanyrequests`), either `docker login` to Docker Hub once, or take the base image from
the ECR Public mirror, which also needs no login:

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-context golang:1.26=docker-image://public.ecr.aws/docker/library/golang:1.26 \
  -t "$REPO:$TAG" -t "$REPO:latest" --push .
```

## Checking the builder

```bash
docker buildx ls                     # all builders; the one in use has a "*"
docker buildx inspect --bootstrap    # the current builder, its status and platforms
```

## Why an amd64-only builder can produce the arm64 image

A builder whose `Platforms` line lists only `linux/amd64` variants still produces a working arm64
image here, without QEMU, because nothing in the Dockerfile runs on the target architecture:

1. The build stage is `FROM --platform=$BUILDPLATFORM golang:1.26`, so for both target
   platforms it runs the amd64 Go image natively on the builder.
2. `CGO_ENABLED=0 GOARCH=$TARGETARCH make build` cross-compiles. The amd64 Go compiler writes an
   arm64 binary for the arm64 target, which is ordinary Go cross-compilation.
3. The final stage is `FROM scratch` and a single `COPY` of that binary. `COPY` is a file
   operation; no `RUN` executes on the target platform.

QEMU emulation (`docker run --privileged --rm tonistiigi/binfmt --install arm64`) is only needed
if a stage that runs on the target platform contains a `RUN`, for example `FROM alpine` followed
by `RUN apk add ...`.

## What the pushed tag looks like

`docker buildx imagetools inspect` on a tag built this way, `f60c4a9` (master after PR #8):

```
$ docker buildx imagetools inspect "$REPO:f60c4a9"
Name:      618668373247.dkr.ecr.eu-west-1.amazonaws.com/process-exporter:f60c4a9
MediaType: application/vnd.oci.image.index.v1+json
Digest:    sha256:8dad5be8b85cf993be21041a685dcc56c23bdbea42c67515255607676648d851

Manifests:
  Name:        618668373247.dkr.ecr.eu-west-1.amazonaws.com/process-exporter:f60c4a9@sha256:4d65973d832b4f650e7f806472a7ae249578d1eac9cad9e92854e338a3737544
  MediaType:   application/vnd.oci.image.manifest.v1+json
  Platform:    linux/amd64

  Name:        618668373247.dkr.ecr.eu-west-1.amazonaws.com/process-exporter:f60c4a9@sha256:bf7c1aac1cd40638c38c8c26934bda479efe4c335de32b41d0d824685eada61c
  MediaType:   application/vnd.oci.image.manifest.v1+json
  Platform:    linux/arm64

  Name:        618668373247.dkr.ecr.eu-west-1.amazonaws.com/process-exporter:f60c4a9@sha256:0f0c1cf487e451dab2a8fde00e7c9972d22e52adf1edeada0e073cd932fbfd09
  MediaType:   application/vnd.oci.image.manifest.v1+json
  Platform:    unknown/unknown
  Annotations:
    vnd.docker.reference.type:   attestation-manifest
    vnd.docker.reference.digest: sha256:4d65973d832b4f650e7f806472a7ae249578d1eac9cad9e92854e338a3737544

  Name:        618668373247.dkr.ecr.eu-west-1.amazonaws.com/process-exporter:f60c4a9@sha256:52cdfe30682ee292bd56dc411d2ec1d40eac5cb0b2c03f51c1d3be9c7777db3c
  MediaType:   application/vnd.oci.image.manifest.v1+json
  Platform:    unknown/unknown
  Annotations:
    vnd.docker.reference.digest: sha256:bf7c1aac1cd40638c38c8c26934bda479efe4c335de32b41d0d824685eada61c
    vnd.docker.reference.type:   attestation-manifest
```

How to read it:

| Entry | What it is |
|---|---|
| `Digest` at the top, `application/vnd.oci.image.index.v1+json` | the index, what the tag points to and what to pin |
| `Platform: linux/amd64` and `linux/arm64` | the two images; a node pulls the one matching its architecture |
| `Platform: unknown/unknown`, `attestation-manifest` | the build provenance buildx attaches, one per image; `vnd.docker.reference.digest` names the image it describes |
