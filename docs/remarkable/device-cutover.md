# reMarkable device cutover: point the tablet at UltraBridge

**Audience:** a Claude instance running on the laptop with **direct access to the
reMarkable tablet** (USB/SSH). This is a self-contained handoff — you are not
expected to have prior context.

**Goal:** the tablet currently syncs to a local **rmfakecloud** instance. We want
it to sync to **UltraBridge (UB)** instead. UB reimplements the reMarkable cloud
sync protocol (it *is* a drop-in cloud, same role rmfakecloud plays). The
server side is built, deployed, and **verified working** — the only remaining
work is **device-side**: repoint the tablet's on-device proxy from rmfakecloud
to UB, then re-pair.

---

## Facts you need

| Thing | Value |
|---|---|
| UB host (LAN) | `192.168.9.52` |
| UB listen port | `8443` (plain HTTP — TLS is terminated by the on-device proxy, same as for rmfakecloud) |
| UB reMarkable source name | `Move` |
| UB pairing code | `remarkab` |
| Tablet SSH (USB) | `ssh root@10.11.99.1` (standard reMarkable) |
| Server-side log access | `ssh sysop@192.168.9.52 'docker logs -f ultrabridge'` |

**Server side is already proven:** a direct `POST /token/json/2/device/new`
with code `remarkab` returns `200` + a device token, and `192.168.9.52:8443` is
reachable on the LAN. So if pairing fails, the cause is in the **device → UB
path**, not UB.

---

## The architecture (why this is a one-knob change)

The tablet reaches its "cloud" like this:

```
xochitl → /etc/hosts redirects *.appspot.com / my.remarkable.com → on-device proxy
        → on-device proxy terminates TLS, forwards to a BACKEND (the "upstream")
        → BACKEND = rmfakecloud today; we change it to UB
```

Because **rmfakecloud already works on this tablet**, the hard parts are already
done: `/etc/hosts` redirects are in place, the `*.appspot.com` CA is trusted,
and the on-device TLS proxy is running. **The only thing that must change is the
proxy's upstream/backend address** — from the rmfakecloud address to
`http://192.168.9.52:8443`.

---

## Procedure

### 0. Record the current state first (so it's reversible)

SSH to the tablet and capture the current proxy config before touching anything:

```sh
ssh root@10.11.99.1
# Find the proxy. It's typically rmfakecloud-proxy or `secure`, run via systemd.
systemctl list-units --type=service | grep -iE 'proxy|secure|rmfake'
cat /etc/hosts                                  # note the redirected hostnames
# Inspect the proxy unit + its config to find the CURRENT upstream:
systemctl cat <proxy-service>                   # e.g. rmfakecloud-proxy.service
# Common config locations (varies by install):
ls -la /opt/etc /etc/secure 2>/dev/null
grep -rinE 'appspot|http://|https://|StorageUrl|STORAGE_URL|upstream|backend' \
    /etc/systemd/system /opt/etc 2>/dev/null | grep -i 'remark\|appspot\|http' 
```

**Write down the current upstream value** (e.g. `http://192.168.9.52:30XX` — one
of the rmfakecloud ports 3010–3013). Reverting = setting it back.

### 1. Repoint the proxy upstream to UB

Change the backend the on-device proxy forwards to:

```
http://192.168.9.52:8443
```

Where you change it depends on the proxy:
- **rmfakecloud-proxy (systemd):** edit its config (often a `proxycfg` file or an
  `Environment=`/`ExecStart` arg) and set the upstream to `http://192.168.9.52:8443`.
- **`secure`:** it's invoked as `secure -cert proxy.crt -key proxy.key http://BACKEND:PORT`
  — change `BACKEND:PORT` to `192.168.9.52:8443` in the unit's `ExecStart`.

Leave `STORAGE_URL` on the device as-is (`https://local.appspot.com`) — that
points at the *local proxy*, not the backend. You're only changing what the
proxy forwards to. **Do not** add a port to `STORAGE_URL` (firmware ≥3.15 only
accepts `https://host` with no port).

The proxy MUST, when forwarding to UB:
- preserve the **`Host`** header (the appspot hostname), and
- set **`X-Forwarded-Proto: https`**, and
- pass **WebSocket upgrades** on `/notifications/ws/json/1`.

(rmfakecloud needs the first two as well, so an existing working proxy almost
certainly already does them — just confirm if discovery/blob URLs misbehave.)

### 2. Mark all files "new" so nothing gets deleted

UB's cloud is **empty** (fresh — no documents yet). A device that believes it's
already synced, pointed at an empty cloud, can interpret "cloud has nothing" as
"everything was deleted." Prevent that — on the tablet, run the rmfakecloud
`fixsync.sh` (marks every file as new/unsynced so the device **uploads** rather
than reconciles):

