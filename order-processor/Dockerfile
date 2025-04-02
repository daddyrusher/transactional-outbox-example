FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o order-processor

FROM scratch
COPY --from=builder /app/order-processor /order-processor
EXPOSE 8081
ENTRYPOINT ["/order-processor"]
