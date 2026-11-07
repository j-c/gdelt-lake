# gdelt-lake: design

Status: **design agreed for v1; nothing built yet.** Decisions for later phases (auth, API and MCP design, AWS) are made at
the start of those phases.

## 1. Purpose

gdelt-lake keeps a copy of GDELT 2.0's raw 15-minute data feeds (Events, Mentions and the Global Knowledge Graph) as
Parquet files in storage you control: a local folder, or a cloud object store. With that copy:

- queries don't depend on GDELT's API, its rate limit (one request per 5 seconds) or its availability;
- history builds up beyond the DOC API's rolling 3-month window;
- any tool that reads Parquet (DuckDB, Spark, Polars, pandas, Databricks and others) can use the data directly.

## 2. Requirements

| ID | Requirement | Source |
|----|-------------|--------|
| R1 | Storage is pluggable: a local volume for development, Azure Blob Storage when deployed. Another backend (e.g. S3) is one new adapter and no other change. | owner |
| R2 | Written in Go (originally C#; changed by D0). The language's DuckDB client must be in DuckDB's primary support tier. | owner |
| R3 | Every endpoint a system, user or MCP client uses is authenticated. The mechanism is not chosen yet (D6). | owner |
| R4 | Collects from when the lake starts, plus history back to a configurable point (backfill, 8.5). Each new 15-minute batch should be in the lake within about 30 minutes, backfill or not. | owner |
| R5 | Feeds: Events, Mentions and GKG, in both the English and translingual streams. GKG is stored without GCAM (see 4.2). | owner |
| R6 | Containers preferred. Other deployment options are open for the ingester. | owner |
| R7 | Only as complex as needed. Every component, dependency and abstraction has to justify itself. Code volume is fine if it's readable. | owner |
| R8 | Open source (MIT). No private details in the repo; deployment specifics come from configuration. | owner |
| R9 | Deployers choose how to run it, to suit their own goals. The reference deployment is the cheapest that works. | owner |
| R10 | Runs locally and on Azure first, and on AWS later. | owner |
| R11 | Idempotent and restartable: running again over a period already done changes nothing, and a failed run is fixed by running again. | derived |
| R12 | Gaps are visible: a batch GDELT never published, or that couldn't be read, is recorded, never silently skipped. | derived |
| R13 | Query access (storage reads now, the query service later) stays on private networks. Nothing is exposed to the internet unless a need forces it. | owner |

## 3. Non-goals

- **Article text.** GDELT doesn't publish it. The lake holds URLs, titles and what GDELT extracted from each article.
- **Re-implementing the DOC API's full-text search.** That runs on GDELT's own index, which isn't published.
- **Lags under 15 minutes.** GDELT itself publishes every 15 minutes.
- **History before GDELT 2.0** (18 February 2015): GDELT 1.0 uses different formats.
- **Transactional guarantees across files** (as Delta Lake or Iceberg provide). See 8.2 for the one place this shows up.
- **Redistributing GDELT data.** The repo contains code only.

## 4. Source data

### 4.1 Feeds

Every 15 minutes GDELT 2.0 publishes a batch of three zipped, tab-delimited files, named by the batch's UTC timestamp:

| Feed | File | One row per |
|------|------|-------------|
| Events | `YYYYMMDDHHMMSS.export.CSV.zip` | event (who did what to whom, where), when first seen |
| Mentions | `YYYYMMDDHHMMSS.mentions.CSV.zip` | article mentioning an event, including events first seen earlier |
| GKG | `YYYYMMDDHHMMSS.gkg.csv.zip` | article, with its themes, places, names, tone, quotes, amounts, dates and title |

There are two streams with the same three feeds:

- **English**: `masterfilelist.txt` (every batch ever) and `lastupdate.txt` (the latest batch).
- **Translingual**: `masterfilelist-translation.txt` and `lastupdate-translation.txt`, with `.translation.` in the
  file names. This is GDELT's machine-translated coverage of about 65 other languages.

Each list line gives the file's size, MD5 hash and URL. The master lists are large (about 130 MB each and growing), so
the ingester only reads their end, with an HTTP range request sized to the lookback window. These are static files on
GDELT's file server (served over HTTPS), not the rate-limited API. The ingester should still be polite: one download at a time, with backoff on errors.

### 4.2 GCAM

GCAM (Global Content Analysis Measures) is a GKG field with each article's scores on about 2,300 emotion and content
dimensions, drawn from roughly 24 dictionaries. It's stored as one long string per row and is probably the largest part
of GKG. It's out of scope (R5). If a few dimensions turn out to be useful later, they can be added as their own columns.

### 4.3 Known quirks (to confirm in phase 0)

- Batches are occasionally missing from the lists, or a zip is truncated or corrupt.
- Some rows contain bytes that aren't valid UTF-8, and GKG's last field (Extras) can contain unescaped line breaks, so
  one record spans several lines (see 4.4).
- Many GKG fields are delimited lists inside one column (`;`, `#`, `,` and `:` as separators).
- An Events row is written once, when the event is first seen. Later coverage of it appears only in Mentions.

### 4.4 Volumes (measured in phase 0)

Measured over one UTC day, 30 October 2026: all 96 batches of every feed in both streams, converted with DuckDB 2.0.0
and zstd compression. It's one day, so treat the figures as a guide, not a guarantee; volumes vary with the news. MB
here is 10^6 bytes.

| Stream / feed | Zipped | Batch Parquet (96 files) | Day file | Rows |
|---------------|-------:|-------------------------:|---------:|-----:|
| English Events | 7.3 MB | 9.8 MB | 5.8 MB | 114,106 |
| English Mentions | 10.2 MB | 6.4 MB | 5.8 MB | 294,813 |
| English GKG | 467.9 MB | 340.6 MB (with GCAM) | 78.8 MB (without GCAM) | 108,414 |
| Translingual Events | 5.4 MB | 7.7 MB | 4.1 MB | 66,323 |
| Translingual Mentions | 7.2 MB | 5.5 MB | 4.6 MB | 122,952 |
| Translingual GKG | 850.3 MB | 680.7 MB (with GCAM) | 150.7 MB (without GCAM) | 215,437 |

- **Total:** about 1.35 GB zipped a day, almost all of it GKG. The translingual GKG is nearly twice the size of the
  English one. Every batch was published; none were missing or corrupt.
- **GCAM** is 72% of the English GKG's compressed Parquet and 76% of the translingual one's (about 0.76 GB of the day's
  1.02 GB of GKG batch Parquet). Leaving it out (R5) makes the lake roughly a quarter of the size it would otherwise be.
- **Lake size:** the compacted day, with GKG without GCAM, is 250 MB, which extrapolates to about 91 GB a year. Batch
  files for the open day are larger until it's compacted, since small files compress less well.
- **Rows not read:** with DuckDB reading the files as published, 1 line of the English GKG and 594 lines of the
  translingual GKG (about 0.3%) were skipped. Two causes, both handled before DuckDB reads the file (P1, step 4):
  - Line breaks inside GKG's last field (Extras, typically in a `mailto:` link's body). GDELT doesn't escape them, so
    the rest of the field lands on lines of its own. Those fragments made up most of the skipped lines; the records
    themselves were read, with Extras cut short. The ingester joins such lines back onto their record.
  - Bytes that aren't valid UTF-8 (100 lines that day). The ingester replaces them with U+FFFD, so the record is kept.
  With both fixes, every record of the sample batches is read.
