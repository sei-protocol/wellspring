# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /usr/bin/wellspring ./cmd/wellspring
RUN mkdir /data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /usr/bin/wellspring /usr/bin/wellspring
# A fresh volume copies this directory, so the nonroot user can write to it.
COPY --from=build --chown=65532:65532 /data /data
ENV WELLSPRING_DB_PATH=/data/wellspring.db
VOLUME /data
EXPOSE 8080 9090
# Numeric, so Kubernetes runAsNonRoot can verify it.
USER 65532:65532
ENTRYPOINT ["/usr/bin/wellspring"]
