FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o order-service

FROM scratch
COPY --from=builder /app/order-service /order-service
EXPOSE 8080
ENTRYPOINT ["/order-service"]