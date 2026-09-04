# den-scout — a single static Go binary on distroless/static (no shell, no libc, non-root).
# All I/O is net/http, so nothing links to the C library: CGO_ENABLED=0 gives a fully static
# binary and the runtime image is just the ~2MB distroless base + the binary (~15MB total).

# Build on the native builder arch, cross-compile to the target (set by `docker build --platform`;
# the homelab publishes linux/amd64). CGO off → a fully static binary regardless of target.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Trim the binary (-s -w strips the symbol/DWARF tables).
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags='-s -w' -o /den-scout ./cmd/den-scout

# An empty directory to seed the cache volume with. It exists only to carry OWNERSHIP: Docker
# initializes a fresh named volume from the image path it is mounted over, so without this the volume
# is created root-owned, uid 65532 cannot write to it, and the disk tier disables itself exactly as it
# did before it had a volume at all. Distroless has no shell, so the mkdir must happen here.
RUN mkdir -p /cache

# :nonroot runs as uid 65532 and carries CA certs for the outbound HTTPS scrape/debrid calls.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /den-scout /den-scout
COPY --from=build --chown=65532:65532 /cache /cache
EXPOSE 8080

# Where the durable cache tier writes. The default (os.TempDir()) is unwritable under the documented
# `read_only: true` container, so persistence silently disabled itself and every redeploy re-paid a
# debrid resolve per probed release — the precise cost internal/scout/diskcache.go exists to avoid.
# Mount a named volume here to keep the store across image pushes. Under `read_only: true` the rootfs
# is not writable either, so a mount at this path is REQUIRED for persistence to work at all — a
# tmpfs survives a restart but not the image push, which is the case the tier was written for.
ENV SCOUT_CACHE_DIR=/cache

# Soft heap ceiling under the documented 256 MiB container limit — Go's GC isn't cgroup-memory-aware,
# so without this RSS can ~2× before a GC and the container gets OOM-killed. Override to match the
# actual mem_limit when running with more/less headroom (the Go runtime reads GOMEMLIMIT natively).
ENV GOMEMLIMIT=230MiB

# Distroless has no shell, so the probe execs the binary itself (audit #2 — no second runtime).
HEALTHCHECK --interval=60s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/den-scout", "-healthcheck"]

ENTRYPOINT ["/den-scout"]
