FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go mod tidy && go build -o router .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/router .
EXPOSE 5000

ENTRYPOINT ["./router"]
