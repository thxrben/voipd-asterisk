# voipd control plane. Pure Go (no cgo), so cross-compiling for the
# Raspberry Pi is just GOARCH=arm64; with buildx:
#   docker buildx build --platform linux/arm64,linux/amd64 -t voipd .
# Without buildx it builds for the host architecture (e.g. on the Pi).
FROM golang:1.27.2 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/voipd ./cmd/voipd \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/voipd /usr/local/bin/voipd
# A fresh named volume copies this directory's ownership, so voipd can
# create its database without running as root.
COPY --from=build --chown=65532:65532 /out/data /var/lib/voipd
# 65532 = distroless "nonroot"; must match VOIPD_UID in the asterisk service.
USER 65532:65532
VOLUME /var/lib/voipd
ENTRYPOINT ["/usr/local/bin/voipd"]
