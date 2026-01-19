# syntax=docker/dockerfile:1

FROM golang:1.24-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./

RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o /out/whatsapp-mcp ./


FROM debian:bookworm-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates util-linux \
  && rm -rf /var/lib/apt/lists/*

RUN useradd -r -u 10001 -g root app

WORKDIR /app

COPY --from=build /out/whatsapp-mcp /app/whatsapp-mcp
COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

VOLUME ["/app/store"]

EXPOSE 8080

ENV WHATSAPP_MCP_LISTEN_ADDR=":8080"

ENTRYPOINT ["/app/entrypoint.sh"]
