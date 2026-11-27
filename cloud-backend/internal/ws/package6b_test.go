package ws

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func TestHub_ProgressAckOnlyAfterDurableCommit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := store.Open(filepath.Join(t.TempDir(), "hub6b.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	orderID := "ord-ws-6b"
	jobID := "job-ws-6b"
	kioskID := "kiosk-ws-6b"
	cmd := contracts.PrintJobCommand{
		JobID:          jobID,
		OrderID:        orderID,
		DocumentIndex:  0,
		TotalDocuments: 1,
		KioskID:        kioskID,
		DisplayCode:    "4321",
		DownloadURL:    "http://localhost/doc.pdf",
		SHA256Checksum: "deadbeef",
		TotalPages:     2,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
	if err := st.CreateOrder(ctx, store.Order{
		OrderID:      orderID,
		Command:      cmd,
		Status:       contracts.JobStatusDispatched,
		PricePaise:   500,
		TotalPages:   2,
		TotalSheets:  2,
		PINExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	hub := NewHub(st)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "?kiosk_id=" + kioskID
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// Send rejected progress (missing/unknown job) with OutcomeVersion -> must NOT receive progress_ack
	badEvt := contracts.JobProgressEvent{
		JobID:     "nonexistent-job",
		KioskID:   kioskID,
		Status:    contracts.JobStatusFailedRefundable,
		Timestamp: time.Now().UTC(),
	}
	if err := wsjson.Write(ctx, conn, wireMessage{Type: "progress", Progress: &badEvt, OutcomeVersion: 7}); err != nil {
		t.Fatalf("Write bad progress: %v", err)
	}

	// Send valid progress with OutcomeVersion -> must receive progress_ack with OutcomeVersion == 3
	goodEvt := contracts.JobProgressEvent{
		JobID:         jobID,
		KioskID:       kioskID,
		Status:        contracts.JobStatusFailedRefundable,
		PagesPrinted:  0,
		SheetsPrinted: 0,
		ErrorReason:   "download failed",
		Timestamp:     time.Now().UTC(),
	}
	if err := wsjson.Write(ctx, conn, wireMessage{Type: "progress", Progress: &goodEvt, OutcomeVersion: 3}); err != nil {
		t.Fatalf("Write good progress: %v", err)
	}

	var ack wireMessage
	for {
		var raw json.RawMessage
		if err := wsjson.Read(ctx, conn, &raw); err != nil {
			t.Fatalf("Read: %v", err)
		}
		if err := json.Unmarshal(raw, &ack); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if ack.Type == "progress_ack" {
			break
		}
	}

	if !ack.OK || ack.OutcomeVersion != 3 || ack.Progress == nil || ack.Progress.JobID != jobID {
		t.Fatalf("unexpected progress_ack: %+v", ack)
	}
}
