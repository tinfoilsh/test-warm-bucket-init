# Build the init proxy
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -o /sidecar-init .

# Layer on top of the published sidecar image
FROM ghcr.io/tinfoilsh/tinfoil-buckets-sidecar@sha256:03d43dd687a5ed352f3d4956db6af706ec9dee8ca2c0fbce651ee59317b2ede5
COPY --from=build /sidecar-init /app/sidecar-init
ENTRYPOINT ["/app/sidecar-init"]
