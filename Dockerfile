# Build stage
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o webshare-proxyd ./cmd/webshare-proxyd

# Runtime stage
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

RUN addgroup -g 1000 -S appgroup && \
    adduser -u 1000 -S appuser -G appgroup

WORKDIR /app

COPY --from=builder /build/webshare-proxyd /app/webshare-proxyd

RUN mkdir -p /data && chown -R appuser:appgroup /app /data

USER appuser

EXPOSE 9090

ENTRYPOINT ["/app/webshare-proxyd"]
CMD ["run", "--data-dir", "/data", "--web-bind", "0.0.0.0:9090"]
