# Sidecar gateway: sing-box tun mode transparent proxy.
# 必须用 glibc 基础镜像——sing-box 官方二进制链接了 glibc。
FROM debian:bookworm-slim

ENV PATH="/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin"

RUN apt-get update && apt-get install -y --no-install-recommends \
        iproute2 ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

ARG SINGBOX_VERSION=1.13.11
RUN set -eux; \
	ARCH="$(dpkg --print-architecture)"; \
	curl -fsSL -o /tmp/sb.tgz "https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/sing-box-${SINGBOX_VERSION}-linux-${ARCH}.tar.gz"; \
	tar -xzf /tmp/sb.tgz -C /tmp; \
	install -m 0755 "/tmp/sing-box-${SINGBOX_VERSION}-linux-${ARCH}/sing-box" /usr/local/bin/sing-box; \
	rm -rf /tmp/sb.tgz "/tmp/sing-box-${SINGBOX_VERSION}-linux-${ARCH}"; \
	sing-box version

COPY deploy/docker/sing-box-gateway/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
