package service

import (
	"context"
	"errors"

	"github.com/sysop/ultrabridge/internal/syncstore"
)

// ErrSyncDeviceNotFound is returned by PruneSyncDevice for an unknown site_id;
// the web layer maps it to 404.
var ErrSyncDeviceNotFound = errors.New("sync device not found")

// ForestNoteSyncAdmin is the source-level device-management seam.
// *forestnote.Source satisfies it (same pattern as ForestNoteReprocessor: the
// source holds the store plus the compaction config the narrow store-level
// reads don't have).
type ForestNoteSyncAdmin interface {
	Devices(ctx context.Context) ([]syncstore.DeviceRow, error)
	PruneDevice(ctx context.Context, siteID string) (bool, error)
	CompactNow(ctx context.Context) (syncstore.CompactOutcome, error)
}

type syncDeviceService struct {
	admin ForestNoteSyncAdmin
}

// NewSyncDeviceService wraps the ForestNote source's device-management seam for
// the web layer. Returns nil if admin is nil so callers can gate the Settings
// card and API routes on a nil service (mirrors NewDigestService).
func NewSyncDeviceService(admin ForestNoteSyncAdmin) SyncDeviceService {
	if admin == nil {
		return nil
	}
	return &syncDeviceService{admin: admin}
}

func (s *syncDeviceService) ListSyncDevices(ctx context.Context) ([]SyncDevice, error) {
	rows, err := s.admin.Devices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SyncDevice, len(rows))
	for i, d := range rows {
		out[i] = SyncDevice{
			SiteID:        d.SiteID,
			Name:          d.Name,
			FirstSeen:     d.FirstSeenMs,
			LastSeen:      d.LastSeenMs,
			LastPullSeq:   d.LastPullSeq,
			AckedOpSeq:    d.AckedOpSeq,
			PendingOps:    d.PendingOps,
			Stale:         d.Stale,
			PinsWatermark: d.PinsWatermark,
		}
	}
	return out, nil
}

func (s *syncDeviceService) PruneSyncDevice(ctx context.Context, siteID string) error {
	found, err := s.admin.PruneDevice(ctx, siteID)
	if err != nil {
		return err
	}
	if !found {
		return ErrSyncDeviceNotFound
	}
	return nil
}

func (s *syncDeviceService) CompactNow(ctx context.Context) (SyncCompactResult, error) {
	o, err := s.admin.CompactNow(ctx)
	if err != nil {
		return SyncCompactResult{}, err
	}
	evicted := o.Evicted
	if evicted == nil {
		evicted = []string{} // emit [] not null on the JSON surface
	}
	return SyncCompactResult{
		Watermark:           o.Watermark,
		CollapsedSuperseded: o.CollapsedSuperseded,
		PurgedTombstones:    o.PurgedTombstones,
		EvictedSites:        evicted,
	}, nil
}
