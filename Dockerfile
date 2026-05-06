# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" \
    -o /out/notifier ./cmd/notifier

FROM gcr.io/distroless/static-debian12:latest
WORKDIR /app
COPY --from=build /out/notifier /usr/local/bin/notifier
ENTRYPOINT ["/usr/local/bin/notifier"]
CMD ["--config", "/app/config.yaml"]
