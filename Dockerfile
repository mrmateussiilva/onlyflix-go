FROM golang:1.26-alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY main.go transcoder.go users.go users_test.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /onlyflix .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates ffmpeg

WORKDIR /app
COPY --from=builder /onlyflix .
COPY templates/ ./templates/

EXPOSE 8080

CMD ["./onlyflix"]
