# Frontend image (tinycdi-frontend): static SPA (web/, built in-stage so the
# image builds from a clean checkout) + a small Go server that serves it
# with the portal security headers. It does not proxy /v1 — the edge routes
# that path to the backend. Build context is the repo root.
ARG NODE_IMAGE=docker.io/library/node:22.23.3-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c
ARG BASE_IMAGE=docker.io/library/golang:1.26@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89

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

FROM gcr.io/distroless/static:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
WORKDIR /
COPY --from=builder /workspace/frontend .
COPY --from=builder /workspace/licenses /licenses
COPY --from=web-builder /web/dist /srv/web
USER 65532:65532
ENTRYPOINT ["/frontend"]
