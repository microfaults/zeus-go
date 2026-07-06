FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /zeus ./cmd/zeus

# grafana/k6 ships a static binary; copy it rather than building from source.
FROM grafana/k6:0.49.0 AS k6

FROM alpine:3.21
RUN apk --no-cache add ca-certificates
COPY --from=builder /zeus /zeus
COPY --from=k6 /usr/bin/k6 /usr/bin/k6
# The k6 engine assets (runner.js, scripts/, personas/, flows/). The launcher
# stages each run's workflow document under k6/flows/zeus-runs/, so this must
# be a writable copy inside the image, not a read-only mount.
COPY k6 /k6
ENV ZEUS_K6_DIR=/k6
EXPOSE 8080
ENTRYPOINT ["/zeus"]
