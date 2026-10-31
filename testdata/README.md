# Test data

Small samples of real GDELT 2.0 files, used by the conversion tests. GDELT data is free to use with attribution:
[the GDELT Project](https://www.gdeltproject.org).

| File | Cut from | Records |
|------|----------|---------|
| `en.events.csv` | `20261030141500.export.CSV` | first 20 |
| `en.mentions.csv` | `20261030141500.mentions.CSV` | first 20 |
| `en.gkg.csv` | `20261030141500.gkg.csv` | first 15 |
| `tr.events.csv` | `20261030141500.translation.export.CSV` | first 20 |
| `tr.mentions.csv` | `20261030141500.translation.mentions.CSV` | first 20 |
| `tr.gkg.csv` | `20261030141500.translation.gkg.csv`, plus one record from `20261030090000.translation.gkg.csv` | 15 |

The files are unchanged except for two things:

- In the GKG samples, the GCAM column (column 18) is cut to its first 60 characters to keep the files small. The
  ingester drops GCAM, so this doesn't affect the tests.
- `tr.gkg.csv` was chosen to include the two problems found in real files: two records whose last column (Extras)
  contains line breaks, so each spans several lines, and one record with bytes that aren't valid UTF-8 (the last one).
