# Container image for the gin-gorm benchmark control app.
#
# Build context is the REPOSITORY ROOT (the Go module root), because the binary
# imports benchmarks/apps/shared and builds from the module:
#
#   docker build -f benchmarks/apps/gin-gorm/Dockerfile -t bench-gin-gorm:local .
#
# benchmarks/compose.yml builds it with `context: ..` for exactly this reason.
# A Dockerfile-scoped .dockerignore next to this file keeps node_modules/vendor/
# .git out of the context.
FROM golang:1.26.0-alpine AS build
WORKDIR /src
# Cache modules before copying the tree so source edits don't re-download.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/gin-gorm ./benchmarks/apps/gin-gorm

FROM alpine:3.20
# Non-root, matching the least-privilege posture of the other benchmark apps.
RUN adduser -D -u 10001 app
COPY --from=build /out/gin-gorm /usr/local/bin/gin-gorm
COPY benchmarks/apps/gin-gorm/docker-entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
USER app
EXPOSE 8081
# `entrypoint.sh serve` (default) runs the API; `entrypoint.sh seed` runs the
# one-shot deterministic seed and exits.
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["serve"]
