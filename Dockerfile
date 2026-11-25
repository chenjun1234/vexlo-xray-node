# Personal VLESS-over-WebSocket image.
# Xray-core v26.11.9 from https://github.com/XTLS/Xray-core/releases/tag/v26.11.9
# SHA-256 values are the SHA2-256 lines from that tag's upstream .dgst files.

FROM golang:1.25.4-alpine@sha256:d3f0cf7723f3429e3f9ed846243970b20a2de7bae6a5b66fc5914e228d831bbb AS build

WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/entrypoint .

FROM alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f

ARG XRAY_VERSION=26.11.9
ARG TARGETARCH

RUN apk add --no-cache ca-certificates curl unzip \
    && arch="$TARGETARCH" \
    && if [ -z "$arch" ]; then \
         case "$(uname -m)" in \
           x86_64) arch=amd64 ;; \
           aarch64|arm64) arch=arm64 ;; \
           *) echo "unsupported architecture" >&2; exit 1 ;; \
         esac; \
       fi \
    && case "$arch" in \
         amd64) asset=Xray-linux-64.zip; sum=b651f7e1d9966ee2c1b1ee1bf8b52c72771824d3065dce96bbc8bcc19ed95f82 ;; \
         arm64) asset=Xray-linux-arm64-v8a.zip; sum=64ed6858982a7f5eb85d88b811089063bd4f17561f4c8f44e0f1dc8e6f6430be ;; \
         *) echo "unsupported architecture" >&2; exit 1 ;; \
       esac \
    && curl -fsSL -o /tmp/xray.zip "https://github.com/XTLS/Xray-core/releases/download/v${XRAY_VERSION}/${asset}" \
    && echo "${sum}  /tmp/xray.zip" | sha256sum -c - \
    && unzip -p /tmp/xray.zip xray > /usr/local/bin/xray \
    && chmod 755 /usr/local/bin/xray \
    && rm -f /tmp/xray.zip \
    && apk del curl unzip \
    && adduser -D -H -u 65532 -s /sbin/nologin xray

COPY --from=build /out/entrypoint /usr/local/bin/entrypoint
RUN chmod 755 /usr/local/bin/entrypoint

USER 65532:65532
ENV PORT=8080
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/entrypoint", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/entrypoint"]
