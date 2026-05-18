# syntax=docker/dockerfile:1.7

# --- Build stage ----------------------------------------------------------
FROM golang:1.25-bookworm AS builder

WORKDIR /build

# Module graph first so the dependency-download layer caches independently of
# source edits. go.sum (once third-party deps exist) joins via the full copy
# below; with no deps yet this download is a no-op.
COPY go.mod ./
RUN go mod download

COPY . .

# Static, stripped binary. CGO off so the result runs on distroless/static
# (no libc, no shell).
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wavefront ./cmd/wavefront

# --- Runtime stage --------------------------------------------------------
# distroless/static: no shell, no package manager, nonroot user. Sufficient
# because the Go binary is fully static (CGO disabled).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/wavefront /wavefront

EXPOSE 8080 9090
ENTRYPOINT ["/wavefront"]
