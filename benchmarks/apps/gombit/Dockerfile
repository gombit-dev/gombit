# Container image for the gombit benchmark app — the real Gombit-runtime
# implementation of the canonical /api/projects CRUD API.
#
# Build context is the REPOSITORY ROOT (the Go module root):
#
#   docker build -f benchmarks/apps/gombit/Dockerfile -t bench-gombit:local .
#
# Unlike gin-gorm (AutoMigrate), this app uses Atlas versioned migrations, the
# same way a deployed Gombit app would. Migrations are NOT applied on serve;
# the entrypoint has three explicit verbs and `serve` (the default CMD) only
# runs the API. Bring-up order is: `run migrate` (ensures the database and
# applies migrations), `run seed`, then `serve`. So the image carries three
# binaries — the app, the `gombit` CLI (applies migrations), and the pinned
# `atlas` binary — plus the migrations directory and a psql client (the migrate
# verb creates the target database if absent).
#
# ATLAS_IMAGE is the *-alpine* Atlas image on purpose: its /atlas is the
# musl-compatible build, so copying it onto the alpine runtime below is safe
# (the default debian-distroless tag can ship a dynamically linked glibc binary
# that would fail on alpine).
ARG ATLAS_IMAGE=arigaio/atlas:0.36.0-alpine

FROM golang:1.26.0-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The app under test and the gombit CLI that applies its migrations.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/gombit-app ./benchmarks/apps/gombit && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/gombit ./cmd/gombit

FROM ${ATLAS_IMAGE} AS atlas

FROM alpine:3.20
# postgresql-client: the migrate verb ensures the target database exists.
RUN apk add --no-cache postgresql-client && adduser -D -u 10001 app
COPY --from=build /out/gombit-app /usr/local/bin/gombit-app
COPY --from=build /out/gombit /usr/local/bin/gombit
COPY --from=atlas --chmod=0755 /atlas /usr/local/bin/atlas
# `gombit db migrate` defaults --dir to database/migrations relative to the
# working directory, so ship the migrations under /app and run from there.
WORKDIR /app
COPY benchmarks/apps/gombit/database ./database
COPY --chmod=0755 benchmarks/apps/gombit/docker-entrypoint.sh /usr/local/bin/entrypoint.sh
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["serve"]
