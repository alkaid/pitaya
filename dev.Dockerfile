FROM golang:1.17.5

ARG GOPROXY=""
ARG GOPRIVATE=""
WORKDIR /usr/src/pitaya
COPY . .
RUN go mod download

CMD []
