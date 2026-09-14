# Build stage. modernc.org/sqlite is pure Go, so CGO can stay off and the
# resulting binary is fully static.
FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/eventd ./cmd/eventd

FROM alpine:3.20
RUN adduser -D -u 10001 eventd \
 && mkdir -p /data \
 && chown -R eventd:eventd /data
COPY --from=build /out/eventd /usr/local/bin/eventd

ENV EVENTD_HTTP_ADDR=:8080 \
    EVENTD_DB_PATH=/data/eventd.db
VOLUME ["/data"]
EXPOSE 8080

USER eventd
ENTRYPOINT ["/usr/local/bin/eventd"]
