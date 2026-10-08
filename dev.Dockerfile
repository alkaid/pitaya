FROM golang:1.27.1

ARG GOPROXY=""
ARG GOPRIVATE=""
WORKDIR /usr/src/pitaya
COPY . .
RUN go mod download

CMD []
