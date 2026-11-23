FROM golang:1.27.4-alpine AS builder
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/token-service .

# Distroless has no shell/package manager and runs as an unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=builder --chown=nonroot:nonroot /out/token-service /token-service
USER nonroot:nonroot
EXPOSE 50051 9090
ENTRYPOINT ["/token-service"]
