# P1 plan: ingester with local storage

Status: **draft for review**. Implements phase P1 of [design.md](design.md): the ingester (batches and compaction),
local and in-memory storage, tests, a container image and the local recipe. Azure (P2), the query service (P3) and AWS
(P4) are out of scope.

## Done when

- 48 hours unattended against a local volume, across at least one UTC day boundary.
- `_state` accounts for every batch GDELT listed in that time: done, failed (and retried) or missing.
- The compacted day matches the sum of its batches (row counts per feed and stream).
- The README's DuckDB example query runs against the result.
- The container image builds and passes its smoke test (`scripts/accept.sh`, see step 8).

## Layout

```
cmd/gdelt-lake/          main: flags, config from env, wiring
internal/source/         GDELT file lists, downloads, size and MD5 checks
internal/convert/        DuckDB: CSV -> Parquet per feed, compaction; sql/*.sql embedded with go:embed
internal/storage/        the interface (design 7), local and in-memory adapters, one shared contract test
internal/state/          per-day state files
internal/ingest/         a run: which batches, retries, gaps, compaction, exit code
testdata/                small sample batches cut from real GDELT files
deploy/local/            compose.yaml for the local recipe
scripts/accept.sh        builds the image and runs its smoke test on any Docker host
```

Module path `github.com/j-c/gdelt-lake`. One binary, `gdelt-lake`.

## Command line

```
gdelt-lake run [--every 15m] [--lookback 7d]
```

`run` processes new batches, compacts finished days, and exits: 0 if everything is accounted for, 1 if anything
failed. With `--every`, it repeats on that interval until stopped (SIGTERM ends it cleanly between batches).
Everything else comes from environment variables: `LAKE_STORAGE=local`, `LAKE_LOCAL_ROOT=/lake`,
`LAKE_DUCKDB_MEMORY=1GB`, `LAKE_GDELT_BASE_URL` (defaults to GDELT's server; tests point it at a fake one).

## Files in the lake

A 15-minute batch writes one file per feed per stream: `gkg/date=2026-10-31/20261031T001500-en.parquet` and
`…-tr.parquet`. Compaction merges a day into `gkg/date=2026-10-31/day.parquet` (both streams, with the `translated`
column).

## Steps

Each step ends with its tests passing and is a commit of its own. Step 1 becomes the repo's single `initial commit`,
replacing the history so far.

| # | Step | Done when |
|---|------|-----------|
| 1 | **Skeleton.** Go module, Go `.gitignore`, `cmd/gdelt-lake` that parses flags and env and exits, Makefile targets (`test`, `lint`), the README's outline. | `go vet`, staticcheck and the `exhaustive` linter are clean; `go test ./...` runs. |
| 2 | **Storage.** The interface; local adapter (temp file then rename); in-memory adapter; one contract test suite run against both. | Contract tests pass for both, including: a reader never sees a partial file; list by prefix; delete. |
| 3 | **Source.** Parse `lastupdate` and master lists (both streams); download one file at a time with backoff; check size and MD5; unzip. | Tests against a fake GDELT server (`httptest`) cover: normal batch, 404, wrong MD5, truncated zip, server errors then success. |
| 4 | **Convert.** One `.sql` file per feed: typed columns, UTC timestamps, `translated`; GKG's parsed fields (themes, locations, persons, organisations, tone, page title, translation info) as lists/structs, the rest as text, GCAM dropped. Bad rows counted, not fatal. | Tests on `testdata/` samples check column types, parsed values against hand-checked rows, and bad-row counts on deliberately broken rows. |
| 5 | **State and runs.** Per-day `_state` files; pick the batches to do within the lookback; record failures and retry them next run; mark missing after the lookback; exit codes. | An end-to-end test against the fake server: a first run, a rerun that does nothing, recovery after a failed batch, a gap recorded as missing. |
| 6 | **Compaction.** When a day has ended (plus 2 h grace) and every batch is accounted for: write `day.parquet` per feed, record it, delete the batch files. DuckDB memory capped by `LAKE_DUCKDB_MEMORY`. | Tests: row counts match; a crash between write and delete leaves the day readable and is finished on the next run. Measured on a full real day under the 1 GB cap. |
| 7 | **Container and local recipe.** Multi-stage Dockerfile (build with the C compiler, minimal runtime image with the C library, non-root user, read-only root filesystem except the lake and temp dirs); `deploy/local/compose.yaml` with `--every 15m` and a lake volume; `scripts/accept.sh` (build, run one cycle against the fake server, check the lake, tear down). | Static checks clean (hadolint, shellcheck, `docker compose config`); `accept.sh` passes on a Docker host. |
| 8 | **Soak.** 48 hours against real GDELT on a development host. README completed with the DuckDB example. | The criteria at the top of this page. |

## Tests

- Unit and integration tests run with `go test ./...`, with no network: GDELT is a fake `httptest` server serving
  `testdata/` zips.
- `testdata/` holds a few dozen real rows per feed and stream, cut from published GDELT files (free to use with
  attribution, credited in the README), plus deliberately broken copies.
- DuckDB runs for real in the tests, not mocked: the SQL is the part most likely to be wrong.
- The container is tested by `scripts/accept.sh` on a Docker host.

## Questions before starting

1. **CI on GitHub Actions**: run lint, tests and the image build on every push and pull request, with read-only
   permissions and no secrets?
2. **Test data**: commit small samples of real GDELT rows in `testdata/`?
3. **Soak and container check**: run the 48-hour soak as the plain binary on the development host, and prove the
   container separately with `accept.sh` on a Docker host?
