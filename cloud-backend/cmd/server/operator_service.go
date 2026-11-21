package main

import (
	"crypto/hmac"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// Restore requires both authenticated kiosk access and an independent operator
// credential. Printer recovery alone never silently re-enables the kiosk.
func (s *appServer) handleRestoreKioskService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }
	if !s.verifyKioskAuth(w, r) { return }
	operatorToken := strings.TrimSpace(os.Getenv("XEROXGO_OPERATOR_TOKEN"))
	presented := strings.TrimSpace(r.Header.Get("X-XeroxGo-Operator-Token"))
	if len(operatorToken) < 16 {
		http.Error(w, "operator token not configured", http.StatusServiceUnavailable)
		return
	}
	if !hmac.Equal([]byte(operatorToken), []byte(presented)) {
		http.Error(w, "operator authorization required", http.StatusForbidden)
		return
	}
	kioskID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/k/"), "/restore-service")
	if kioskID == "" || strings.Contains(kioskID, "/") {
		http.Error(w, "invalid kiosk id", http.StatusBadRequest)
		return
	}
	if s.hub == nil || !s.hub.IsConnected(kioskID) {
		http.Error(w, "kiosk must be online before restoration", http.StatusConflict)
		return
	}
	h, present := s.hub.LatestHealth(kioskID)
	if !present || time.Since(h.UpdatedAt) > 60*time.Second ||
		!h.Online || h.PaperJam || h.CoverOpen || h.OutputBinFull ||
		h.State == "ERROR" || h.State == "OFFLINE" {
		http.Error(w, "printer health must be clean and recent", http.StatusConflict)
		return
	}
	unsafe, err := s.db.HasUnsafeRefundStops(r.Context(), kioskID)
	if err != nil { http.Error(w, "could not verify safe-stop state", http.StatusServiceUnavailable); return }
	if unsafe {
		http.Error(w, "unconfirmed stopped print jobs require manual safety reconciliation", http.StatusConflict)
		return
	}
	if err := s.db.ClearKioskBlock(r.Context(), kioskID); err != nil {
		http.Error(w, "failed to restore kiosk service", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok":true, "kiosk_id":kioskID, "service":"AVAILABLE"})
}
