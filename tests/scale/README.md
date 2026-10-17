# Scale tests

How the parts of RedundaNet that touch every object behave as the object
count grows. None of this runs in CI or in `pytest tests/unit`: it needs
minutes to hours, disk, and (tier 2) Docker. Everything runs locally; the
live network is never touched.

```bash
scripts/scale-test.sh tier1                 # pure Python, minutes
scripts/scale-test.sh tier2                 # real grid in Docker, hours at large sizes
FILES=20000 DIRS=2000 scripts/scale-test.sh tier2
```

| Script | What it measures |
|---|---|
| `bench_census.py` | A storage node's share census at N objects: the walk, the JSON body, the ETag, and what the hub pays to parse and hold it. `--drop-caches` (Linux, root) measures a cold walk. |
| `bench_hub.py` | The hub's status collection with 8 nodes holding N objects each: seconds per tick and process RSS, with and without the census cache writes, with unchanged (304) and changed censuses. |
| `gen_tree.py` | The synthetic backup source for tier 2: many unique small files in many directories, plus a churn pass that rewrites a percentage of them. |
| `walk_webapi.py` | Runs inside the client container: the usage meter's walk done through the node's web API instead of one `tahoe ls` process per directory. |

Tier 2 steps (see `scripts/scale-test.sh`): first `tahoe backup`, incremental
backups after churn, one usage-meter measurement, the same walk via the web
API, a lease sweep (`deep-check --add-lease --repair`), a backup after aging
the backupdb by 70 days (Tahoe then re-checks every file on the grid), the
census on each storage node warm and cold, and a storage restart during a
lease-crawler cycle to reproduce the Tahoe 1.20 expirer crash.

The Linux numbers are the ones that matter: run the tier 1 benchmarks inside
the storage image when developing on macOS (APFS directory walks are three
to four times slower than ext4):

```bash
docker run --rm --privileged -v rn-scale-bench:/data/bench -v "$PWD":/repo:ro \
  --entrypoint python3 redundanet-grid-storage:latest \
  /repo/tests/scale/bench_census.py --objects 1000000 --root /data/bench --drop-caches
```
