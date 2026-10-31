# syntax=docker/dockerfile:1
# The gdelt-lake image: a static-ish Go binary (DuckDB linked in through cgo) on a minimal runtime image.
#   docker build -t gdelt-lake .                 the runtime image
#   docker build --target test -t gdelt-lake-test .   adds lake-accept, for scripts/accept.sh

FROM golang:1.27.3-trixie@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/gdelt-lake ./cmd/gdelt-lake \
 && mkdir -p /out/lake /out/scratch

# Test tooling for scripts/accept.sh: a fake GDELT and a lake checker. Never shipped.
FROM build AS test
RUN CGO_ENABLED=1 go build -trimpath -o /out/lake-accept ./cmd/lake-accept
ENTRYPOINT ["/out/lake-accept"]

FROM gcr.io/distroless/cc-debian13:nonroot@sha256:3ee00291cf386ec52a552e6a858d555debd74776eb4bfa53fa3df2cd6677515e AS runtime
COPY --from=build /out/gdelt-lake /usr/local/bin/gdelt-lake
# Mount points owned by the non-root user, so empty named volumes take that ownership.
COPY --from=build --chown=65532:65532 /out/lake /lake
COPY --from=build --chown=65532:65532 /out/scratch /scratch
USER 65532:65532
ENV LAKE_STORAGE=local LAKE_LOCAL_ROOT=/lake LAKE_TEMP_DIR=/scratch
# A run-once job (or a loop with --every): there's no server to health-check.
ENTRYPOINT ["/usr/local/bin/gdelt-lake"]
CMD ["run"]
