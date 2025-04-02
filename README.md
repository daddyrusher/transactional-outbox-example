# Order Service with Transactional Outbox

This is a production-ready service written in Golang, also instrumented with OpenTelemetry and integrates with Grafana Tempo (for tracing), Prometheus (for metrics), and provides health-check endpoints.

## Health Checks

- **Health Check:**  
  `GET /healthz`  
  Returns an `OK` status if the service is healthy.

- **Readiness Check:**  
  `GET /ready`  
  Returns an `OK` status if the database is reachable.

## Monitoring URLs

- **Grafana**:  
  URL: `http://localhost:3000`  
  Default username/password: `admin/admin`  
  Dashboards are available for visualizing application performance and metrics.

- **Prometheus**:  
  URL: `http://localhost:9090`  
  Prometheus is used for collecting metrics exposed by the service. You can query and visualize these metrics from the Prometheus UI.

- **Grafana Tempo (Tracing)**:  
  URL: `http://localhost:4318`  
  Tempo receives tracing data (OTLP protocol) from the service for distributed tracing.
  
## Setup Tempo tracing visualization
- Connect to Grafana admin panel http://localhost:3000
- Add new data source Tempo - type url http://tempo:3200 into Connection block and check connection
- Send some queries to `/order`
- Choose Explore and pick Search query type, you'll see trace history down there

## Test Order Creation

To test the service, you can send a POST request to create an order.

Example:

```bash
curl -X POST http://localhost:8080/order \
  -H "Content-Type: application/json" \
  -d '{"status": "created"}'
