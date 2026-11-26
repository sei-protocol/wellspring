# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /usr/bin/wellspring ./cmd/wellspring
RUN mkdir /data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=build /usr/bin/wellspring /usr/bin/wellspring
# A fresh volume copies this directory, so the nonroot user can write to it.
COPY --from=build --chown=65532:65532 /data /data
ENV WELLSPRING_DB_PATH=/data/wellspring.db
VOLUME /data
EXPOSE 8080 9090
# Numeric, so Kubernetes runAsNonRoot can verify it.
USER 65532:65532
ENTRYPOINT ["/usr/bin/wellspring"]
