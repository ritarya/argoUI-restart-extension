FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY src/ ./src/
RUN CGO_ENABLED=0 GOOS=linux go build -o /webhook ./src/

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /webhook /webhook
EXPOSE 8443
ENTRYPOINT ["/webhook"]
