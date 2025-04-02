package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Shopify/sarama"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"

	"go.uber.org/zap"
)

type Order struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

type OutboxEvent struct {
	ID          int64
	AggregateID int64
	Type        string
	Payload     string
	Processed   bool
}

var (
	db            *sql.DB
	kafkaProducer sarama.SyncProducer
	logger        *zap.Logger

	orderCreatedCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "order_created_total",
			Help: "Total number of orders created",
		},
	)
)

func initMetrics() {
	prometheus.MustRegister(orderCreatedCounter)
}

func initTracer(ctx context.Context) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint("tempo:4318"),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String("order-service"),
			attribute.String("environment", "demo"),
		)),
	)
	otel.SetTracerProvider(tp)
	return tp, nil
}

func main() {
	var err error

	logger, err = zap.NewProduction()
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}
	defer logger.Sync()

	ctx := context.Background()
	tp, err := initTracer(ctx)
	if err != nil {
		logger.Fatal("Failed to initialize OpenTelemetry", zap.Error(err))
	}
	defer func() {
		if err := tp.Shutdown(ctx); err != nil {
			logger.Error("Error shutting down tracer provider", zap.Error(err))
		}
	}()

	postgresHost := os.Getenv("POSTGRES_HOST")
	postgresUser := os.Getenv("POSTGRES_USER")
	postgresPassword := os.Getenv("POSTGRES_PASSWORD")
	postgresDB := os.Getenv("POSTGRES_DB")
	dsn := "host=" + postgresHost + " user=" + postgresUser + " password=" + postgresPassword + " dbname=" + postgresDB + " sslmode=disable"
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		logger.Fatal("Failed to connect to database", zap.Error(err))
	}
	defer db.Close()

	if err := createTables(); err != nil {
		logger.Fatal("Failed to create tables", zap.Error(err))
	}

	kafkaBroker := os.Getenv("KAFKA_BROKER")
	producer, err := initKafkaProducer([]string{kafkaBroker})
	if err != nil {
		logger.Fatal("Failed to initialize Kafka producer", zap.Error(err))
	}
	kafkaProducer = producer
	defer kafkaProducer.Close()

	initMetrics()

	go startOutboxProcessor()

	mux := http.NewServeMux()
	mux.Handle("/order", otelhttp.NewHandler(http.HandlerFunc(createOrderHandler), "CreateOrder"))
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/ready", readinessHandler)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		logger.Info("Order Service started", zap.String("port", "8080"))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("HTTP server error", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("Shutdown signal received, stopping service...")

	ctxShutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctxShutdown); err != nil {
		logger.Error("Error during HTTP server shutdown", zap.Error(err))
	}
	logger.Info("Service stopped")
}

func createTables() error {
	ordersTable := `
	CREATE TABLE IF NOT EXISTS orders (
		id SERIAL PRIMARY KEY,
		status TEXT NOT NULL,
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);`
	outboxTable := `
	CREATE TABLE IF NOT EXISTS outbox (
		id SERIAL PRIMARY KEY,
		aggregate_id INT NOT NULL,
		event_type TEXT NOT NULL,
		payload TEXT NOT NULL,
		processed BOOLEAN DEFAULT FALSE,
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := db.Exec(ordersTable); err != nil {
		return err
	}
	_, err := db.Exec(outboxTable)
	return err
}

func createOrderHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, span := otel.Tracer("order-service").Start(ctx, "CreateOrderHandler")
	defer span.End()

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var order Order
	if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if order.Status == "" {
		order.Status = "created"
	}
	orderID, err := createOrderTransaction(ctx, order)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		span.RecordError(err)
		return
	}
	order.ID = orderID
	orderCreatedCounter.Inc()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(order)
}

func createOrderTransaction(ctx context.Context, order Order) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	var orderID int64
	err = tx.QueryRowContext(ctx, "INSERT INTO orders (status) VALUES ($1) RETURNING id", order.Status).Scan(&orderID)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	event := OutboxEvent{
		AggregateID: orderID,
		Type:        "OrderCreated",
	}
	payloadBytes, _ := json.Marshal(order)
	event.Payload = string(payloadBytes)
	_, err = tx.ExecContext(ctx, "INSERT INTO outbox (aggregate_id, event_type, payload) VALUES ($1, $2, $3)",
		event.AggregateID, event.Type, event.Payload)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return orderID, nil
}

func initKafkaProducer(brokers []string) (sarama.SyncProducer, error) {
	config := sarama.NewConfig()
	config.Producer.Return.Successes = true
	return sarama.NewSyncProducer(brokers, config)
}

func startOutboxProcessor() {
	ticker := time.NewTicker(5 * time.Second)
	for range ticker.C {
		processOutboxEvents()
	}
}

func processOutboxEvents() {
	rows, err := db.Query("SELECT id, aggregate_id, event_type, payload FROM outbox WHERE processed = false")
	if err != nil {
		logger.Error("Error querying outbox", zap.Error(err))
		return
	}
	defer rows.Close()

	for rows.Next() {
		var event OutboxEvent
		if err := rows.Scan(&event.ID, &event.AggregateID, &event.Type, &event.Payload); err != nil {
			logger.Error("Error scanning outbox row", zap.Error(err))
			continue
		}
		kafkaMsg := &sarama.ProducerMessage{
			Topic: "orders",
			Key:   sarama.StringEncoder(strconv.FormatInt(event.AggregateID, 10)),
			Value: sarama.StringEncoder(event.Payload),
		}
		partition, offset, err := kafkaProducer.SendMessage(kafkaMsg)
		if err != nil {
			logger.Error("Error sending message to Kafka", zap.Error(err))
			continue
		}
		logger.Info("Event sent to Kafka", zap.Int64("event_id", event.ID), zap.Int32("partition", partition), zap.Int64("offset", offset))
		_, err = db.Exec("UPDATE outbox SET processed = true WHERE id = $1", event.ID)
		if err != nil {
			logger.Error("Error updating outbox event as processed", zap.Error(err))
		}
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func readinessHandler(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(); err != nil {
		http.Error(w, "Database unreachable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
