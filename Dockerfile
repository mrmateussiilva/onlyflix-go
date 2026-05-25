FROM golang:1.26-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /onlyflix .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates xdg-utils

WORKDIR /app
COPY --from=builder /onlyflix .
COPY templates/ ./templates/
COPY credential.json .

EXPOSE 8080

CMD ["./onlyflix"]
