FROM golang:1.24-alpine AS builder
WORKDIR /app

COPY . .
RUN go mod download

RUN go build -o gitver .

FROM alpine AS runner
COPY --from=builder /app/gitver /app/gitver
ENTRYPOINT ["/app/gitver"]