- **Compaction** took under a second per feed for Events and Mentions, and 14 s (English) and 10 s (translingual) for
  GKG. With DuckDB's memory limit set to 1 GB, the compacting process (the spike's, including its own runtime) peaked
  at 871 MB (English GKG) and 824 MB (translingual GKG) of resident memory, and 329 MB for the other feeds.
  Downloading and converting the whole day, one file at a time, took about 12 minutes.

## 5. Shape of the solution

```
GDELT file server ──HTTPS──> ingester ──> storage (local folder | Azure Blob | S3 later)
                             (every 15 min)  └─ Parquet files, partitioned by UTC date, plus per-day state

consumers ──read Parquet directly (storage credentials)──> storage
later (D6): query API + MCP server (authenticated) ──> storage
```

| Component | What it does |
|-----------|--------------|
| Ingester | Console app in a container. Reads GDELT's file lists, finds batches not yet in the lake, downloads and checks them, converts them to Parquet and writes them to storage. Once a day is complete, it compacts that day into one file per feed. Runs on a schedule (D2). |
| Lake | Parquet files and a small state file per day. It's the product: anything that reads Parquet can use it. |
| Query service | Later (D6). An HTTP API and MCP server over the lake. |

There's no database. The ingester works out its progress from what's already in storage (R11).

