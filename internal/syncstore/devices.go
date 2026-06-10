package syncstore

import (
	"context"
	"fmt"

	"github.com/jdkruzr/rhizome/server-go/compaction"
)

// DeviceRow is one synced device as the management UI sees it: the sync_cursors
// registry row joined with derived health fields. FirstSeenMs is decoded from
// the site_id ULID's embedded timestamp (the moment the client minted it — when
// that install first enabled sync), not stored.
type DeviceRow struct {
	SiteID      string
	Name        string // "" if the device never sent a device_name
	FirstSeenMs int64  // 0 if the ULID timestamp half is all-zero (test/synthetic ids)
	LastSeenMs  int64  // sync_cursors.updated_at
	LastPullSeq int64
	AckedOpSeq  int64
	PendingOps  int64 // relay ops this device has not pulled yet (excluding its own)
	// Stale mirrors compaction's stale-site eviction: unseen for STRICTLY longer
	// than the horizon, so it no longer holds back the tombstone watermark.
	Stale bool
	// PinsWatermark marks the active laggard: a non-stale device whose pull
	// high-water IS the watermark while some other active device is strictly
	// ahead — i.e. the device currently holding tombstone compaction back. A
	// sole device (or devices all equally caught up) pins nothing.
	PinsWatermark bool
}

// ListDevices returns every registered device ordered by most recently seen.
// nowMs is injected for deterministic staleness in tests (same convention as
// ComputeWatermark); staleHorizonMs is the compaction stale horizon.
func (s *Store) ListDevices(ctx context.Context, nowMs, staleHorizonMs int64) ([]DeviceRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.site_id, c.device_name, c.last_pull_seq, c.acked_op_seq, c.updated_at,
		        (SELECT COUNT(*) FROM sync_ops o
		          WHERE o.seq > c.last_pull_seq AND o.site_id <> c.site_id) AS pending
		   FROM sync_cursors c
		  ORDER BY c.updated_at DESC, c.site_id`)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()

	var out []DeviceRow
	sites := map[string]compaction.Site{}
	var maxActivePull int64
	var activeSites int
	for rows.Next() {
		var d DeviceRow
		if err := rows.Scan(&d.SiteID, &d.Name, &d.LastPullSeq, &d.AckedOpSeq, &d.LastSeenMs, &d.PendingOps); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		d.FirstSeenMs, _ = ULIDTime(d.SiteID)
		d.Stale = nowMs-d.LastSeenMs > staleHorizonMs
		if !d.Stale {
			activeSites++
			if d.LastPullSeq > maxActivePull {
				maxActivePull = d.LastPullSeq
			}
		}
		sites[d.SiteID] = compaction.Site{LastPullSeq: d.LastPullSeq, LastSeenUnixMs: d.LastSeenMs}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate devices: %w", err)
	}

	// Same eviction rule as ComputeWatermark, so the badge agrees with what a
	// compaction pass would actually do.
	watermark, _ := compaction.Watermark(sites, nowMs, staleHorizonMs)
	for i := range out {
		d := &out[i]
		d.PinsWatermark = !d.Stale && activeSites > 1 &&
			d.LastPullSeq == watermark && d.LastPullSeq < maxActivePull
	}
	return out, nil
}

// DeleteDevice removes a device's registry (cursor) row — the prune operation
// (spec §4.3). Cleanup only: ops the device authored stay in the changelog and
// mirror, and a still-alive device transparently re-registers on its next sync
// (ApplyBatch reseeds its accepted_through from the changelog). Returns whether
// a row was actually deleted.
func (s *Store) DeleteDevice(ctx context.Context, siteID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sync_cursors WHERE site_id = ?`, siteID)
	if err != nil {
		return false, fmt.Errorf("delete device %s: %w", siteID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete device %s: rows affected: %w", siteID, err)
	}
	return n > 0, nil
}

// CompactOutcome is one full watermark + sweep pass's result, as surfaced to
// the management UI/API ("Run compaction now") and the periodic ticker's log.
type CompactOutcome struct {
	Watermark           int64
	Evicted             []string // stale site_ids excluded from the watermark this pass
	CollapsedSuperseded int
	PurgedTombstones    int
}