```sh
# fixsync.sh ships with the rmfakecloud device installer; locate and run it:
find / -name 'fixsync*.sh' 2>/dev/null
sh /path/to/fixsync.sh
```

If you can't find it, the equivalent is clearing the per-document sync status so
xochitl re-uploads. Do **not** skip this — it's the difference between a clean
first upload and losing notebooks.

### 3. Restart and re-pair

```sh
systemctl restart <proxy-service>
systemctl restart xochitl
```

On the tablet UI: Settings → (account) → **Connect / re-pair**, and enter the
code **`remarkab`**. The device's existing rmfakecloud token won't validate
against UB, so a fresh pair is expected.

### 4. Verify — watch UB receive the traffic

From the laptop, tail UB's log **while** you trigger the pair:

```sh
ssh sysop@192.168.9.52 'docker logs -f ultrabridge'
```

Success ladder (you want to see each in turn):
1. `POST /token/json/2/device/new` → **200** — pairing reached UB and the code
   was accepted. (A **400** means it reached UB but the code is wrong; a **total
   absence** means the proxy still isn't forwarding to UB — recheck step 1.)
2. `POST /token/json/2/user/new` → 200 — device upgraded to a user token.
3. `GET /sync/v3/root` then `PUT /sync/v3/root` / blob `PUT`s — the device is
   syncing its tree up.
4. Confirm the documents surfaced (run on the laptop):
   ```sh
   curl -u <ub-user>:<ub-pass> https://<ub-public-host>/api/v1/remarkable/documents
   # → {"documents":[{id,name,type,parent,page_count}, …]}
   ```
   (Or hit `http://192.168.9.52:8443/...` directly on the LAN with Basic Auth.)
5. Live push (optional): with a second client holding
   `/notifications/ws/json/1` open (Bearer user token), a sync should deliver a
   `SyncComplete` frame.

---

## Failure modes → diagnosis

| Symptom | Likely cause | Fix |
|---|---|---|
| "Unable to complete pairing" **and** nothing in UB log | Proxy still forwarding to rmfakecloud, or can't reach `192.168.9.52:8443` | Confirm the upstream change took; from the tablet: `curl -v http://192.168.9.52:8443/health` (expect `200`) |
| `POST /token/.../device/new` → **400** in UB log | Reached UB but wrong code | Enter exactly `remarkab` (UB pairing code; case-sensitive) |
| Pairs, but sync stalls / blob 404s | Proxy not preserving `Host` / `X-Forwarded-Proto: https` → UB advertises wrong URLs | Fix proxy header passthrough; UB derives advertised URLs from the request |
| Device won't open the notifications socket | Proxy not upgrading WebSockets on `/notifications/ws/json/1`, or discovery `notifications` host format | Enable WS upgrade on that path. If still failing, the discovery field may need a bare host vs `https://host` — flag it back to the server-side Claude (it's a one-line change in `internal/source/remarkable/protocol.go:handleDiscoveryEndpoints`) |
| TLS errors on the tablet | `*.appspot.com` CA not trusted (unlikely if rmfakecloud worked) | Re-trust the proxy CA: copy to `/usr/local/share/ca-certificates`, `update-ca-certificates` |
| Notebooks disappear after first sync | `fixsync.sh` not run before cutover | Restore from the tablet's local trash/backup; re-run with fixsync next time |

---

## Reverting

Set the proxy upstream back to the recorded rmfakecloud address (from step 0),
restart the proxy + xochitl. UB never touches the rmfakecloud data, so the old
cloud is intact. This is a low-risk, reversible experiment.

---

## What's actually new server-side (for your awareness)

UB implements: device pairing + token flow, legacy document-storage v2, modern
sync v3/v4 blob storage, a read-only document/folder listing
(`GET /api/v1/remarkable/documents`, the basis for a future Files tab), and a
live-notification WebSocket (`/notifications/ws/json/1`, `SyncComplete` push to
a user's other devices). MQTT (the rmfakecloud `:8883` broker) is **screenshare
only** and is **not** implemented in UB — it is not needed for document sync.

Storage path: the source's `data_path` is `/mnt/remarkable` (a CephFS mount on
the host, now bind-mounted into the container). Blobs/documents persist there.

If you find a genuine **server-side** wire mismatch (e.g. discovery host format,
a missing field a real device requires), capture the exact failing request from
the UB log and hand it to the server-side Claude on `192.168.9.52` — the fix
will be in `internal/source/remarkable/`.
