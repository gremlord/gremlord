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

command -v python3 >/dev/null || { echo "python3 is required but not found" >&2; exit 1; }

# Prefer gh locally; fall back to curl with a token from the environment. The
# cloud runner that executes this on a schedule has curl and GH_TOKEN but no gh
# CLI, and a gh-only script silently skips every scheduled run — which loses the
# 14-day window this exists to preserve.
TOKEN="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
if command -v gh >/dev/null 2>&1; then
    api() { gh api "$1"; }
elif [ -n "$TOKEN" ] && command -v curl >/dev/null 2>&1; then
    api() {
        curl -fsSL \
            -H "Authorization: Bearer $TOKEN" \
            -H "Accept: application/vnd.github+json" \
            -H "X-GitHub-Api-Version: 2022-11-28" \
            "https://api.github.com/$1"
    }
else
    echo "need either the gh CLI, or curl plus GH_TOKEN/GITHUB_TOKEN in the environment" >&2
    exit 1
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/docs/metrics"
[ "$DRY_RUN" -eq 1 ] && OUT="$(mktemp -d)"
mkdir -p "$OUT"

TODAY="$(date -u +%Y-%m-%d)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Traffic endpoints require push access; fail loudly rather than writing zeros.
for ep in clones views; do
    if ! api "repos/$REPO/traffic/$ep" > "$WORK/$ep.json" 2>"$WORK/$ep.err"; then
        echo "ERROR: could not read repos/$REPO/traffic/$ep" >&2
        echo "  The traffic API needs push access on the repo. Response:" >&2
        sed 's/^/  /' "$WORK/$ep.err" >&2
        exit 1
    fi
done
api "repos/$REPO/traffic/popular/referrers" > "$WORK/referrers.json"
api "repos/$REPO/traffic/popular/paths"     > "$WORK/paths.json"
api "repos/$REPO"                           > "$WORK/repo.json"
# 100 per page covers every release this project will plausibly have; the
# reader below accepts either a flat list or gh's paginated list-of-lists.
api "repos/$REPO/releases?per_page=100"     > "$WORK/releases.json"

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
releases = load("releases")
if releases and isinstance(releases[0], list):
    releases = [rel for page in releases for rel in page]   # gh --slurp shape
downloads = sum(a["download_count"] for rel in releases for a in rel["assets"])
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
