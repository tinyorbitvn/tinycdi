# Frontend image (tinycdi-frontend): static SPA (web/, built in-stage so the
# image builds from a clean checkout) + a small Go server that serves it
# with the portal security headers. It does not proxy /v1 — the edge routes
# that path to the backend. Build context is the repo root.
ARG NODE_IMAGE=docker.io/library/node:22.23.3-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c
ARG BASE_IMAGE=docker.io/library/golang:1.26@sha256:6c2a5538f964f1c82f97ad14988bf05de100d922d159d0e398b54c7b0ca0c6c9

FROM ${NODE_IMAGE} AS web-builder
WORKDIR /web
COPY web/ ./
# npm ci removes any node_modules that rode along in the context, then vite
# writes a fresh dist/.
RUN npm ci --no-audit --no-fund && npm run build

FROM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o frontend ./build/frontend
RUN GOBIN=/workspace/bin go install github.com/google/go-licenses/v2@v2.0.1 \
 && /workspace/bin/go-licenses save --save_path=/workspace/licenses --force --ignore github.com/tinyorbitvn/tinycdi ./build/frontend; \
 mkdir -p /workspace/licenses

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
WORKDIR /
COPY --from=builder /workspace/frontend .
COPY --from=builder /workspace/licenses /licenses
COPY --from=web-builder /web/dist /srv/web
USER 65532:65532
ENTRYPOINT ["/frontend"]