## 6. Lake layout

```
<root>/
  events/date=2026-10-31/20261031T0000.parquet      one file per 15-minute batch while the day is open
  events/date=2026-10-30/day.parquet                one file per feed once the day is compacted
  mentions/date=…/…
  gkg/date=…/…
  _state/2026-10-31.json        batches present, missing or failed, rejected-row counts, compacted or not
  _state/lake.json              when the lake started
```

- `date` is the batch's UTC date. All GDELT timestamps are UTC, and so are the lake's.
- Both streams go in the same tables, with a `translated` column (true for the translingual stream).
- Columns keep GDELT's names, in snake_case. Timestamps are stored as UTC timestamps, not strings. Values that don't
  parse as their column's type become NULL; the row is kept.
- GKG's parsed fields (A14) use GDELT's V2 versions, which hold the same values as the older V1 fields plus character
  offsets, so the V1 fields (themes, locations, persons, organisations, counts) aren't stored. The page title is taken
  out of Extras into its own column, as published (HTML entities such as `&#x10D;` are not decoded).
- How the GKG fields described in 4.3 are stored is D5.
- Readers use hive partitioning, for example in DuckDB:
  `select … from read_parquet('gkg/*/*.parquet', hive_partitioning = true, union_by_name = true) where date >= '2026-10-01'`.

### 6.1 Schema changes

Each day's `_state` records the schema version its files were written with. Columns are only ever added, never renamed
or removed within a major version, so readers that match columns by name (`union_by_name` in DuckDB, `mergeSchema` in
Spark) see a new column as empty (NULL) in older files.

Parsing another GKG field later:

1. Add the parsed column to the GKG SQL and bump the schema version. New batches have it from then on.
2. Older days still hold that field as GDELT's text, so a `reprocess` command rebuilds them from the lake's own files:
   read the day, derive the new column, write the day file again atomically, record the new version. Nothing is
   downloaded again.
3. Until a day is reprocessed, its new column is empty. A reader that needs it everywhere can wait for reprocessing to
   finish, or derive it from the text column in the query.

The fields that are parsed (A14) don't also keep their original text, which would store them twice. If a parsing bug
turns up, the affected days are re-ingested from GDELT, which keeps every batch since 2015: the same mechanism as
backfill.

## 7. Storage abstraction

One interface, as small as the ingester needs:

- list paths under a prefix
- check whether a path exists
- write a file **atomically**, so a reader never sees a half-written file
- open a file for reading
- delete a file (needed only for compaction)

