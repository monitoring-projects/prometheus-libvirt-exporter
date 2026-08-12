FROM golang:1.26 as builder
WORKDIR /app

# Requirements/Dependencies
COPY go.mod go.sum ./
RUN go mod download

# Build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o prometheus-libvirt-exporter

FROM alpine:3.21

# Default listen on port 9177
EXPOSE 9177

COPY --from=builder /app/prometheus-libvirt-exporter /usr/bin/prometheus-libvirt-exporter

ENTRYPOINT ["/usr/bin/prometheus-libvirt-exporter"]
