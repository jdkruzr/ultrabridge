package service

import (
	"context"

	rmsource "github.com/sysop/ultrabridge/internal/source/remarkable"
)

type remarkableDeviceService struct {
	admin *rmsource.Source
}

func NewRemarkableDeviceService(admin *rmsource.Source) RemarkableDeviceService {
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
