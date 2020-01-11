ARG ARCH="amd64"
ARG OS="linux"
FROM quay.io/prometheus/busybox-${OS}-${ARCH}:latest
LABEL maintainer="The KubeVault Authors <kubevault-dev@appscode.com>"

ARG ARCH="amd64"
ARG OS="linux"
COPY .build/${OS}-${ARCH}/vault_exporter /bin/vault_exporter

USER        nobody
EXPOSE      9102 9125 9125/udp
HEALTHCHECK CMD wget --spider -S "http://localhost:9102/metrics" -T 60 2>&1 || exit 1
ENTRYPOINT  [ "/bin/vault_exporter" ]
