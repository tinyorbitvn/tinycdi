# Merged backend image (cmd/backend): API + session gateway + in-process
# broker. Build context is the repo root. Same pinned toolchain and
# distroless base as the other Go images.
ARG BASE_IMAGE=docker.io/library/golang:1.26@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89
FROM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o backend ./cmd/backend
RUN GOBIN=/workspace/bin go install github.com/google/go-licenses/v2@v2.0.1 \
 && /workspace/bin/go-licenses save --save_path=/workspace/licenses --force --ignore github.com/tinyorbitvn/tinycdi ./cmd/backend

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
WORKDIR /
COPY --from=builder /workspace/backend .
COPY --from=builder /workspace/licenses /licenses
USER 65532:65532
ENTRYPOINT ["/backend"]
