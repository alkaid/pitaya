# Build the k6 binary with the extension
ARG PITAYA_REVISION
FROM golang:1.26.5@sha256:3aff6657219a4d9c14e27fb1d8976c49c29fddb70ba835014f477e1c70636647 AS builder

ARG PITAYA_REVISION
ARG GOPROXY="https://goproxy.cn|https://proxy.golang.org|direct"
ENV GOPROXY=$GOPROXY

RUN GOWORK=off go install go.k6.io/xk6/cmd/xk6@v1.4.7

WORKDIR /src
COPY .pitaya-docker-context /tmp/pitaya-source-revision
COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -e; \
    test -n "$PITAYA_REVISION"; \
    test "$(cat /tmp/pitaya-source-revision)" = "$PITAYA_REVISION"; \
    attempt=1; \
    while [ "$attempt" -le 3 ]; do \
        if GOWORK=off xk6 --verbose build v1.8.0 \
            --build-flags=-trimpath \
            --build-flags="-ldflags=-s -w" \
            --build-flags=-buildvcs=false \
            --with github.com/alkaid/pitaya/xk6-pitaya=./xk6-pitaya \
            --with github.com/alkaid/pitaya/v2=. \
            --output /k6; then \
            exit 0; \
        fi; \
        attempt=$((attempt + 1)); \
    done; \
    exit 1

# Use the operator's base image and override the k6 binary
FROM grafana/k6:1.8.0@sha256:b992f241070f3f3a7d78096fa6020db1edcda49297ee8ed9eb0ab847ef3dcb32
ARG PITAYA_REVISION
LABEL org.opencontainers.image.revision=$PITAYA_REVISION
COPY --from=builder /k6 /usr/bin/k6
