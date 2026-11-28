package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
	"xeroxgo/pkg/contracts"
)

func TestCloudSessionAdmission_NativeIPPAdmission(t *testing.T) {
	_, server, st, secret := setupPaperTestServer(t)
	kioskID := "kiosk-ipp-adm-cloud"
	now := time.Now().UTC()

	seedTestOrder(t, st, "ord_adm_idle_no", kioskID, "4101", 5, contracts.JobStatusPendingPayment, 1000, now.Add(15*time.Minute))
	seedTestOrder(t, st, "ord_adm_unk", kioskID, "4102", 5, contracts.JobStatusPendingPayment, 1000, now.Add(15*time.Minute))
	seedTestOrder(t, st, "ord_adm_print_no", kioskID, "4103", 5, contracts.JobStatusPendingPayment, 1000, now.Add(15*time.Minute))

	mkHealth := func(state string, adm contracts.AcceptingJobsStatus) contracts.PrinterHealth {
		ts := time.Now().UTC()
		return contracts.PrinterHealth{
			Online:                 true,
			State:                  state,
			EvaluatedHealth:        contracts.EvaluatedHealthHealthy,
			NativeIPPAdmission:     adm,
			PaperTrayStatus:        "OK",
			EstimatedSheets:        100,
			ConfiguredTrayCapacity: 250,
			InventoryCalibrated:    true,
			ExecutionSafe:          true,
			TonerLevelPercent:      90,
			TonerLevelKnown:        true,
			DrumLifePercent:        95,
			DrumLifeKnown:          true,
			InputTrayLevel:         100,
			InputTrayLevelKnown:    true,
			InputTrayCapacity:      250,
			InputTrayCapacityKnown: true,
			EvidenceFreshness:      contracts.BuildUniformEvidenceFreshness(ts, contracts.DefaultTelemetryMaxAge),
			UpdatedAt:              ts,
		}
	}

	conn, cancel := connectKioskWithHealth(t, server.URL, kioskID, mkHealth("IDLE", contracts.AcceptingJobsNotAccepting))
	defer func() {
		cancel()
		conn.CloseNow()
	}()

	claimOrder := func(orderID string) (*http.Response, map[string]string) {
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/k/%s/orders/%s/claim", server.URL, kioskID, orderID), nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("claim %s failed: %v", orderID, err)
		}
		var body map[string]string
		_ = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		return res, body
	}

	// 1. IDLE + NOT_ACCEPTING -> rejected with PRINTER_NOT_ACCEPTING
	res1, body1 := claimOrder("ord_adm_idle_no")
	if res1.StatusCode != http.StatusConflict || body1["code"] != "PRINTER_NOT_ACCEPTING" {
		t.Fatalf("expected 409 PRINTER_NOT_ACCEPTING, got %d %+v", res1.StatusCode, body1)
	}

	// 2. UNKNOWN -> rejected with TELEMETRY_UNVERIFIED
	ctx := context.Background()
	hUnk := mkHealth("IDLE", contracts.AcceptingJobsUnknown)
	_ = wsjson.Write(ctx, conn, wireMsg{Type: "health", Health: &hUnk})
	time.Sleep(50 * time.Millisecond)

	res2, body2 := claimOrder("ord_adm_unk")
	if res2.StatusCode != http.StatusConflict || body2["code"] != "TELEMETRY_UNVERIFIED" {
		t.Fatalf("expected 409 TELEMETRY_UNVERIFIED, got %d %+v", res2.StatusCode, body2)
	}

	// 3. PRINTING + NOT_ACCEPTING -> allowed (200 OK)
	hPrint := mkHealth("PRINTING", contracts.AcceptingJobsNotAccepting)
	_ = wsjson.Write(ctx, conn, wireMsg{Type: "health", Health: &hPrint})
	time.Sleep(50 * time.Millisecond)

	res3, body3 := claimOrder("ord_adm_print_no")
	if res3.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for PRINTING+NOT_ACCEPTING, got %d %+v", res3.StatusCode, body3)
	}
}
