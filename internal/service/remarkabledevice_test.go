package service

import (
	"context"
	"errors"
	"testing"

	rmsource "github.com/sysop/ultrabridge/internal/source/remarkable"
)

type fakeRemarkableAdmin struct {
	devices      []rmsource.DeviceRow
	devicesErr   error
	documents    []rmsource.Document
	documentsErr error
}

func (f *fakeRemarkableAdmin) Devices(context.Context) ([]rmsource.DeviceRow, error) {
	return f.devices, f.devicesErr
}

func (f *fakeRemarkableAdmin) ListDocuments(context.Context) ([]rmsource.Document, error) {
	return f.documents, f.documentsErr
}

func TestNewRemarkableDeviceService_NilAdminYieldsNil(t *testing.T) {
	if svc := NewRemarkableDeviceService(nil); svc != nil {
		t.Error("nil admin must yield a nil service so the UI card and API can gate on it")
	}
}

func TestRemarkableListDevices_MapsFields(t *testing.T) {
	admin := &fakeRemarkableAdmin{devices: []rmsource.DeviceRow{{
		DeviceID: "dev-1", DeviceDesc: "Paper Pro", CreatedAt: 100, LastSeen: 200,
	}}}

	got, err := NewRemarkableDeviceService(admin).ListDevices(context.Background())
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	want := RemarkableDevice{DeviceID: "dev-1", Name: "Paper Pro", FirstSeen: 100, LastSeen: 200}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("mapped devices = %+v, want %+v", got, want)
	}
}

func TestRemarkableListDevices_PropagatesError(t *testing.T) {
	admin := &fakeRemarkableAdmin{devicesErr: errors.New("db down")}
	if _, err := NewRemarkableDeviceService(admin).ListDevices(context.Background()); err == nil || err.Error() != "db down" {
		t.Fatalf("ListDevices error = %v, want db down", err)
	}
}

func TestRemarkableListDocuments_MapsFields(t *testing.T) {
	admin := &fakeRemarkableAdmin{documents: []rmsource.Document{
		{ID: "folder-1", Name: "Projects", Type: "folder", Parent: "", PageCount: 0},
		{ID: "doc-1", Name: "Meeting Notes", Type: "document", Parent: "folder-1", PageCount: 12},
	}}

	got, err := NewRemarkableDeviceService(admin).ListDocuments(context.Background())
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	want := []RemarkableDocument{
		{ID: "folder-1", Name: "Projects", Type: "folder", Parent: "", PageCount: 0},
		{ID: "doc-1", Name: "Meeting Notes", Type: "document", Parent: "folder-1", PageCount: 12},
	}
	if len(got) != len(want) {
		t.Fatalf("documents len = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("documents[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRemarkableListDocuments_PropagatesError(t *testing.T) {
	admin := &fakeRemarkableAdmin{documentsErr: errors.New("tree unavailable")}
	if _, err := NewRemarkableDeviceService(admin).ListDocuments(context.Background()); err == nil || err.Error() != "tree unavailable" {
		t.Fatalf("ListDocuments error = %v, want tree unavailable", err)
	}
}
