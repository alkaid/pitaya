# Build from the repository root: docker build -f xk6-pitaya/Dockerfile .
FROM golang:1.27.1 AS builder
WORKDIR /src
COPY . .
RUN GOWORK=off go install go.k6.io/xk6/cmd/xk6@v1.4.14
RUN GOWORK=off xk6 build v0.46.0 \
    --with github.com/alkaid/pitaya/xk6-pitaya=/src/xk6-pitaya \
    --replace github.com/alkaid/pitaya/v2=/src \
    --output /k6

FROM grafana/k6:0.46.0
COPY --from=builder /k6 /usr/bin/k6
