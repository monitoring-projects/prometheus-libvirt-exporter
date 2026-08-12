FROM alpine:3.21
ARG TARGETPLATFORM

COPY --chmod=755 $TARGETPLATFORM/prometheus-libvirt-exporter /usr/bin/prometheus-libvirt-exporter
EXPOSE 9177
ENTRYPOINT ["/usr/bin/prometheus-libvirt-exporter"]