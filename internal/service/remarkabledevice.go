package service

import (
	"context"

	rmsource "github.com/sysop/ultrabridge/internal/source/remarkable"
)

// RemarkableAdmin is the source-level read seam for reMarkable device and
// document management. *remarkable.Source satisfies it; keeping this narrow
// lets the service mapping be tested without starting the device protocol.
type RemarkableAdmin interface {
	Devices(ctx context.Context) ([]rmsource.DeviceRow, error)
	ListDocuments(ctx context.Context) ([]rmsource.Document, error)
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
			FirstSeen: d.CreatedAt,
			LastSeen:  d.LastSeen,
		}
	}
	return out, nil
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
