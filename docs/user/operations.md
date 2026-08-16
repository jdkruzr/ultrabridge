# Operations

## Data To Back Up

Back up the data directory and any configured source roots:

- `ultrabridge.db`
- `ultrabridge-tasks.db`
- `task-attachments/` (signed task attachment blobs; `UB_TASK_ATTACH_DIR`, default `/data/task-attachments`)
- Supernote SPC file root
- Boox notes path and `.versions`
- reMarkable data path
- Any external cache/render paths you configured

For simple filesystem backups, stop the container before copying SQLite databases.

## Pipeline Status

Every page carries a persistent **Pipeline Status** bar. The summary line shows whatever is active or needs attention; the toggle expands a per-source breakdown that stays expanded as you navigate. Supernote and Boox have start/stop worker controls in the bar; ForestNote and reMarkable report status only (their work is per-notebook/per-document). Check this bar before digging into logs.

## Logs

Use:

```bash
docker logs ultrabridge
```

or the web UI **Logs** tab. Enable verbose API logging in **Settings -> System** when debugging auth, OAuth, CalDAV, or API clients.

## Device Registries

ForestNote and reMarkable devices appear under **Settings -> Devices** with health information. Give each device a name you recognize — the name is stored separately from what the device reports and is never overwritten by a sync. This is the difference between "prune `01J8...ULID`" and "prune the old kitchen tablet."

## ForestNote Relay Log

The ForestNote `sync_ops` relay log grows by one row per edit. Enable **Relay-log compaction** (Settings -> Devices -> ForestNote) to sweep it on a schedule, or press **Compact Relay Log** for a one-off pass; it collapses superseded snapshots and purges tombstones every device has pulled past. A dead device's registration pins that history — prune it first (its notes are kept; an active device simply re-registers on its next sync).

## Rebuild

```bash
./rebuild.sh
```

or:

```bash
docker compose up -d --build
```

`rebuild.sh` force-recreates the container and waits up to 180 seconds for `/health`. It also accepts `--fresh` (delete both SQLite databases) and `--nuke` (delete all data) — both destructive, both confirmed unless `-y` is passed.

## Health

```bash
curl http://localhost:8443/health
```

The response includes `status` and `config_dirty`. A dirty config usually means a restart-sensitive setting changed.

## Recovery Checklist

1. Check health.
2. Check the Pipeline Status bar.
3. Check container logs.
4. Check the relevant source row in **Settings -> Devices**.
5. Confirm paths are mounted into the container.
6. Confirm reverse-proxy routing and hostnames.
7. Reprocess or rescan a small item before triggering a broad backfill.

## Troubleshooting

See [Troubleshooting](../TROUBLESHOOTING.md) for client-specific checks.
