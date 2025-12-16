FROM docker.io/library/golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN GOOS=linux go build .

FROM docker.io/library/alpine:latest

COPY --from=builder /app/streamserver .
EXPOSE 8181
ENTRYPOINT ["./streamserver", "-a", "0.0.0.0:8181", "-e", "https://streams.hoppenr.xyz/oauth-callback"]
