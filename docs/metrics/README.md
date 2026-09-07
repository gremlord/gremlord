# Traffic metrics

GitHub serves only a rolling **14-day** window of clone and view counts and
discards anything older, with no way to recover it. These CSVs exist so the
project has a real series instead of a window that keeps forgetting.

Written by [`scripts/traffic-snapshot.sh`](../../scripts/traffic-snapshot.sh),
which is idempotent — re-running on the same day refreshes that day rather than
double-counting it. Run it at least every 14 days or the gap is permanent.

| File | Shape |
|---|---|
| `traffic-daily.csv` | One row per day: clones, unique cloners, views, unique visitors. Upserted by date, so the current day's partial bucket gets corrected by the next run. |
| `referrers.csv` | Dated snapshot of the top referrers. The API gives a 14-day aggregate with no daily breakdown, so a dated snapshot is the only honest shape. |
| `paths.csv` | Dated snapshot of the most-visited paths, same caveat. |
| `repo.csv` | Dated snapshot of stars, forks, watchers, open issues, and total release-asset downloads. |

Two things these numbers are not. Clone counts include CI and scrapers, so they
overstate humans. Release-asset downloads include the maintainer's own
`gremlord update` runs across every release, so early counts are mostly self-traffic.
