FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go github.go ./
COPY views ./views
COPY static ./static
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /gitver .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /gitver /gitver
VOLUME ["/data"]
EXPOSE 8090
ENV DATA_DIR=/data
ENTRYPOINT ["/gitver"]
