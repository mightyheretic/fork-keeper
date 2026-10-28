# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod ./
RUN go mod download

COPY . .
# Static binary: SQLite comes from a pure-Go driver (modernc.org/sqlite), so CGO stays off.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/forkkeeper ./cmd/forkkeeper

# ---- runtime ----
# Distroless static: no shell or package manager, and runs as uid 65532 by default.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/forkkeeper /usr/local/bin/forkkeeper

# State (SQLite DB, git working copies) lives on a volume; config is mounted read-only.
VOLUME ["/var/lib/forkkeeper"]
ENV FORKKEEPER_DATA_DIR=/var/lib/forkkeeper \
    FORKKEEPER_CONFIG=/etc/forkkeeper/config.yaml \
    CREDENTIALS_DIRECTORY=/run/secrets

# 8080: public webhook receiver. 8081: private API (never exposed through the proxy).
EXPOSE 8080 8081

USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/forkkeeper", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/forkkeeper"]
CMD ["serve"]
