FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/proxy .

FROM alpine:3.20
RUN adduser -D -H -u 10001 proxy && apk add --no-cache ca-certificates wget
USER proxy
COPY --from=build /out/proxy /usr/local/bin/proxy
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/proxy"]
