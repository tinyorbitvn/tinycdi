# Kasm workspace adapter image (tinycdi-kasm-adapter).
#
# Delivers the adapter scripts (entrypoint.sh, healthcheck.sh, xstartup.sh)
# into a shared emptyDir mounted at /opt/tcdi for templates using
# spec.linux.adapter=kasm — the initContainer for UNMODIFIED kasmweb/*
# runtime images (docs/kasm-images.md). Static binary, scratch-sized.
#
# Pinned bases per docs/compatibility.md (digest-pinned, same pins as the
# other images). Build context is the repo root.
ARG BASE_IMAGE=gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG BUILD_IMAGE=docker.io/library/golang:1.26@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89

FROM ${BUILD_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY build/kasm-adapter/ ./build/kasm-adapter/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o install-adapter ./build/kasm-adapter

FROM ${BASE_IMAGE}
COPY LICENSE NOTICE THIRD_PARTY_LICENSES.md SOURCE-OFFER /usr/share/doc/tinycdi/
COPY --from=builder /workspace/install-adapter /install-adapter
USER 65532:65532
ENTRYPOINT ["/install-adapter"]
