# gdelt-lake

Keeps a copy of [GDELT 2.0](https://www.gdeltproject.org)'s raw 15-minute data feeds (Events, Mentions and the Global
Knowledge Graph, in English and translingual streams) as Parquet files in storage you control. Query it with DuckDB,
Spark, Polars or anything else that reads Parquet, without GDELT's API rate limits or its 3-month search window.

**Status: in development.** Nothing here is ready to use yet. The design is in [docs/design.md](docs/design.md) and
the current phase's plan in [docs/p1-plan.md](docs/p1-plan.md).

## How it works

An ingester runs on a schedule (every 15 minutes, matching GDELT). Each run downloads any batches not yet in the lake,
checks them, converts them to Parquet and writes them to storage. Once a UTC day is complete, it compacts that day into
one file per feed. Progress is kept in the lake itself, so a failed run is fixed by running again.

```
<lake>/events/date=YYYY-MM-DD/…parquet
<lake>/mentions/date=YYYY-MM-DD/…parquet
<lake>/gkg/date=YYYY-MM-DD/…parquet
<lake>/_state/YYYY-MM-DD.json
```

## Running it

With Docker, using the local recipe (a named volume holds the lake; runs every 15 minutes):

```
docker compose -f deploy/local/compose.yaml up -d --build
```

On Azure, [deploy/azure](deploy/azure/README.md) runs it as a scheduled Container Apps job writing to a private storage
account (Bicep).

Or the binary directly:

```
gdelt-lake run [--every 15m] [--lookback 7d]
```

`run` processes new batches, compacts finished days and exits 0 when it did its job, 1 when it failed and needs
attention (DuckDB, storage or disk errors) and 2 for bad configuration. Problems with individual GDELT files (not
published yet, corrupt, gaps) don't change the exit code: they're retried on later runs and reported by `status`.
With `--every` it repeats until stopped; SIGTERM stops it between files. A new lake starts with the current batch.
`--lookback` is how far back a run looks for batches it missed, and for gaps GDELT may still fill.

With `LAKE_BACKFILL` set, each run also fetches history once the current batches are in: newest day first, a whole
day at a time (each compacted when done), for at most `LAKE_BACKFILL_BUDGET` per run. A day of history takes about
10 minutes to fetch and about 190 MB to store (weekdays more, weekends less), so with the defaults and a 15-minute schedule the lake gains roughly a
day of history per run. Raising `LAKE_BACKFILL` later extends the lake further back; lowering it deletes nothing.

```
gdelt-lake status
```

prints the lake's health (from `_state/health.json`, written by every run) and exits 0 when healthy, 3 for data
problems (gaps, a stream more than an hour behind, a file failing for more than an hour) and 1 when the last run
failed or the health file can't be read.

| Variable | Meaning | Default |
|----------|---------|---------|
| `LAKE_STORAGE` | Storage backend: `local` or `azure` (Azure Blob Storage) | `local` |
| `LAKE_LOCAL_ROOT` | The lake's folder, for `local` | required for `local` |
| `LAKE_AZURE_ACCOUNT_URL` | For `azure`: the account's blob endpoint, e.g. `https://<account>.blob.core.windows.net/`. Credentials come from the standard Azure chain: a managed identity in Azure (set `AZURE_CLIENT_ID` for a user-assigned one), or `az login` on a workstation. The identity needs *Storage Blob Data Contributor* on the container. | one of these two for `azure` |
| `LAKE_AZURE_CONNECTION_STRING` | For `azure`, instead of the account URL: a connection string (a SAS, or the Azurite emulator) | |
| `LAKE_AZURE_CONTAINER` | For `azure`: the container; created if missing and the identity may create it | `gdelt` |
| `LAKE_DUCKDB_MEMORY` | DuckDB's memory cap | `1GB` |
| `LAKE_GDELT_BASE_URL` | GDELT's file server | `https://data.gdeltproject.org/gdeltv2/` |
| `LAKE_TEMP_DIR` | Scratch space for downloads and compaction (a day's GKG needs about 1 GB) | the system temp dir |
| `LAKE_BACKFILL` | History to fetch: a duration (`90d`) or a date (`2025-01-01`); GDELT 2.0 starts 2015-02-18 | none |
| `LAKE_BACKFILL_BUDGET` | Most time a run spends on backfill; `0` pauses it | `10m` |

## Querying it

Any Parquet reader works. With DuckDB, from the lake's folder:

```sql
SELECT date, translation.source_language AS language, page_title, document_identifier
FROM read_parquet('gkg/*/*.parquet', hive_partitioning = true, union_by_name = true)
WHERE date >= '2026-10-31'
  AND list_contains(list_transform(themes, lambda t: t.theme), 'ARMEDCONFLICT')
LIMIT 10;
```

`union_by_name = true` lets one query read files written with older and newer schemas. While a day is being compacted
there's a moment, a few seconds, when both its batch files and its day file exist; a query that must never count a row
twice can deduplicate on `gkg_record_id` (GKG), `global_event_id` (Events) or `global_event_id` plus
`mention_identifier` (Mentions).

## Development

Go (current release) and a C compiler (DuckDB is linked in through cgo).

```
make test    # unit and integration tests, no network needed
make lint    # go vet, staticcheck, exhaustive, gofmt
make build   # bin/gdelt-lake
```

CI runs the same lint and tests on every push and pull request. Its test report is on each run's summary page: results and coverage per package, then
every test and subtest with its result and time (with the message of each failure or skip), failures with their full
output, and the slowest tests. Failed tests are also marked in the diff;
the raw results (`test.json`, `coverage.out`) are kept for 30 days as the run's `test-results` artifact. To see the
report locally: `go test -json -cover ./... | go run ./scripts/testreport`.

`scripts/accept.sh` checks the container image on any Docker host: it builds the image, runs it once against a fake
GDELT server, checks the lake it wrote and that the image carries its licences, and cleans up, ending with `PASS` or
`FAIL`.

## Data and attribution

The code is MIT licensed (see [LICENSE](LICENSE)). The container image also carries the licences and notices of the
libraries compiled into it, in `/usr/share/licenses/gdelt-lake`; CI checks they all allow the image to be published. The data comes from the GDELT Project, which makes it freely
available with attribution: if you use data collected with this tool, cite [the GDELT Project](https://www.gdeltproject.org).
This repository doesn't contain or redistribute GDELT data, apart from a few sample rows used as test fixtures.
