# syntax=docker/dockerfile:1

# ──────────────────────────────────────────────────────────────────────────────
# Stage 1: Builder
# ──────────────────────────────────────────────────────────────────────────────
# Go 1.27.1 Alpine builder — matches go.mod. CGO_ENABLED=0 for a fully static
# binary (no libc, no musl, no runtime CGO dependency).
FROM golang:1.27.1-alpine3.23@sha256:0908ac9b9319e09d7c238aabe914e0395c51d63c4e3d0ae8c554fda9158a5769 AS builder

WORKDIR /src

# Layer-cache the module download: go.mod/go.sum change far less often than
# source, so the download step is cached across builds.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build the static binary.
COPY . .

RUN CGO_ENABLED=0 go build -o /out/vector-service ./cmd/vector-service

# ──────────────────────────────────────────────────────────────────────────────
# Stage 2: Runtime
# ──────────────────────────────────────────────────────────────────────────────
# Distroless static (Debian 12, nonroot). Pinned by digest for reproducibility.
# The image contains only the compiled binary — no shell, no toolchain, no
# source, no migrations, no test artifacts, no secrets.
#
# TLS client certificates for PostgreSQL are mounted at runtime (files or a
# projected volume), never baked into the image.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=builder /out/vector-service /usr/local/bin/vector-service

# The distroless nonroot image provides a "nonroot" user (UID 65532).
USER nonroot

ENTRYPOINT ["/usr/local/bin/vector-service"]
CMD ["serve"]
