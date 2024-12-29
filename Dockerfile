FROM golang:1.22.5 AS builder
WORKDIR /work

ARG VERSION

COPY ./go.mod ./go.sum ./
RUN go mod download && go mod verify
COPY ./ ./

RUN VERSION=${VERSION:-$(git describe)} \
 && BUILD_MACHINE=$(uname -srmo) \
 && BUILD_TIME=$(date) \
 && GO_VERSION=$(go version) \
 && go build -ldflags "-s -w -X main.version=${VERSION} -X \"main.buildMachine=${BUILD_MACHINE}\" -X \"main.buildTime=${BUILD_TIME}\" -X \"main.goVersion=${GO_VERSION}\"" -o ccworld-ap-relay .

FROM ubuntu:latest
RUN apt-get update && apt-get install -y ca-certificates curl --no-install-recommends && rm -rf /var/lib/apt/lists/*

COPY --from=builder /work/ccworld-ap-relay /usr/local/bin

CMD ["ccworld-ap-relay"]
