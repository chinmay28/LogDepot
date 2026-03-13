FROM golang:1.22-alpine AS builder

RUN apk add --no-cache make

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make build

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /src/logdepot /usr/local/bin/logdepot

# Default port.
EXPOSE 8080
ENV LISTEN_ADDR=:8080

ENTRYPOINT ["logdepot"]