Adapters: local filesystem (write to a temp file, then rename), Azure Blob Storage (a block blob upload becomes visible
only when committed), and in-memory for tests. S3 comes later (R10); its uploads are also atomic. Config chooses one
(`LAKE_STORAGE=local|azure`, plus that adapter's settings).

Parquet is written to a local temp file and then handed to storage, so storage never needs to know about Parquet and
any backend works the same way.

Azure credentials go through the standard SDK credential chain: managed identity when running in Azure, or a connection
string or SAS elsewhere. No custom auth code.

## 8. An ingest run

### 8.1 Batches

1. Read both streams' file lists: `lastupdate` normally, the end of the master list (a range request) when catching up.
2. Work out which batches in the lookback window (e.g. 7 days) aren't recorded as done in `_state`.
3. For each batch: download, check size and MD5, unzip, convert to Parquet, write to storage, update `_state`.
4. On failure: retry with backoff. A batch that still fails is recorded as failed and retried on the next run. A file
   still unavailable (unlisted, not published, or unreadable) 6 hours after its batch time is recorded as missing
   (R12), so a day can be compacted without waiting for it.
5. Exit codes keep run errors apart from data problems (A18):
   - **0**: the run did its job. Problems with individual files (not published yet, listed but not downloadable yet,
     corrupt, gaps) are soft: retried on later runs, logged at WARN and reported by `health.json` (8.3).
   - **1**: the run itself failed and needs attention: storage, DuckDB or other unrecoverable errors, logged at ERROR.
   - **2**: bad configuration or arguments.

   Download and unzip problems count as file problems; DuckDB, storage and disk errors stop the run with exit 1. (The
   first soak showed why: GDELT sometimes lists a file up to about 45 minutes before it can be downloaded, and with
   every failed file counted as a failed run, six of 53 runs "failed" though nothing was lost.)

After downtime, the next run catches up on its own within the lookback window. A new lake records its first batch in
`_state/lake.json`; history before that is fetched only as configured backfill (8.5).

### 8.2 Compaction

There's no separate job. When a run finds a UTC day that has ended (plus a grace period, e.g. 2 hours) and every batch
in it is accounted for (done or missing), it:

1. merges that day's batch files into one `day.parquet` per feed;
2. writes it and records the day as compacted in `_state`;
3. deletes the batch files.

Between steps 2 and 3, which takes seconds, a reader listing the folder sees both the batch files and the day file, so
the same rows appear twice. Each feed has a unique row ID (`global_event_id` for Events, `gkg_record_id` for GKG, event
ID plus URL for Mentions), so a reader that must never double-count can deduplicate. If this ever matters in practice,
a table format such as Delta Lake or Iceberg is the fix; that's not justified today.

Compaction keeps long-window queries fast: a 3-month query opens about 90 files per feed rather than about 8,600.

### 8.3 Gaps and reconciliation (A20)

- A file still unavailable 6 hours after its batch time is recorded as **missing**, so its day can be compacted
  without it. The day file then simply lacks that batch's rows; nothing else is affected.
- Missing files stay on the to-do list for the whole lookback window (7 days by default). Each run checks them
  against GDELT's list, which is cheap, and downloads only those GDELT now lists.
- When a missing file arrives late, it's added and its day is re-opened; the next run re-compacts the day, merging the
  existing day file with the late batch, and checks the row totals as usual. `health.json` records the gap as
  recovered.
- After the lookback window a gap is final. To reconcile older gaps, run once with a longer window, e.g.
  `gdelt-lake run --lookback 30d`: the same mechanism, no separate tool.

### 8.4 Health (A19)

Each run writes `_state/health.json`: when the run finished and its outcome (including any run error), the newest batch
in the lake and how far behind GDELT that is, files being retried and for how long, and gaps per day (open, recovered,
final). `gdelt-lake status` prints it and exits:

- **0**: healthy;
- **3**: data problems: a gap in the window, a stream more than an hour behind, or a file failing for more than an
  hour;
- **1**: needs attention: the last run failed (a system error), or the health file can't be read.

`health.json` covers the lookback window; gaps older than that stay recorded in the per-day state files.

So a scheduler or cloud alert can watch run failures (exit 1 from `run`) and data health (exit 3 from `status`)
separately. Nothing is served over the network, so R3 doesn't come into it.

### 8.5 Backfill (A21)

`LAKE_BACKFILL` sets how much history the lake should hold: a duration (`90d`) or a date (`2025-01-01`). Unset means
none: the lake starts with the current batch. On startup, and on every run after, the ingester fetches whatever of
that history is missing, until it's all there. Raising the value later extends the lake further back; lowering it
doesn't delete anything.

- **Live data first.** Each run brings the lookback window up to date before touching history, so new batches still
  arrive within about 30 minutes.
- **Then history, newest first, a whole day at a time.** Recent history is usually the most useful, and finishing one
  day before starting the next means each day is compacted and usable as soon as it's done. A backfilled day goes
  through the same batch files, state and compaction as a live one; old files GDELT never published are recorded as
  missing straight away, since they're long past the 6-hour wait. Backfilled days that fall inside the lookback window are then treated
  like live ones: their gaps are re-checked, and a day that ended less than two hours ago is compacted by a later run.
- **A time budget per run** (`LAKE_BACKFILL_BUDGET`, default 10 minutes; `0` pauses backfill). When it's used up, the
  run stops at the next file boundary and the following run carries on. With 15-minute runs that leaves headroom, so
  runs never overlap, and on a scheduled job the cost per run stays bounded.
- **Throttling.** Downloads stay one at a time, with a short pause between backfill files (250 ms). GDELT's file
  server is static storage behind a CDN rather than the rate-limited API, so this is politeness rather than a known
  limit. At phase 0's speed a history day takes roughly 12 minutes to fetch and convert, so 90 days takes about
  1.5 days of running; that depends mostly on bandwidth.
- **Reading old parts of the file list.** The master list is sorted by time, so the ingester finds a day's lines with a
  few small range requests (a binary search on byte offsets) instead of downloading the whole 130 MB list.
- **Size:** at phase 0's measurement, about 250 MB per day of history (about 91 GB a year).
- `health.json` (8.4) reports backfill progress: target, oldest complete day, days remaining.

Older GDELT files could differ from today's, so samples were checked with phase 0's "every record read" test when
backfill was built: six hours of 19 February 2015 (GDELT 2.0's first full day) and three hours of 15 June 2020, both
streams, all three feeds. Every record was read; the same clean-up as today's files applied (a few invalid UTF-8 and
continuation lines in translingual GKG). Fetching ran at about one file a second, so about 10 minutes per history
day.

A history file GDELT lists but can't serve is retried on the next runs and recorded as missing after three failed
attempts, so one bad file can't hold backfill up for long. A file GDELT never listed is missing straight away.

## 9. Decisions

### 9.1 Accepted

| ID | Decision |
|----|----------|
| A1 | Go (R2; changed from C#, see D0). |
| A2 | Pluggable storage: local volume and Azure Blob first, S3 later (R1, R10). |
| A3 | Endpoints are authenticated (R3); the mechanism is decided at the start of P3 (D6). |
| A4 | Forward-only mirror of the raw feeds, not a proxy in front of GDELT's API. |
| A5 | Events, Mentions and GKG; GKG without GCAM. |
| A6 | Parquet as the storage format. |
| A7 | MIT licence. |
| A8 | Containers preferred. |
| A9 | Freshness: one file per 15-minute batch, compacted into one file per day once the day is complete (was D3). |
| A10 | Both the English and translingual streams, in the same tables with a `translated` column (was D4). |
| A11 | The ingester is one run-once container image; ways to run it are deployment recipes (local and Azure Container Apps Job in v1, AWS later), not code variants (D2). |
| A12 | DuckDB, through its official Go client, converts CSV to Parquet and compacts days. One `.sql` file per feed (D1). |
| A13 | No query service in v1: the lake is the interface. An HTTP API and MCP server in one process come in P3 (D6). |
| A14 | GKG: the commonly used delimited fields are parsed into Parquet lists/structs; the rest stay as GDELT's text (D5 b). More can be parsed later without re-downloading (6.1). |
| A15 | Consumers outside Azure read blob storage directly with DuckDB over the private network; a local sync comes only if needed (D7 a). |
| A16 | Images are published to GitHub Container Registry (D9 a); built locally in P1, public before P2. |
| A17 | The Azure recipe uses Bicep (D8 a). |
| A18 | Exit codes separate run errors (1) and bad configuration (2) from data problems, which are soft (exit 0, WARN, reported by health) (8.1). |
| A19 | Health is reported by the lake itself: `_state/health.json` from every run, and `gdelt-lake status` with exit 3 for data problems (8.4). No health service. |
| A20 | Gaps: missing after 6 hours (so the day compacts), re-checked for the lookback window, late files re-open and re-compact their day; older gaps are reconciled with a longer `--lookback` (8.3). The few seconds of duplicate visibility during compaction stay as they are (no table format). |
| A21 | Backfill is a config value (`LAKE_BACKFILL`); the ingester fetches missing history on startup and every run after, live data first, then history newest-first a day at a time, within a per-run time budget (8.5). |
| A22 | Azure recipe (P2): the jobs run in a new subnet of the deployer's existing virtual network; the lake gets its own new storage account (general-purpose v2, Standard, LRS, Hot tier, no public network access; separate so its tiering and retention rules don't mix with other uses of an existing account) with a private endpoint, whose DNS record goes in the deployer's existing `privatelink.blob.core.windows.net` zone (a parameter); logs go to a Log Analytics workspace (30-day retention, daily cap); backfill is a parameter, default six months. |

### 9.2 Decision records

**D0. Language: Go** (accepted)

The deciding test was a DuckDB client in DuckDB's primary support tier, maintained by its core team. In DuckDB 2.0's
client list that covers C, the CLI, Java, Go, ODBC, Python, Rust and WebAssembly. C# is secondary: one maintainer, and a
major version behind at the time of writing.

Go over Java: one toolchain (`go build`, `go test`), a small image, and one memory pool to size (Java adds a JVM heap
alongside DuckDB's native memory). Smaller containers also cost less where billing is per second of vCPU and memory.
What's given up is Java's sealed types and exhaustive `switch`. Typed constants plus the `exhaustive` linter cover most
of that here, since the only enumerations are feeds, streams, batch states and storage backends.

**D1. What converts GDELT's CSV files to Parquet** (accepted: a)

| Option | Gains | Loses |
|--------|-------|-------|
| a. DuckDB, embedded through its official Go client | CSV reading that already copes with malformed rows, odd encodings and quoting (rejected rows can be counted and logged). A mature Parquet writer: statistics, row-group sizing, zstd, nested lists and structs. Bounded memory, spilling to disk, so compacting a large GKG day doesn't need a large machine. The same engine the query service will use (D6), so one data dependency, not two. Transformations are short SQL statements that can be run by hand in the DuckDB CLI to debug. | A native library: the build needs a C compiler (one Docker build stage) and the image needs the C library. Mistakes in the SQL show up when it runs, not when it compiles. Contributors need to read SQL as well as Go. |
| b. A pure Go Parquet library plus `encoding/csv` | All Go, checked when compiling; no native code. | We write and test the column mapping, types and nested-field building for about 200 columns, and manage memory during compaction. The query service would still need DuckDB. |

Each feed's SQL lives in its own file and is tested against small sample files.

**D2. How the ingester runs** (accepted)

One container image, run once per schedule tick: process any new batches, compact any finished day, exit. The image
doesn't know where it runs. Ways to run it are **deployment recipes** (`deploy/<target>/`), so deployers can choose by
their own goals (R9) without changes to the code.

| Recipe | When it suits | Rough cost (to confirm in P2) |
|--------|---------------|-------------------------------|
| Local: `docker compose`, with a `--every 15m` flag so the container loops | Development; anyone with an always-on machine | Nothing extra |
| Azure: Container Apps **Job** on a cron schedule (Consumption plan) | **Reference deployment (cheapest)**: pays only for seconds running, and runs of a minute or two every 15 minutes probably fit inside the plan's monthly free allowance | Roughly storage only |
| Any Linux VM: cron, or the loop flag | Someone who already has a VM | The VM |
| AWS (later): ECS Fargate task started by EventBridge Scheduler, plus an S3 adapter | AWS deployers | Probably under a few dollars a month |

Not proposed: Azure Functions or AWS Lambda. Each would need its own entry point, and neither is cheaper than a
scheduled container at this load. A recipe can still be added later if a deployer needs one.

To reach storage that's private (R13), the Azure recipe puts the Container Apps environment in a virtual network with a
private endpoint to the storage account.

**D6. Query surface, and with it the auth mechanism** (accepted: a, then c)

| Option | Gains | Loses |
|--------|-------|-------|
| a. None in v1: the lake is the interface. Consumers read Parquet with storage credentials (Azure RBAC or SAS). | No service to build, secure or run. R3 is met by the storage's own auth, since there are no endpoints of ours. | Each consumer needs DuckDB or similar and storage credentials. No MCP access yet. |
| b. HTTP query API (Go standard library) with a few endpoints: article search by terms, themes, country and time; event lookup | One door with our own auth; consumers need only HTTP. | A service to run and secure; endpoint design before we know the real queries. |
| c. (b) plus an MCP server in the same process, exposing the same queries as tools | AI agents can use the lake directly. | As (b), plus MCP's own auth flow. |

(a) in v1, then (c) in P3, once the lake holds a few weeks of data and the queries consumers actually run are known.
Bring P3 forward if a consumer can't be given storage credentials, or needs MCP access sooner. The service stays on
private networks (R13). The auth mechanism is decided at the start of P3. Candidates: API keys, OIDC bearer tokens from
any identity provider (Microsoft Entra ID and others), or Entra ID only. MCP's authorisation spec is OAuth-based,
which favours OIDC.

### 9.3 Decided with the owner (were open)

**D5. GKG's delimited fields**

| Option | Gains | Loses |
|--------|-------|-------|
| a. Store them as GDELT ships them (strings) | Least ingest code. | Every consumer has to re-implement the parsing. |
| b. Parse the commonly used ones into Parquet lists/structs (themes, locations, persons, organisations, tone, page title from `Extras`, translation info); keep the rest as strings | Easy queries for what most people use, e.g. "theme X, located in country Y". | Parsing code to write and test for those fields. |
| c. Parse everything | Complete. | The most code, much of it for rarely used fields. |

*Accepted: (b) (A14).* See 6.1 for adding more later.

**D7. How consumers outside Azure read the lake**

For consumers on a private network connected to Azure (VPN), with the storage account reachable only through a private
endpoint.

| Option | Gains | Loses |
|--------|-------|-------|
| a. DuckDB in each consumer reads blob storage directly, with an Entra ID identity holding a read-only data role | Nothing to run or sync. Queries read only the days and columns they need. | Every query moves data out of Azure, which is charged, and pays network latency. |
| b. A scheduled `azcopy sync` copies a recent window to a local disk; consumers query the local copy | Fast local queries with no charge per query. | A sync job to run, and local disk for the window. |

*Accepted: (a), adding (b) if repeated scans make transfer cost or latency matter (A15).*

**D8. Tooling for the Azure recipe**

| Option | Gains | Loses |
|--------|-------|-------|
| a. Bicep | Declarative and safe to re-run; no state file; needs only the Azure CLI. | Azure only (other clouds get their own native recipe). |
| b. Terraform / OpenTofu | One tool across Azure and AWS. | Every deployer has to set up and protect a state file. |
| c. A plain `az` CLI script | Simplest to read. | Less safe to re-run; drift goes unnoticed. |

*Accepted: (a) (A17).*

**D9. Where the container image is published**

| Option | Gains | Loses |
|--------|-------|-------|
| a. GitHub Container Registry, public once the repo is public | Free, next to the code, built by CI. | While private, pulls count against the account's package transfer allowance, and a Container Apps Job may pull the image on every run (caching on the host isn't guaranteed), so 96 runs a day could go over it. |
| b. Azure Container Registry (Basic) | In the same tenant; pulled with managed identity; no transfer limits from GitHub. | A monthly fee (a few dollars); Azure-specific. |
| c. Docker Hub | Familiar to most deployers. | Pull rate limits for anonymous users; another account. |

*Accepted: (a) (A16).* Images are built locally during P1. The repo and its image are public before the Azure
deployment (P2), so Container Apps pulls a public image: no pull token, and no private-package transfer limits.

### 9.4 Defaults (internal; say if any matter)

Go, current stable release. Standard library first: `net/http`, `log/slog` (JSON to stdout), `encoding/json`,
`testing`. `go vet`, staticcheck and the `exhaustive` linter in CI (GitHub Actions). Configuration from environment
variables only. Parquet with zstd compression. A minimal runtime image with the C library (e.g. distroless `cc`). The
storage contract tested once against every adapter (Azure via the Azurite emulator).

## 10. Phases

| Phase | Delivers | Done when |
|-------|----------|-----------|
| P0. Measure | A throwaway spike pulling one day of both streams. | Sizes per feed and column, GCAM's share, row error rate, conversion and compaction time replace the estimates in 4.4. |
| P1. Ingester, local storage | The ingester (batches and compaction), local and in-memory storage, tests, a locally built container image, the local recipe. Runs on a development host against a local volume. | 48 hours unattended locally, across a day boundary; `_state` accounts for every batch; the README's DuckDB example query works. |
| P2. Azure | Before it starts: the repo and image are public. The Azure Blob adapter and the Bicep recipe, with storage reachable only from private networks (R13). | A week in Azure with no intervention; gaps recorded; actual monthly cost known. |
| P3. Query service | API and MCP server with auth (D6). | Decided when we get there. |
| P4. AWS | S3 adapter and the AWS recipe. | As P2, on AWS. |

## 11. Operations

- **Health:** `_state/health.json` and `gdelt-lake status` (8.4); `run` exits 1 only for run errors; structured logs.
- **Retention:** keep everything in v1. A storage lifecycle rule can move older data to a cheaper tier.
- **Cost:** mostly storage, plus egress when the lake is read from outside its cloud region.

## 12. Licensing and attribution

Code: MIT. GDELT data is free to use, with attribution to the GDELT Project (https://www.gdeltproject.org). Lake
contents are not part of this repository.
