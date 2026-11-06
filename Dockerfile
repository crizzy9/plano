FROM docker.io/library/golang:1.26.10-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=1 GOMAXPROCS=2 go build -p 1 -trimpath -o /out/plano ./cmd/plano \
    && CGO_ENABLED=0 GOMAXPROCS=2 go build -p 1 -trimpath -o /out/plano-train ./cmd/plano-train

FROM docker.io/library/debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 plano \
    && useradd --uid 10001 --gid plano --no-create-home --shell /usr/sbin/nologin plano \
    && install -d -m 0750 -o plano -g plano /data \
    && install -m 0640 -o plano -g plano /dev/null /data/.plano-volume

COPY --from=build /out/plano /out/plano-train /usr/local/bin/
COPY --from=build /src/internal/itinerary/passport_airport_zones.LICENSE /usr/share/doc/plano/
ENV PLANO_ADDR=:8080 PLANO_DB=/data/plano.db
WORKDIR /data
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/plano", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/plano"]
