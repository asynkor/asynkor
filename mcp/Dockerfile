FROM golang:1.23-alpine AS builder

WORKDIR /app

ENV GOFLAGS="-mod=mod"
ENV CGO_ENABLED=0
ENV GOOS=linux

COPY go.mod ./
COPY . .

RUN go mod tidy && go build -ldflags="-s -w" -o asynkor-mcp .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
RUN addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=builder /app/asynkor-mcp .
USER app
EXPOSE 4000
CMD ["./asynkor-mcp"]
