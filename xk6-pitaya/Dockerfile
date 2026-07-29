# Build the k6 binary with the extension
FROM golang:1.26.5@sha256:3aff6657219a4d9c14e27fb1d8976c49c29fddb70ba835014f477e1c70636647 AS builder

ARG pitaya_revision

RUN go install go.k6.io/xk6/cmd/xk6@v1.4.7
RUN test -n "${pitaya_revision}" && xk6 build v1.8.0 \
    --build-flags=-trimpath \
    --build-flags="-ldflags=-s -w" \
    --build-flags=-buildvcs=false \
    --with github.com/alkaid/pitaya/xk6-pitaya@${pitaya_revision} \
    --with github.com/alkaid/pitaya/v2@${pitaya_revision} \
    --output /k6

# Use the operator's base image and override the k6 binary
FROM grafana/k6:1.8.0@sha256:b992f241070f3f3a7d78096fa6020db1edcda49297ee8ed9eb0ab847ef3dcb32
COPY --from=builder /k6 /usr/bin/k6
