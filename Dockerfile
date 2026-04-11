FROM golang:1.26-alpine AS builder

RUN apk --no-cache add gcc musl-dev

WORKDIR ${GOPATH}/src/github.com/baragoon/ofelia

COPY go.mod go.sum ${GOPATH}/src/github.com/baragoon/ofelia/
RUN go mod download

COPY . ${GOPATH}/src/github.com/baragoon/ofelia/

RUN go build -o /go/bin/ofelia .

FROM alpine:3.23

# this label is required to identify container with ofelia running
LABEL ofelia.service=true
LABEL ofelia.enabled=true

RUN apk add --no-cache \
    ca-certificates \
    tini \
    tzdata \
    'zlib>=1.3.2-r0' \
    'libssl3>=3.5.6-r0' \
    'libcrypto3>=3.5.6-r0' \
    'musl>=1.2.5-r23' \
    'musl-utils>=1.2.5-r23'

COPY --from=builder /go/bin/ofelia /usr/bin/ofelia

ENTRYPOINT ["/sbin/tini", "/usr/bin/ofelia"]

CMD ["daemon", "--config", "/etc/ofelia/config.ini"]
