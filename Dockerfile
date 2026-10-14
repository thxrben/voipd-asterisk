# voipd control plane. Pure Go (no cgo); deploy/build.sh builds it with the
# Asterisk image, natively (also with the classic builder, e.g. on the Pi)
# or for linux/arm64 with buildx. No FROM --platform=$BUILDPLATFORM: the
# classic builder cannot parse it, so buildx runs these stages emulated.
# Web UI: built once (it is platform independent) and embedded into voipd.
FROM node:24-alpine AS ui
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.27.2 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/voipd ./cmd/voipd \
    && mkdir -p /out/data/voicemail /out/generated

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/voipd /usr/local/bin/voipd
# A fresh named volume copies this directory's ownership, so voipd can
# create its database without running as root (and use the voicemail
# volume mounted below it).
COPY --from=build --chown=65532:65532 /out/data /var/lib/voipd
# Same for the generated Asterisk config: voipd starts first and must be
# able to write it into a fresh shared volume.
COPY --from=build --chown=65532:65532 /out/generated /etc/asterisk/voipd
# 65532 = distroless "nonroot"; must match VOIPD_UID in the asterisk service.
USER 65532:65532
VOLUME /var/lib/voipd
ENTRYPOINT ["/usr/local/bin/voipd"]
