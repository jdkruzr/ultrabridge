# Operations

## Data To Back Up

Back up the data directory and any configured source roots:

- `ultrabridge.db`
- `ultrabridge-tasks.db`
- Supernote SPC file root
- Boox notes path and `.versions`
- reMarkable data path
- Any external cache/render paths you configured

For simple filesystem backups, stop the container before copying SQLite databases.

## Logs

Use:

```bash
docker logs ultrabridge
```

or the web UI **Logs** tab. Enable verbose API logging in **Settings -> System** when debugging auth, OAuth, CalDAV, or API clients.

## Rebuild

```bash
./rebuild.sh
```

or:

```bash
docker compose up -d --build
```

## Health

```bash
curl http://localhost:8443/health
```

The response includes `status` and `config_dirty`. A dirty config usually means a restart-sensitive setting changed.

## Recovery Checklist

1. Check health.
2. Check container logs.
3. Check the relevant source row in **Settings -> Devices**.
4. Confirm paths are mounted into the container.
5. Confirm reverse-proxy routing and hostnames.
6. Reprocess or rescan a small item before triggering a broad backfill.

## Troubleshooting

See [Troubleshooting](../TROUBLESHOOTING.md) for client-specific checks.
