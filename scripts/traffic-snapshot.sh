#!/usr/bin/env bash
# Snapshot GitHub traffic into docs/metrics/, which git keeps forever.
#
#   scripts/traffic-snapshot.sh [--repo owner/name] [--dry-run]
#
# GitHub serves only a rolling 14-day window of clone and view counts and
# discards everything older, so a series only exists if something writes it
# down. Run this at least every 14 days or the gap is unrecoverable.
#
# Idempotent: the daily series is upserted by date, so re-running the same day
# refreshes today's partial bucket instead of double-counting it. Traffic
# endpoints need push access on the repo.
set -euo pipefail

REPO="gremlord/gremlord"
DRY_RUN=0
while [ $# -gt 0 ]; do
    case "$1" in
        --repo) REPO="$2"; shift 2 ;;
        --dry-run) DRY_RUN=1; shift ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

command -v gh >/dev/null || { echo "gh is required but not found" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is required but not found" >&2; exit 1; }

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/docs/metrics"
[ "$DRY_RUN" -eq 1 ] && OUT="$(mktemp -d)"
mkdir -p "$OUT"

TODAY="$(date -u +%Y-%m-%d)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Traffic endpoints require push access; fail loudly rather than writing zeros.
for ep in clones views; do
    if ! gh api "repos/$REPO/traffic/$ep" > "$WORK/$ep.json" 2>"$WORK/$ep.err"; then
        echo "ERROR: could not read repos/$REPO/traffic/$ep" >&2
        echo "  The traffic API needs push access on the repo. Response:" >&2
        sed 's/^/  /' "$WORK/$ep.err" >&2
        exit 1
    fi
done
gh api "repos/$REPO/traffic/popular/referrers" > "$WORK/referrers.json"
gh api "repos/$REPO/traffic/popular/paths"     > "$WORK/paths.json"
gh api "repos/$REPO"                           > "$WORK/repo.json"
gh api "repos/$REPO/releases" --paginate --slurp > "$WORK/releases.json"

python3 - "$WORK" "$OUT" "$TODAY" <<'PY'
import csv, json, os, sys

work, out, today = sys.argv[1], sys.argv[2], sys.argv[3]
load = lambda n: json.load(open(os.path.join(work, n + ".json")))


def write_csv(path, header, rows):
    with open(path, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(header)
        w.writerows(rows)


def upsert_daily():
    """Merge the 14-day clone/view buckets into one gap-free daily series.

    GitHub's bucket for the current day is partial, so a later run must be able
    to correct a date it has already written -- hence upsert by date, not append.
    """
    path = os.path.join(out, "traffic-daily.csv")
    rows = {}
    if os.path.exists(path):
        with open(path, newline="") as f:
            for r in csv.DictReader(f):
                rows[r["date"]] = r

    for key, series in (("clones", "clones"), ("views", "views")):
        for day in load(key).get(series, []):
            date = day["timestamp"][:10]
            r = rows.setdefault(date, {"date": date, "clones": "", "clone_uniques": "",
                                       "views": "", "view_uniques": ""})
            if key == "clones":
                r["clones"], r["clone_uniques"] = day["count"], day["uniques"]
            else:
                r["views"], r["view_uniques"] = day["count"], day["uniques"]

    header = ["date", "clones", "clone_uniques", "views", "view_uniques"]
    write_csv(path, header, [[rows[d].get(h, "") for h in header] for d in sorted(rows)])
    return len(rows)


def append_snapshot(name, header, new_rows):
    """Append a point-in-time snapshot, replacing today's if it already exists.

    Referrers and paths are 14-day aggregates with no daily breakdown, so the
    only honest shape is a dated snapshot.
    """
    path = os.path.join(out, name)
    kept = []
    if os.path.exists(path):
        with open(path, newline="") as f:
            rdr = csv.reader(f)
            next(rdr, None)
            kept = [r for r in rdr if r and r[0] != today]
    write_csv(path, header, kept + new_rows)


append_snapshot("referrers.csv", ["snapshot_date", "referrer", "count", "uniques"],
                [[today, r["referrer"], r["count"], r["uniques"]] for r in load("referrers")])
append_snapshot("paths.csv", ["snapshot_date", "path", "title", "count", "uniques"],
                [[today, p["path"], p["title"], p["count"], p["uniques"]] for p in load("paths")])

repo = load("repo")
downloads = sum(a["download_count"]
                for page in load("releases") for rel in page for a in rel["assets"])
append_snapshot("repo.csv",
                ["snapshot_date", "stars", "forks", "watchers", "open_issues",
                 "release_downloads"],
                [[today, repo["stargazers_count"], repo["forks_count"],
                  repo["subscribers_count"], repo["open_issues_count"], downloads]])

days = upsert_daily()
clones, views = load("clones"), load("views")
print(f"{today}: {days} days of history | 14d clones {clones['count']} "
      f"({clones['uniques']} unique), views {views['count']} ({views['uniques']} unique) | "
      f"stars {repo['stargazers_count']}, downloads {downloads}")
PY

if [ "$DRY_RUN" -eq 1 ]; then
    echo "--- dry run, wrote to $OUT ---"
    head -100 "$OUT"/*.csv
fi
