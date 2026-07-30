# syntax=docker/dockerfile:1.18
FROM golang:1.25.12-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN release_version="${VERSION#v}" && \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${release_version}" \
    -o /out/telegram-bot-mux ./cmd/telegram-bot-mux

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/telegram-bot-mux /usr/local/bin/telegram-bot-mux
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/telegram-bot-mux"]
