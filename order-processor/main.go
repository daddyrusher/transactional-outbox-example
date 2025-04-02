package main

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Shopify/sarama"
	"go.uber.org/zap"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	logger *zap.Logger
)

func initTracer() func(context.Context) error {
	ctx := context.Background()

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint("tempo:4318"),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		logger.Fatal("Failed to create exporter: %v", zap.Error(err))
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("order-processor"),
			semconv.ServiceVersionKey.String("1.0.0"),
		),
	)
	if err != nil {
		logger.Fatal("Failed to create resource: %v", zap.Error(err))
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)

	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return tp.Shutdown(ctx)
	}
}

func main() {
	var err error
	logger, err = zap.NewProduction()
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}
	defer logger.Sync()

	ctx := context.Background()
	cleanup := initTracer()
	defer func() {
		if err := cleanup(ctx); err != nil {
			logger.Error("Error shutting down tracer provider: %v", zap.Error(err))
		}
	}()

	tracer := otel.Tracer("order-processor")

	kafkaBroker := os.Getenv("KAFKA_BROKER")
	config := sarama.NewConfig()
	config.Consumer.Return.Errors = true

	consumer, err := sarama.NewConsumer([]string{kafkaBroker}, config)
	if err != nil {
		logger.Fatal("Error creating Kafka consumer: ", zap.Error(err))
	}
	defer consumer.Close()

	topic := "orders"
	partitionConsumer, err := consumer.ConsumePartition(topic, 0, sarama.OffsetNewest)
	if err != nil {
		logger.Fatal("Error starting partition consumer: ", zap.Error(err))
	}
	defer partitionConsumer.Close()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	logger.Info("Order Processor is running and listening for events...")

consumerLoop:
	for {
		select {
		case msg := <-partitionConsumer.Messages():
			ctx, span := tracer.Start(
				context.Background(),
				"process_order_message",
				trace.WithAttributes(
					attribute.String("kafka.topic", topic),
					attribute.String("kafka.key", string(msg.Key)),
					attribute.Int64("kafka.partition", int64(msg.Partition)),
					attribute.Int64("kafka.offset", msg.Offset),
				),
			)

			logger.Info("Received message: ",
				zap.String("Key", string(msg.Key)), zap.String("Value", string(msg.Value)))

			processMessage(ctx, msg)

			span.End()

		case err := <-partitionConsumer.Errors():
			_, span := tracer.Start(context.Background(), "kafka_consumer_error")
			span.SetAttributes(attribute.String("error.message", err.Error()))
			logger.Error("Error: ", zap.Error(err))
			span.End()

		case <-signals:
			break consumerLoop
		}
	}
	logger.Info("Order Processor shutting down.")
}

func processMessage(ctx context.Context, msg *sarama.ConsumerMessage) {
	tracer := otel.Tracer("order-processor")

	ctx, parseSpan := tracer.Start(ctx, "parse_order_json")
	var orderData map[string]interface{}
	err := json.Unmarshal(msg.Value, &orderData)
	if err != nil {
		parseSpan.SetAttributes(attribute.String("error.message", err.Error()))
		logger.Error("Error parsing order JSON", zap.Error(err))
		parseSpan.End()
		return
	}

	if orderID, ok := orderData["order_id"].(string); ok {
		parseSpan.SetAttributes(attribute.String("order.id", orderID))
	}
	if customerID, ok := orderData["customer_id"].(string); ok {
		parseSpan.SetAttributes(attribute.String("customer.id", customerID))
	}
	parseSpan.End()

	ctx, processSpan := tracer.Start(ctx, "process_order_business_logic")

	time.Sleep(100 * time.Millisecond)

	processSpan.End()
}
