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

```
gdelt-lake run [--every 15m] [--lookback 7d]
```

| Variable | Meaning | Default |
|----------|---------|---------|
| `LAKE_STORAGE` | Storage backend: `local` (Azure Blob Storage comes next) | `local` |
| `LAKE_LOCAL_ROOT` | The lake's folder, for `local` | required |
| `LAKE_DUCKDB_MEMORY` | DuckDB's memory cap | `1GB` |
| `LAKE_GDELT_BASE_URL` | GDELT's file server | `http://data.gdeltproject.org/gdeltv2/` |
| `LAKE_TEMP_DIR` | Scratch space for downloads | the system temp dir |

## Development

Go (current release) and a C compiler (DuckDB is linked in through cgo).

```
make test    # unit and integration tests, no network needed
make lint    # go vet, staticcheck, exhaustive, gofmt
make build   # bin/gdelt-lake
```

## Data and attribution

The code is MIT licensed (see [LICENSE](LICENSE)). The data comes from the GDELT Project, which makes it freely
available with attribution: if you use data collected with this tool, cite [the GDELT Project](https://www.gdeltproject.org).
This repository doesn't contain or redistribute GDELT data, apart from a few sample rows used as test fixtures.
