package service

import (
	"context"
	"errors"

	rmsource "github.com/sysop/ultrabridge/internal/source/remarkable"
)

// ErrRemarkableDeviceNotFound is returned by RenameDevice for an unknown
// device_id; the web layer maps it to 404 (sibling of ErrSyncDeviceNotFound).
var ErrRemarkableDeviceNotFound = errors.New("remarkable device not found")

// RemarkableAdmin is the source-level seam for reMarkable device and document
// management. *remarkable.Source satisfies it; keeping this narrow lets the
// service mapping be tested without starting the device protocol.
type RemarkableAdmin interface {
	Devices(ctx context.Context) ([]rmsource.DeviceRow, error)
	ListDocuments(ctx context.Context) ([]rmsource.Document, error)
	SetDeviceLabel(ctx context.Context, deviceID, label string) (bool, error)
}

type remarkableDeviceService struct {
	admin RemarkableAdmin
}

func NewRemarkableDeviceService(admin RemarkableAdmin) RemarkableDeviceService {
	if admin == nil {
		return nil
	}
	return &remarkableDeviceService{admin: admin}
}

func (s *remarkableDeviceService) ListDevices(ctx context.Context) ([]RemarkableDevice, error) {
	rows, err := s.admin.Devices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RemarkableDevice, len(rows))
	for i, d := range rows {
		out[i] = RemarkableDevice{
			DeviceID:  d.DeviceID,
			Name:      d.DeviceDesc,
			Label:     d.Label,
			FirstSeen: d.CreatedAt,
			LastSeen:  d.LastSeen,
		}
	}
	return out, nil
}

func (s *remarkableDeviceService) RenameDevice(ctx context.Context, deviceID, label string) error {
	found, err := s.admin.SetDeviceLabel(ctx, deviceID, normalizeOperatorLabel(label))
	if err != nil {
		return err
	}
	if !found {
		return ErrRemarkableDeviceNotFound
	}
	return nil
}

func (s *remarkableDeviceService) ListDocuments(ctx context.Context) ([]RemarkableDocument, error) {
	docs, err := s.admin.ListDocuments(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RemarkableDocument, len(docs))
	for i, d := range docs {
		out[i] = RemarkableDocument{
			ID:        d.ID,
			Name:      d.Name,
			Type:      d.Type,
			Parent:    d.Parent,
			PageCount: d.PageCount,
		}
	}
	return out, nil
}
