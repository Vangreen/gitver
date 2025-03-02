FROM golang:1.24-alpine AS builder
WORKDIR /app

COPY . .
RUN go mod download

RUN go build -o gitver .

FROM alpine AS runner
WORKDIR /app
COPY --from=builder /app/gitver /app/gitver
COPY templates/ templates/
COPY static/ static/
ENTRYPOINT ["/app/gitver"]