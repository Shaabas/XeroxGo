package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestAdsAndAdminAuth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_ads.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	// 1. Test SeedDefaultAds
	if err := s.SeedDefaultAds(ctx); err != nil {
		t.Fatalf("SeedDefaultAds failed: %v", err)
	}
	ads, err := s.GetActiveAds(ctx)
	if err != nil {
		t.Fatalf("GetActiveAds failed: %v", err)
	}
	if len(ads) != 3 {
		t.Fatalf("expected 3 default ads, got %d", len(ads))
	}
	if ads[0].Title != "Quick & Easy" {
		t.Errorf("expected first ad to be 'Quick & Easy', got %q", ads[0].Title)
	}

	// 2. Test BootstrapAdmin & AuthenticateAdmin
	if err := s.BootstrapAdmin(ctx, "admin", "secret123"); err != nil {
		t.Fatalf("BootstrapAdmin failed: %v", err)
	}
	ok, err := s.AuthenticateAdmin(ctx, "admin", "secret123")
	if err != nil || !ok {
		t.Fatalf("AuthenticateAdmin with correct password failed: ok=%v, err=%v", ok, err)
	}
	badOk, err := s.AuthenticateAdmin(ctx, "admin", "wrongpassword")
	if err != nil || badOk {
		t.Fatalf("AuthenticateAdmin with wrong password should fail: ok=%v, err=%v", badOk, err)
	}

	// 3. Test Sessions
	token, expiresAt, err := s.CreateAdminSession(ctx, "admin")
	if err != nil || token == "" {
		t.Fatalf("CreateAdminSession failed: %v", err)
	}
	if expiresAt.Before(time.Now()) {
		t.Fatalf("session already expired")
	}

	user, valid := s.ValidateAdminSession(ctx, token)
	if !valid || user != "admin" {
		t.Fatalf("ValidateAdminSession failed: valid=%v, user=%s", valid, user)
	}

	if err := s.RevokeAdminSession(ctx, token); err != nil {
		t.Fatalf("RevokeAdminSession failed: %v", err)
	}
	_, afterRevoke := s.ValidateAdminSession(ctx, token)
	if afterRevoke {
		t.Fatalf("token should be invalid after revoke")
	}

	// 4. Test SaveAd & Reorder
	newAd := contracts.AdItem{
		ID:          "ad-custom-1",
		Title:       "Special Event",
		Description: "Free printing day!",
		Type:        contracts.AdTypeGradient,
		GradientC0:  "#FF0000",
		GradientC1:  "#00FF00",
		GradientC2:  "#0000FF",
		SortOrder:   4,
		IsActive:    true,
	}
	if err := s.SaveAd(ctx, newAd); err != nil {
		t.Fatalf("SaveAd failed: %v", err)
	}

	allAds, err := s.GetAllAds(ctx)
	if err != nil {
		t.Fatalf("GetAllAds failed: %v", err)
	}
	if len(allAds) != 4 {
		t.Fatalf("expected 4 ads, got %d", len(allAds))
	}

	// Reorder
	if err := s.ReorderAds(ctx, []string{"ad-custom-1", "ad-quick-easy", "ad-high-quality", "ad-skip-queue"}); err != nil {
		t.Fatalf("ReorderAds failed: %v", err)
	}
	reordered, err := s.GetActiveAds(ctx)
	if err != nil {
		t.Fatalf("GetActiveAds failed: %v", err)
	}
	if reordered[0].ID != "ad-custom-1" {
		t.Errorf("expected first ad after reorder to be 'ad-custom-1', got %s", reordered[0].ID)
	}

	// Delete
	if err := s.DeleteAd(ctx, "ad-custom-1"); err != nil {
		t.Fatalf("DeleteAd failed: %v", err)
	}
	afterDelete, err := s.GetActiveAds(ctx)
	if err != nil {
		t.Fatalf("GetActiveAds failed: %v", err)
	}
	if len(afterDelete) != 3 {
		t.Fatalf("expected 3 ads after delete, got %d", len(afterDelete))
	}
}
