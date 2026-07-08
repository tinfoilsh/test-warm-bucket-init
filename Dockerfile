FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o init-proxy .

FROM alpine:3.23
RUN apk --no-cache add ca-certificates
COPY --from=build /src/init-proxy /app/init-proxy
ENTRYPOINT ["/app/init-proxy"]
