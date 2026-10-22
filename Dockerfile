# Base images can be overridden (CI uses a mirror because Docker Hub throttles anonymous pulls on shared runners).
ARG GO_IMAGE=golang:1.27-alpine
ARG RUN_IMAGE=alpine:3.20
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pve-vm-handoff ./cmd/pve-vm-handoff

FROM ${RUN_IMAGE}
LABEL org.opencontainers.image.source="https://github.com/yniverz/pve-vm-handoff" \
      org.opencontainers.image.description="Hand shared hardware (like a passthrough GPU) between Proxmox VMs from a small web UI" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 handoff && mkdir /data /config && chown handoff /data
COPY --from=build /out/pve-vm-handoff /usr/local/bin/pve-vm-handoff
USER handoff
ENV CONFIG_FILE=/config/config.yaml
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=4s --start-period=10s CMD ["pve-vm-handoff", "healthcheck"]
ENTRYPOINT ["pve-vm-handoff"]
