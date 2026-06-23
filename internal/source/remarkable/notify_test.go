package remarkable

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sysop/ultrabridge/internal/source"
)

// TestWsMessageWireFormat pins the exact JSON the device expects (rmfakecloud
// internal/messages/messages.go). The tags are the wire contract.
func TestWsMessageWireFormat(t *testing.T) {
	msg := wsMessage{
		Message: notificationMessage{
			MessageID3: "1716295631000000000",
			Attributes: attributes{
				Auth0UserID:    "remarkable",
				Event:          eventSyncComplete,
				SourceDeviceID: "device-a",
			},
		},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		`"message":`,
		`"attributes":`,
		`"auth0UserID":"remarkable"`,
		`"event":"SyncComplete"`,
		`"sourceDeviceID":"device-a"`,
		`"messageid":"1716295631000000000"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wsMessage JSON missing %q in:\n%s", want, got)
		}
	}
	// Empty subscription must omit, not serialize null/"".
	if strings.Contains(got, "subscription") {
		t.Errorf("empty subscription should be omitted:\n%s", got)
	}
}

// startNotifyServer boots a started Source serving its protocol mux over an
// httptest.Server, returning the server and the source.
func startNotifyServer(t *testing.T) (*httptest.Server, *Source) {
	t.Helper()
	db := testDB(t)
	row := source.SourceRow{
		Type:       "remarkable",
		Name:       "RM",
		ConfigJSON: `{"data_path":"` + t.TempDir() + `","pairing_code":"123456"}`,
	}
	src, err := NewSource(db, row, source.SharedDeps{})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mux := http.NewServeMux()
	src.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Cleanup(src.Stop)
	return srv, src
}

func dialNotifyWS(t *testing.T, srv *httptest.Server, token, deviceID string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/notifications/ws/json/1"
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	_ = deviceID
	return websocket.DefaultDialer.Dial(wsURL, hdr)
}

// TestNotificationsFanOut: device A's sync notifies device B but not A.
func TestNotificationsFanOut(t *testing.T) {
	srv, _ := startNotifyServer(t)
	mux := http.NewServeMux()
	// We need the same mux that the httptest.Server serves; reuse via the
	// pairing helper which posts against srv directly.

	tokenA := pairUserTokenHTTP(t, srv, "device-a", "reMarkable 2")
	tokenB := pairUserTokenHTTP(t, srv, "device-b", "reMarkable Paper Pro")

	connA, _, err := dialNotifyWS(t, srv, tokenA, "device-a")
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	defer connA.Close()
	connB, _, err := dialNotifyWS(t, srv, tokenB, "device-b")
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer connB.Close()

	// Give both read pumps a moment to register on the hub.
	time.Sleep(50 * time.Millisecond)

	// Device A commits a sync root — should notify B, not A.
	putRootV3(t, srv, tokenA)

	// B must receive a SyncComplete frame.
	connB.SetReadDeadline(time.Now().Add(2 * time.Second))
	var got wsMessage
	if err := connB.ReadJSON(&got); err != nil {
		t.Fatalf("device B read: %v", err)
	}
	if got.Message.Attributes.Event != eventSyncComplete {
		t.Errorf("B event = %q, want SyncComplete", got.Message.Attributes.Event)
	}
	if got.Message.Attributes.SourceDeviceID != "device-a" {
		t.Errorf("B sourceDeviceID = %q, want device-a", got.Message.Attributes.SourceDeviceID)
	}

	// A must NOT receive its own notification.
	connA.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if err := connA.ReadJSON(&got); err == nil {
		t.Errorf("device A unexpectedly received its own notification: %+v", got)
	}

	_ = mux
}

// pairUserTokenHTTP pairs a device against a live httptest.Server and returns
// the user token (HTTP analogue of pairUserToken which drives a *ServeMux).
func pairUserTokenHTTP(t *testing.T, srv *httptest.Server, deviceID, desc string) string {
	t.Helper()
	body := `{"code":"123456","deviceDesc":"` + desc + `","deviceID":"` + deviceID + `"}`
	resp, err := http.Post(srv.URL+"/token/json/2/device/new", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("pair %s: %v", deviceID, err)
	}
	deviceToken := readBody(t, resp)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/token/json/2/user/new", nil)
	req.Header.Set("Authorization", "Bearer "+deviceToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("user token %s: %v", deviceID, err)
	}
	return readBody(t, resp)
}

func putRootV3(t *testing.T, srv *httptest.Server, token string) {
	t.Helper()
	body := `{"generation":0,"hash":"root-hash","broadcast":true}`
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/sync/v3/root", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put root: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put root status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}

// TestNotificationsAuthRejected: no/invalid token → no upgrade.
func TestNotificationsAuthRejected(t *testing.T) {
	srv, _ := startNotifyServer(t)
	if _, resp, err := dialNotifyWS(t, srv, "", "device-x"); err == nil {
		t.Fatal("dial without token succeeded, want failure")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("no-token dial status = %d, want 401", code)
	}
	if _, resp, err := dialNotifyWS(t, srv, "bogus-token", "device-x"); err == nil {
		t.Fatal("dial with bad token succeeded, want failure")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("bad-token dial status = %d, want 401", code)
	}
}
