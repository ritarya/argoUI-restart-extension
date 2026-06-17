FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY src/ ./src/
RUN CGO_ENABLED=0 GOOS=linux go build -o /rfc-validator ./src/

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /rfc-validator /rfc-validator
EXPOSE 8080
ENTRYPOINT ["/rfc-validator"]
