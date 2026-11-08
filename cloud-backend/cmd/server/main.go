package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xeroxgo/cloud-backend/internal/payments"
	"xeroxgo/cloud-backend/internal/pdf"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

//go:embed static
var staticEmbedFS embed.FS


type serverConfig struct {
	addr               string
	dbPath             string
	jobsDir            string
	pricePerSheetPaise int64
	devMode            bool
	downloadSecret     []byte
	webhookSecret      []byte
}

type appServer struct {
	cfg      *serverConfig
	db       *store.Store
	hub      *ws.Hub
	convWake chan struct{}
}

const portalHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0, maximum-scale=1.0, user-scalable=no" />
  <title>XeroxGO - Campus Print Portal</title>
  <meta name="description" content="XeroxGO - Instant campus print portal. Upload, customize, and print documents directly from your phone." />

  <!-- PWA & Mobile Web App Meta -->
  <meta name="theme-color" content="#2563EB" />
  <meta name="mobile-web-app-capable" content="yes" />
  <meta name="apple-mobile-web-app-capable" content="yes" />
  <meta name="apple-mobile-web-app-status-bar-style" content="default" />
  <meta name="apple-mobile-web-app-title" content="XeroxGO" />

  <!-- Icons & Manifest -->
  <link rel="manifest" href="/manifest.webmanifest" />
  <link rel="icon" type="image/svg+xml" href="/static/assets/favicon.svg" />
  <link rel="icon" type="image/png" sizes="48x48" href="/static/assets/favicon-48x48.png" />
  <link rel="icon" type="image/png" sizes="32x32" href="/static/assets/favicon-32x32.png" />
  <link rel="apple-touch-icon" href="/static/assets/apple-touch-icon.png" />

  <link rel="stylesheet" href="/static/css/portal.css?v=2.0.0" />
  <script src="/static/vendor/pdf-lib.min.js"></script>
  <script type="module">
    import * as pdfjsLib from '/static/vendor/pdf.min.mjs';
    window.pdfjsLib = pdfjsLib;
    try {
      const workerUrl = new URL('/static/vendor/pdf.worker.min.mjs', window.location.href).href;
      pdfjsLib.GlobalWorkerOptions.workerSrc = workerUrl;
    } catch (_) {
      pdfjsLib.GlobalWorkerOptions.workerSrc = '/static/vendor/pdf.worker.min.mjs';
    }
    window.dispatchEvent(new CustomEvent('pdfjs-ready', { detail: pdfjsLib }));
  </script>
  <script src="/static/js/portal.js?v=2.0.0" defer></script>
  <script>
    (function() {
      var match = window.location.pathname.match(/\/k\/([^/]+)/);
      var params = new URLSearchParams(window.location.search);
      var kiosk = (match && match[1]) || params.get('kiosk_id') || 'kiosk-demo';
      window.XEROXGO_KIOSK_ID = kiosk;
      document.addEventListener('DOMContentLoaded', function() {
        var el = document.getElementById('app-container');
        if (el) el.dataset.kioskId = kiosk;
      });
    })();
  </script>
</head>
<body>
  <div class="app-container" id="app-container" data-kiosk-id="{{.KioskID}}" data-dev-mode="{{.DevMode}}" data-price-per-sheet-paise="{{.PricePerSheetPaise}}">

    <!-- Top Floating PWA Install Banner -->
    <div id="pwa-install-banner" class="pwa-install-banner animate-slide-down hidden">
      <div class="pwa-banner-left">
        <div class="pwa-icon-wrapper">
          <img src="/static/assets/xeroxgo-logo-transparent.png" alt="XeroxGO" class="pwa-banner-icon" />
        </div>
        <div class="pwa-banner-text">
          <div class="pwa-banner-title">Install XeroxGO App</div>
          <div class="pwa-banner-desc">Fast, full-screen printing right from your phone</div>
        </div>
      </div>
      <div class="pwa-banner-actions">
        <button id="pwa-install-btn" class="pwa-install-btn" type="button" title="Install App">
          <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" style="margin-right:4px;"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>
          Install
        </button>
        <button id="pwa-dismiss-btn" class="pwa-dismiss-btn" type="button" aria-label="Dismiss">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
        </button>
      </div>
    </div>

    <!-- Mini Install Pill (shown if dismissed) -->
    <div id="pwa-mini-pill" class="hidden" style="display:flex; justify-content:center; margin:0.5rem 0;">
      <button id="pwa-pill-btn" type="button" class="pwa-header-pill-btn" style="box-shadow:0 2px 6px rgba(37,99,235,0.08);">
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><rect width="14" height="20" x="5" y="2" rx="2" ry="2"/><line x1="12" y1="18" x2="12.01" y2="18"/></svg>
        Install XeroxGO App
      </button>
    </div>

    <!-- ================================================================= -->
    <!-- VIEW 1: FILES CART SCREEN                                         -->
    <!-- ================================================================= -->
    <main id="view-cart" class="portal-view active-view">
      <!-- Header -->
      <header class="portal-header">
        <div class="logo-box">
          <img
            src="/static/assets/xeroxgo-full-logo-transparent.png"
            alt="XeroxGO"
            class="header-logo-img"
          />
        </div>
        <p class="header-tagline">Upload, Print, Collect - It's that simple!</p>
      </header>

      <!-- Promo Banner Carousel -->
      <div id="promo-banner" class="banner-container">
        <div id="banner-track" class="banner-track">
          <div class="banner-slide" style="background: linear-gradient(135deg, #1E3A8A 0%, #2563EB 50%, #4F46E5 100%);">
            <div class="banner-title">Quick &amp; Easy</div>
            <div class="banner-desc">Direct UPI payment. No coins, no cash needed.</div>
          </div>
          <div class="banner-slide" style="background: linear-gradient(135deg, #065F46 0%, #10B981 50%, #059669 100%);">
            <div class="banner-title">High Quality Prints</div>
            <div class="banner-desc">Crisp laser documents ready in seconds.</div>
          </div>
          <div class="banner-slide" style="background: linear-gradient(135deg, #581C87 0%, #7C3AED 50%, #D97706 100%);">
            <div class="banner-title">Skip The Queue</div>
            <div class="banner-desc">Pick up your print job with your 4-digit code.</div>
          </div>
        </div>
        <button id="banner-close-btn" class="banner-close" type="button" aria-label="Close banner">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
        </button>
        <div id="banner-dots" class="banner-indicators">
          <div class="indicator active"></div>
          <div class="indicator"></div>
          <div class="indicator"></div>
        </div>
      </div>

      <!-- Add Documents Dropzone Card -->
      <div class="cart-upload-card" id="cart-upload-card">
        <div class="upload-cloud-icon">
          <svg width="42" height="42" viewBox="0 0 24 24" fill="none" stroke="#2563EB" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <path d="M17.5 19H9a7 7 0 1 1 6.71-9h1.79a4.5 4.5 0 1 1 0 9Z"/>
            <polyline points="16 13 12 9 8 13"/>
            <line x1="12" y1="9" x2="12" y2="21"/>
          </svg>
        </div>
        <h3 class="upload-heading">Add documents</h3>
        <p class="upload-subheading">Select multiple PDFs, Office files or images</p>
        <button type="button" class="btn-choose-files" id="btn-choose-files">+ Choose files</button>
        <input type="file" id="file-input" multiple accept=".pdf,.doc,.docx,.ppt,.pptx,.xls,.xlsx,.odt,.ods,.odp,.rtf,.txt,.jpg,.jpeg,.png,.webp,.bmp" style="display:none;" />
        
        <!-- Upload Processing Spinner -->
        <div id="upload-processing-indicator" class="upload-processing-box hidden">
          <div class="spinner-icon"></div>
          <span id="upload-processing-text" class="processing-text">Processing document...</span>
        </div>

        <!-- Cart Alert -->
        <div id="cart-alert" class="alert alert-error hidden"></div>
      </div>

      <!-- Document Cards List -->
      <div id="cart-doc-list" class="cart-doc-list">
        <!-- Rendered dynamically via JavaScript -->
      </div>

      <!-- Empty Cart Message (when no documents added yet) -->
      <div id="cart-empty-placeholder" class="cart-empty-state">
        <p>No documents added yet. Select one or more files to start printing.</p>
      </div>

      <!-- Sticky Bottom Total Bar -->
      <div id="cart-bottom-bar" class="cart-bottom-bar hidden">
        <div class="cart-total-info">
          <span class="cart-total-label">Estimated total &middot; <span id="cart-docs-count">0 documents</span></span>
          <span class="cart-total-amount" id="cart-total-amount">&#8377;0.00</span>
        </div>
        <button type="button" id="btn-review-order" class="btn-review-order">
          <span>Review Order</span>
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="5" y1="12" x2="19" y2="12"/><polyline points="12 5 19 12 12 19"/></svg>
        </button>
      </div>
    </main>

    <!-- ================================================================= -->
    <!-- VIEW 2: DOCUMENT SETTINGS SCREEN                                  -->
    <!-- ================================================================= -->
    <section id="view-settings" class="portal-view hidden">
      <!-- Navigation Bar -->
      <div class="view-nav-bar">
        <button type="button" id="btn-settings-back" class="btn-nav-back" aria-label="Back to Cart">
          <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="15 18 9 12 15 6"/></svg>
        </button>
        <div class="view-nav-title-box">
          <h2 class="view-nav-title" id="settings-filename">Document.pdf</h2>
          <p class="view-nav-subtitle">Document settings</p>
        </div>
        <div class="view-nav-actions">
          <button type="button" id="btn-settings-menu" class="btn-nav-menu" aria-label="Document options">
            <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="1.5"/><circle cx="6" cy="12" r="1.5"/><circle cx="18" cy="12" r="1.5"/></svg>
          </button>
          <div id="settings-menu-dropdown" class="nav-dropdown-menu hidden">
            <button type="button" id="menu-opt-reset" class="dropdown-item">Reset to default</button>
            <button type="button" id="menu-opt-delete" class="dropdown-item dropdown-item-danger">Delete document</button>
          </div>
        </div>
      </div>

      <!-- Settings Form Card -->
      <div class="settings-card">
        <!-- 1. Page Selection -->
        <div class="settings-field-group">
          <label class="settings-field-label">Page Selection</label>
          <div class="segmented-control">
            <button type="button" class="segmented-opt active" id="btn-opt-all-pages">All Pages</button>
            <button type="button" class="segmented-opt" id="btn-opt-custom-range">Custom Range</button>
          </div>
          <div id="settings-custom-range-wrapper" class="custom-range-wrapper hidden">
            <input type="text" id="input-custom-range" class="custom-range-input" placeholder="e.g. 1-3, 5" autocomplete="off" />
            <p class="custom-range-hint" id="custom-range-hint">Enter pages to print (e.g. 1-3, 5)</p>
            <p class="custom-range-error hidden" id="custom-range-error"></p>
          </div>
        </div>

        <!-- 2. Number of Copies -->
        <div class="settings-field-group">
          <label class="settings-field-label">Number of Copies</label>
          <div class="copies-stepper">
            <button type="button" id="btn-copies-minus" class="stepper-btn" aria-label="Decrease copies">&minus;</button>
            <span id="copies-count-display" class="stepper-value">1</span>
            <button type="button" id="btn-copies-plus" class="stepper-btn" aria-label="Increase copies">&plus;</button>
          </div>
        </div>

        <!-- 3. Print Mode -->
        <div class="settings-field-group">
          <label class="settings-field-label">Print Mode</label>
          <div class="choice-cards-row">
            <div class="choice-card active" id="card-mode-bw">
              <div class="choice-icon-circle bw-circle"></div>
              <div class="choice-card-title">B&amp;W</div>
              <div class="choice-card-sub" id="settings-bw-price-label">&#8377;2.00/side</div>
            </div>
            <div class="choice-card" id="card-mode-color">
              <div class="choice-icon-circle color-circle"></div>
              <div class="choice-card-title">Color</div>
              <div class="choice-card-sub" id="settings-color-price-label">&#8377;10.00/side</div>
            </div>
          </div>
        </div>

        <!-- 4. Print Sides -->
        <div class="settings-field-group">
          <label class="settings-field-label">Print Sides</label>
          <div class="choice-cards-row">
            <div class="choice-card active" id="card-side-single">
              <div class="sheet-icon-single"></div>
              <div class="choice-card-title">Single Side</div>
            </div>
            <div class="choice-card" id="card-side-double">
              <div class="sheet-icon-double">
                <div class="sheet-under"></div>
                <div class="sheet-over"></div>
              </div>
              <div class="choice-card-title">Double Side</div>
              <div class="save-paper-badge">Save paper!</div>
            </div>
          </div>
        </div>

        <!-- 5. Orientation -->
        <div class="settings-field-group">
          <label class="settings-field-label">Orientation</label>
          <div class="segmented-control three-segment">
            <button type="button" class="segmented-opt active" id="btn-orient-auto">Auto</button>
            <button type="button" class="segmented-opt" id="btn-orient-portrait">Portrait</button>
            <button type="button" class="segmented-opt" id="btn-orient-landscape">Landscape</button>
          </div>
        </div>

        <!-- 6. Pages Per Sheet -->
        <div class="settings-field-group">
          <label class="settings-field-label">Pages Per Sheet</label>
          <div class="choice-cards-row">
            <div class="choice-card active" id="card-nup-1">
              <div class="choice-card-title">1 page/sheet</div>
              <div class="choice-card-sub">Standard A4 layout</div>
            </div>
            <div class="choice-card disabled" id="card-nup-2" title="2 pages per sheet is not yet available">
              <div class="choice-card-title">2 pages/sheet</div>
              <div class="choice-card-sub">Not yet available</div>
            </div>
          </div>
        </div>

        <p class="settings-footnote">Selected settings apply to this document only.</p>
      </div>

      <!-- Save Settings Sticky Button -->
      <div class="settings-bottom-bar">
        <button type="button" id="btn-save-doc-settings" class="btn-save-settings">
          Save Settings &middot; <span id="save-settings-price">&#8377;0.00</span>
        </button>
      </div>
    </section>

    <!-- ================================================================= -->
    <!-- VIEW 3: PRINT PREVIEW SCREEN                                      -->
    <!-- ================================================================= -->
    <section id="view-preview" class="portal-view hidden">
      <!-- Navigation Bar -->
      <div class="view-nav-bar">
        <button type="button" id="btn-preview-back" class="btn-nav-back" aria-label="Back to Cart">
          <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="15 18 9 12 15 6"/></svg>
        </button>
        <div class="view-nav-title-box">
          <h2 class="view-nav-title" id="preview-filename">Document.pdf</h2>
          <p class="view-nav-subtitle">Print Preview</p>
        </div>
        <div class="view-nav-actions">
          <button type="button" id="btn-preview-menu" class="btn-nav-menu" aria-label="Options">
            <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="1.5"/><circle cx="6" cy="12" r="1.5"/><circle cx="18" cy="12" r="1.5"/></svg>
          </button>
          <div id="preview-menu-dropdown" class="nav-dropdown-menu hidden">
            <button type="button" id="preview-menu-settings" class="dropdown-item">Edit settings</button>
          </div>
        </div>
      </div>

      <!-- Preview Frame -->
      <div class="preview-frame-card">
        <div class="preview-paper-stage">
          <div id="preview-loading-spinner" class="preview-spinner hidden">
            <div class="spinner-icon"></div>
            <span>Rendering preview...</span>
          </div>
          <div id="preview-error-box" class="preview-error-box hidden">
            <p>Preview unavailable</p>
          </div>
          <canvas id="preview-canvas" class="preview-canvas"></canvas>
        </div>

        <!-- Sheet Pager -->
        <div class="preview-sheet-pager">
          <button type="button" id="btn-sheet-left" class="sheet-arrow-btn" aria-label="Previous sheet" disabled>
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="15 18 9 12 15 6"/></svg>
          </button>
          <span id="preview-sheet-indicator" class="preview-sheet-indicator">Sheet 1 of 1 (Front)</span>
          <button type="button" id="btn-sheet-right" class="sheet-arrow-btn" aria-label="Next sheet" disabled>
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="9 18 15 12 9 6"/></svg>
          </button>
        </div>
      </div>

      <!-- Flip Sheet Action -->
      <div class="preview-action-row">
        <button type="button" id="btn-sheet-flip" class="btn-flip-sheet">Flip Sheet</button>
      </div>
    </section>

    <!-- ================================================================= -->
    <!-- VIEW 4: CHECKOUT / PIN SCREEN                                     -->
    <!-- ================================================================= -->
    <section id="view-checkout" class="portal-view hidden">
      <!-- Header -->
      <header class="portal-header">
        <div class="logo-box">
          <img
            src="/static/assets/xeroxgo-full-logo-transparent.png"
            alt="XeroxGO"
            class="header-logo-img"
          />
        </div>
        <p class="header-tagline">Upload, Print, Collect - It's that simple!</p>
      </header>

      <!-- Ready Card -->
      <div class="checkout-ready-card">
        <div class="checkout-check-circle">
          <svg width="26" height="26" viewBox="0 0 24 24" fill="none" stroke="#16a34a" stroke-width="3" stroke-linecap="round" stroke-linejoin="round">
            <polyline points="20 6 9 17 4 12"/>
          </svg>
        </div>
        <h2 class="checkout-ready-title">Your order is ready!</h2>
        <p class="checkout-ready-subtitle">Enter this PIN at the XeroxGo kiosk to continue.</p>

        <!-- 4 Large PIN Digit Boxes -->
        <div class="pin-digit-boxes" id="pin-digit-boxes">
          <div class="pin-digit-box" id="pin-box-0">-</div>
          <div class="pin-digit-box" id="pin-box-1">-</div>
          <div class="pin-digit-box" id="pin-box-2">-</div>
          <div class="pin-digit-box" id="pin-box-3">-</div>
        </div>

        <div class="pin-expiry-text" id="pin-expiry-text">Expires in 15 minutes</div>

        <button type="button" id="btn-copy-pin" class="btn-copy-pin">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <rect x="9" y="9" width="13" height="13" rx="2" ry="2"/>
            <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/>
          </svg>
          <span id="btn-copy-pin-label">Copy PIN</span>
        </button>
      </div>

      <!-- Order Summary Card -->
      <div class="checkout-summary-card">
        <div class="checkout-summary-row">
          <span class="summary-label">Documents</span>
          <span class="summary-value" id="checkout-docs-count">0 files</span>
        </div>
        <div class="checkout-summary-row total-row">
          <span class="summary-label">Total</span>
          <span class="summary-value total-price" id="checkout-total-amount">&#8377;0.00</span>
        </div>
      </div>

      <!-- Kiosk Notice -->
      <p class="checkout-footer-notice">
        Payment will be completed at the kiosk. Keep this page open or copy your PIN.
      </p>

      <!-- Live Job Progress / Status (Spooling, Printing, Completed) -->
      <div id="checkout-live-status-box" class="checkout-live-status hidden">
        <div class="live-status-header">
          <span class="live-status-label">Kiosk Progress</span>
          <span id="checkout-status-pill" class="status-pill status-PENDING_PAYMENT">PENDING_PAYMENT</span>
        </div>
        <div class="progress-bar-bg" style="margin-top:0.5rem;">
          <div id="checkout-progress-bar" class="progress-bar-fill" style="width:0%;"></div>
        </div>
        <div id="checkout-status-msg" class="checkout-status-msg"></div>
      </div>

      <!-- Action to Start New Order -->
      <div style="text-align:center; margin-top:1.5rem; margin-bottom:2rem;">
        <button type="button" id="btn-start-new-order" class="btn-new-order">Print More Documents</button>
      </div>
    </section>

    <!-- PWA Install Guide Modal (iOS/Android manual) -->
    <div id="pwa-guide-modal" class="modal-overlay hidden">
      <div class="pwa-ios-modal animate-scale-up" onclick="event.stopPropagation();">
        <div class="pwa-ios-modal-header">
          <div style="display:flex; align-items:center; gap:8px;">
            <img src="/static/assets/xeroxgo-logo-transparent.png" alt="XeroxGO" style="height:26px; width:auto;" />
            <h3 style="font-size:1.15rem; font-weight:700; margin:0; color:var(--text-primary);">Install XeroxGO App</h3>
          </div>
          <button id="pwa-modal-close-btn" class="remove-btn" type="button" aria-label="Close modal">
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
          </button>
        </div>
        <p style="font-size:0.88rem; color:#475569; margin-bottom:1.25rem; line-height:1.4;">
          Install XeroxGO on your phone for instant 1-tap printing:
        </p>
        <div class="pwa-ios-steps">
          <div class="pwa-ios-step">
            <div class="pwa-step-number">1</div>
            <div class="pwa-step-content">
              <span>Tap the browser <strong>Share</strong> icon (iOS) or <strong>Menu (&vellip;)</strong> (Android)</span>
            </div>
          </div>
          <div class="pwa-ios-step">
            <div class="pwa-step-number">2</div>
            <div class="pwa-step-content">
              <span>Select <strong>"Add to Home Screen"</strong> or <strong>"Install App"</strong></span>
            </div>
          </div>
          <div class="pwa-ios-step">
            <div class="pwa-step-number">3</div>
            <div class="pwa-step-content">
              <span>Confirm <strong>"Add"</strong> to launch XeroxGO in full-screen anytime</span>
            </div>
          </div>
        </div>
        <div style="margin-top:1.5rem;">
          <button id="pwa-modal-gotit-btn" type="button" class="btn btn-primary" style="width:100%; padding:0.75rem; font-weight:700;">
            Got it, Thanks!
          </button>
        </div>
      </div>
    </div>

  </div>
</body>
</html>
`

func randomHex(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomDisplayCode() (string, error) {
	const charset = "0123456789"
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b), nil
}

func generateSignedDownloadURL(baseURL, orderID string, secret []byte, ttl time.Duration) string {
	expires := time.Now().Add(ttl).Unix()
	msg := fmt.Sprintf("%s\n%d", orderID, expires)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s/api/orders/%s/pdf?expires=%d&sig=%s", baseURL, orderID, expires, sig)
}

func verifySignedDownloadURL(orderID string, expires int64, sig string, secret []byte) bool {
	if time.Now().Unix() > expires {
		return false
	}
	msg := fmt.Sprintf("%s\n%d", orderID, expires)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(strings.ToLower(sig)), []byte(strings.ToLower(expected)))
}

func (s *appServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *appServer) handlePortal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	prefix := "/k/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	kioskID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, prefix))
	if kioskID == "" {
		http.Error(w, "kiosk id required", http.StatusBadRequest)
		return
	}

	tmpl, err := template.New("portal").Parse(portalHTML)
	if err != nil {
		http.Error(w, "failed to render portal", http.StatusInternalServerError)
		return
	}

	data := struct {
		KioskID            string
		DevMode            bool
		PricePerSheetPaise int64
	}{
		KioskID:            kioskID,
		DevMode:            s.cfg.devMode,
		PricePerSheetPaise: s.cfg.pricePerSheetPaise,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.Execute(w, data)
}

func (s *appServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	prefix := "/api/k/"
	suffix := "/upload"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}
	kioskID := strings.TrimPrefix(r.URL.Path, prefix)
	kioskID = strings.TrimSuffix(kioskID, suffix)
	kioskID = strings.TrimSpace(kioskID)
	if kioskID == "" {
		http.Error(w, "kiosk id required", http.StatusBadRequest)
		return
	}

	const maxUploadSize = 50 * 1024 * 1024 // 50 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "request exceeds 50 MiB limit or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	var fileHeaders []*multipart.FileHeader
	if r.MultipartForm != nil {
		if files, ok := r.MultipartForm.File["files"]; ok && len(files) > 0 {
			fileHeaders = files
		} else if fileList, ok := r.MultipartForm.File["file"]; ok && len(fileList) > 0 {
			fileHeaders = fileList
		}
	}
	if len(fileHeaders) == 0 {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}

	if len(fileHeaders) > 1 {
		type docItemMeta struct {
			Copies    int    `json:"copies"`
			Duplex    string `json:"duplex"`
			ColorMode string `json:"color_mode"`
			PageRange string `json:"page_range"`
		}
		var metaItems []docItemMeta
		if mStr := r.FormValue("metadata"); strings.TrimSpace(mStr) != "" {
			_ = json.Unmarshal([]byte(mStr), &metaItems)
		}

		pricing, err := s.db.GetPricing(r.Context())
		if err != nil {
			pricing = store.PricingConfig{
				BWSimplexPricePaise:    200,
				BWDuplexPricePaise:     150,
				ColorSimplexPricePaise: 1000,
				ColorDuplexPricePaise:  750,
				ColorEnabled:           false,
			}
		}

		orderHex, err := randomHex(12)
		if err != nil {
			http.Error(w, "failed to generate order id", http.StatusInternalServerError)
			return
		}
		orderID := "ord_" + orderHex

		jobHex, err := randomHex(12)
		if err != nil {
			http.Error(w, "failed to generate job id", http.StatusInternalServerError)
			return
		}
		jobID := "job_" + jobHex

		displayCode, err := randomDisplayCode()
		if err != nil {
			http.Error(w, "failed to generate display code", http.StatusInternalServerError)
			return
		}

		h := sha256.Sum256([]byte(orderID))
		outputPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(h[:])+".pdf")

		var intermediateNormPDFs []string
		defer func() {
			for _, p := range intermediateNormPDFs {
				_ = os.Remove(p)
			}
		}()

		var totalOrderPricePaise int64
		var totalPrintedPages int
		var totalSheets int
		anyDuplex := false
		anyColor := false

		for idx, fh := range fileHeaders {
			originalFilename := fh.Filename
			if !pdf.IsSupportedFormat(originalFilename) {
				_ = os.Remove(outputPath)
				http.Error(w, fmt.Sprintf("unsupported file format %q in document %d", filepath.Ext(originalFilename), idx+1), http.StatusBadRequest)
				return
			}

			f, err := fh.Open()
			if err != nil {
				_ = os.Remove(outputPath)
				http.Error(w, fmt.Sprintf("failed to open uploaded file %q: %v", originalFilename, err), http.StatusBadRequest)
				return
			}

			ext := strings.ToLower(filepath.Ext(originalFilename))
			if ext == "" {
				ext = ".pdf"
			}
			tmpInput, err := os.CreateTemp(s.cfg.jobsDir, "upload_in_*"+ext)
			if err != nil {
				_ = f.Close()
				_ = os.Remove(outputPath)
				http.Error(w, "failed to create temp file", http.StatusInternalServerError)
				return
			}
			tmpInputName := tmpInput.Name()
			_, copyErr := io.Copy(tmpInput, f)
			_ = f.Close()
			_ = tmpInput.Close()
			if copyErr != nil {
				_ = os.Remove(tmpInputName)
				_ = os.Remove(outputPath)
				http.Error(w, "failed to save uploaded file", http.StatusInternalServerError)
				return
			}

			normalizedInputPath := tmpInputName
			if ext != ".pdf" {
				tmpConverted, err := os.CreateTemp(s.cfg.jobsDir, "upload_conv_*.pdf")
				if err != nil {
					_ = os.Remove(tmpInputName)
					_ = os.Remove(outputPath)
					http.Error(w, "failed to create temp file for conversion", http.StatusInternalServerError)
					return
				}
				tmpConvertedName := tmpConverted.Name()
				_ = tmpConverted.Close()

				if err := pdf.ConvertToPDF(r.Context(), tmpInputName, originalFilename, tmpConvertedName); err != nil {
					_ = os.Remove(tmpInputName)
					_ = os.Remove(tmpConvertedName)
					_ = os.Remove(outputPath)
					http.Error(w, fmt.Sprintf("conversion failed for %q: %v", originalFilename, err), http.StatusBadRequest)
					return
				}
				_ = os.Remove(tmpInputName)
				normalizedInputPath = tmpConvertedName
			}
			defer func(path string) { _ = os.Remove(path) }(normalizedInputPath)

			docCopies := 1
			docDuplexMode := contracts.DuplexSimplex
			docColorMode := "BW"
			docPageRange := ""

			if idx < len(metaItems) {
				if metaItems[idx].Copies >= 1 && metaItems[idx].Copies <= 999 {
					docCopies = metaItems[idx].Copies
				}
				switch metaItems[idx].Duplex {
				case string(contracts.DuplexLongEdge):
					docDuplexMode = contracts.DuplexLongEdge
				case string(contracts.DuplexShortEdge):
					docDuplexMode = contracts.DuplexShortEdge
				default:
					docDuplexMode = contracts.DuplexSimplex
				}
				if strings.EqualFold(metaItems[idx].ColorMode, "COLOR") && pricing.ColorEnabled {
					docColorMode = "COLOR"
				}
				docPageRange = strings.TrimSpace(metaItems[idx].PageRange)
			} else {
				if c, err := strconv.Atoi(r.FormValue("copies")); err == nil && c >= 1 && c <= 999 {
					docCopies = c
				}
				switch r.FormValue("duplex") {
				case string(contracts.DuplexLongEdge):
					docDuplexMode = contracts.DuplexLongEdge
				case string(contracts.DuplexShortEdge):
					docDuplexMode = contracts.DuplexShortEdge
				default:
					docDuplexMode = contracts.DuplexSimplex
				}
				if strings.EqualFold(r.FormValue("color_mode"), "COLOR") && pricing.ColorEnabled {
					docColorMode = "COLOR"
				}
				docPageRange = strings.TrimSpace(r.FormValue("page_range"))
			}

			if docDuplexMode != contracts.DuplexSimplex {
				anyDuplex = true
			}
			if docColorMode == "COLOR" {
				anyColor = true
			}

			tmpNorm, err := os.CreateTemp(s.cfg.jobsDir, "upload_norm_*.pdf")
			if err != nil {
				_ = os.Remove(outputPath)
				http.Error(w, "failed to create intermediate file", http.StatusInternalServerError)
				return
			}
			tmpNormPath := tmpNorm.Name()
			_ = tmpNorm.Close()
			intermediateNormPDFs = append(intermediateNormPDFs, tmpNormPath)

			normRes, err := pdf.NormalizeAndCountWithOptions(r.Context(), normalizedInputPath, tmpNormPath, pdf.NormalizeOptions{
				Copies:    docCopies,
				Duplex:    docDuplexMode,
				ColorMode: docColorMode,
				PageRange: docPageRange,
			})
			if err != nil {
				_ = os.Remove(outputPath)
				http.Error(w, fmt.Sprintf("normalization failed for %q: %v", originalFilename, err), http.StatusBadRequest)
				return
			}

			var unitPrice int64
			isDuplex := docDuplexMode != contracts.DuplexSimplex
			if docColorMode == "COLOR" {
				if isDuplex {
					unitPrice = pricing.ColorDuplexPricePaise
				} else {
					unitPrice = pricing.ColorSimplexPricePaise
				}
			} else {
				if isDuplex {
					unitPrice = pricing.BWDuplexPricePaise
				} else {
					unitPrice = pricing.BWSimplexPricePaise
				}
			}
			if unitPrice <= 0 {
				unitPrice = s.cfg.pricePerSheetPaise
			}
			if unitPrice <= 0 {
				unitPrice = 200
			}

			docPrintedSides := normRes.TotalPages * docCopies
			docPrice, err := payments.CalculatePricePaise(docPrintedSides, unitPrice)
			if err != nil {
				_ = os.Remove(outputPath)
				http.Error(w, "failed to calculate price", http.StatusInternalServerError)
				return
			}

			totalOrderPricePaise += docPrice
			totalPrintedPages += docPrintedSides
			totalSheets += normRes.TotalSheets
		}

		qpdfArgs := []string{"--empty", "--pages"}
		for _, p := range intermediateNormPDFs {
			qpdfArgs = append(qpdfArgs, p, "1-z")
		}
		qpdfArgs = append(qpdfArgs, "--", outputPath)

		cmdMerge := exec.CommandContext(r.Context(), "qpdf", qpdfArgs...)
		outMerge, errMerge := cmdMerge.CombinedOutput()
		if errMerge != nil {
			_ = os.Remove(outputPath)
			http.Error(w, fmt.Sprintf("failed to merge documents: %s: %v", strings.TrimSpace(string(outMerge)), errMerge), http.StatusInternalServerError)
			return
		}

		finalBytes, err := os.ReadFile(outputPath)
		if err != nil {
			_ = os.Remove(outputPath)
			http.Error(w, "failed to read merged document", http.StatusInternalServerError)
			return
		}
		checksumBytes := sha256.Sum256(finalBytes)
		sha256Checksum := hex.EncodeToString(checksumBytes[:])

		baseURL := os.Getenv("XEROXGO_PUBLIC_URL")
		if baseURL == "" {
			scheme := "http"
			if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
				scheme = "https"
			}
			baseURL = fmt.Sprintf("%s://%s", scheme, r.Host)
		}
		baseURL = strings.TrimRight(baseURL, "/")
		downloadURL := generateSignedDownloadURL(baseURL, orderID, s.cfg.downloadSecret, 24*time.Hour)

		overallDuplex := contracts.DuplexSimplex
		if anyDuplex {
			overallDuplex = contracts.DuplexLongEdge
		}
		overallColor := "BW"
		if anyColor {
			overallColor = "COLOR"
		}

		cmd := contracts.PrintJobCommand{
			JobID:          jobID,
			KioskID:        kioskID,
			DisplayCode:    displayCode,
			DownloadURL:    downloadURL,
			SHA256Checksum: sha256Checksum,
			TotalPages:     totalPrintedPages,
			TotalSheets:    totalSheets,
			Copies:         1,
			Duplex:         overallDuplex,
			StartFromPage:  1,
		}

		order := store.Order{
			OrderID:    orderID,
			Command:    cmd,
			Status:     contracts.JobStatusPendingPayment,
			PricePaise: totalOrderPricePaise,
			ColorMode:  overallColor,
		}

		if err := s.db.CreateOrder(r.Context(), order); err != nil {
			_ = os.Remove(outputPath)
			http.Error(w, "failed to persist order", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id":              orderID,
			"job_id":                jobID,
			"display_code":          displayCode,
			"color_mode":            overallColor,
			"total_pages":           totalPrintedPages,
			"total_sheets":          totalSheets,
			"copies":                1,
			"duplex":                string(overallDuplex),
			"documents_count":       len(fileHeaders),
			"total_price_paise":     totalOrderPricePaise,
			"status":                string(contracts.JobStatusPendingPayment),
		})
		return
	}

	fileHeader := fileHeaders[0]
	file, err := fileHeader.Open()
	if err != nil {
		http.Error(w, "failed to open file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	originalFilename := fileHeader.Filename
	if !pdf.IsSupportedFormat(originalFilename) {
		http.Error(w, fmt.Sprintf("unsupported file format %q: only PDF, Office documents (DOCX, PPTX, XLSX), and Images (JPG, PNG) are supported", filepath.Ext(originalFilename)), http.StatusBadRequest)
		return
	}

	copiesStr := r.FormValue("copies")
	copies := 1
	if copiesStr != "" {
		c, err := strconv.Atoi(copiesStr)
		if err != nil || c < 1 || c > 999 {
			http.Error(w, "copies must be between 1 and 999", http.StatusBadRequest)
			return
		}
		copies = c
	}

	duplexStr := r.FormValue("duplex")
	var duplexMode contracts.DuplexMode
	switch duplexStr {
	case string(contracts.DuplexSimplex):
		duplexMode = contracts.DuplexSimplex
	case string(contracts.DuplexLongEdge):
		duplexMode = contracts.DuplexLongEdge
	case string(contracts.DuplexShortEdge):
		duplexMode = contracts.DuplexShortEdge
	default:
		http.Error(w, "invalid duplex mode", http.StatusBadRequest)
		return
	}

	ext := strings.ToLower(filepath.Ext(originalFilename))
	if ext == "" {
		ext = ".pdf"
	}
	tmpInput, err := os.CreateTemp(s.cfg.jobsDir, "upload_in_*"+ext)
	if err != nil {
		http.Error(w, "failed to create temp file", http.StatusInternalServerError)
		return
	}
	tmpInputName := tmpInput.Name()
	defer func() {
		_ = tmpInput.Close()
		_ = os.Remove(tmpInputName)
	}()

	if _, err := io.Copy(tmpInput, file); err != nil {
		http.Error(w, "failed to save uploaded file", http.StatusInternalServerError)
		return
	}
	_ = tmpInput.Close()

	// Convert to PDF if non-PDF document or image
	normalizedInputPath := tmpInputName
	if ext != ".pdf" {
		tmpConverted, err := os.CreateTemp(s.cfg.jobsDir, "upload_conv_*.pdf")
		if err != nil {
			http.Error(w, "failed to create temp file for conversion", http.StatusInternalServerError)
			return
		}
		tmpConvertedName := tmpConverted.Name()
		defer func() {
			_ = tmpConverted.Close()
			_ = os.Remove(tmpConvertedName)
		}()
		_ = tmpConverted.Close()

		if err := pdf.ConvertToPDF(r.Context(), tmpInputName, originalFilename, tmpConvertedName); err != nil {
			http.Error(w, fmt.Sprintf("document conversion failed: %v", err), http.StatusBadRequest)
			return
		}
		normalizedInputPath = tmpConvertedName
	}

	orderHex, err := randomHex(12)
	if err != nil {
		http.Error(w, "failed to generate order id", http.StatusInternalServerError)
		return
	}
	orderID := "ord_" + orderHex

	jobHex, err := randomHex(12)
	if err != nil {
		http.Error(w, "failed to generate job id", http.StatusInternalServerError)
		return
	}
	jobID := "job_" + jobHex

	displayCode, err := randomDisplayCode()
	if err != nil {
		http.Error(w, "failed to generate display code", http.StatusInternalServerError)
		return
	}

	h := sha256.Sum256([]byte(orderID))
	outputPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(h[:])+".pdf")

	colorMode := strings.ToUpper(strings.TrimSpace(r.FormValue("color_mode")))
	if colorMode != "COLOR" {
		colorMode = "BW"
	}

	pricing, err := s.db.GetPricing(r.Context())
	if err == nil && !pricing.ColorEnabled {
		// Hardware enforcement: coerce to BW if kiosk color is disabled
		colorMode = "BW"
	}

	pageRange := strings.TrimSpace(r.FormValue("page_range"))

	normRes, err := pdf.NormalizeAndCountWithOptions(r.Context(), normalizedInputPath, outputPath, pdf.NormalizeOptions{
		Copies:    copies,
		Duplex:    duplexMode,
		ColorMode: colorMode,
		PageRange: pageRange,
	})
	if err != nil {
		_ = os.Remove(outputPath)
		http.Error(w, fmt.Sprintf("normalization failed: %v", err), http.StatusBadRequest)
		return
	}

	var unitPrice int64
	isDuplex := duplexMode != contracts.DuplexSimplex
	if err == nil {
		if colorMode == "COLOR" {
			if isDuplex {
				unitPrice = pricing.ColorDuplexPricePaise
			} else {
				unitPrice = pricing.ColorSimplexPricePaise
			}
		} else {
			if isDuplex {
				unitPrice = pricing.BWDuplexPricePaise
			} else {
				unitPrice = pricing.BWSimplexPricePaise
			}
		}
	}
	if unitPrice <= 0 {
		unitPrice = s.cfg.pricePerSheetPaise
	}
	if unitPrice <= 0 {
		unitPrice = 200
	}

	totalPrintedPages := normRes.TotalPages * copies
	totalPricePaise, err := payments.CalculatePricePaise(totalPrintedPages, unitPrice)
	if err != nil {
		_ = os.Remove(outputPath)
		http.Error(w, "failed to calculate price", http.StatusInternalServerError)
		return
	}

	baseURL := os.Getenv("XEROXGO_PUBLIC_URL")
	if baseURL == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		baseURL = fmt.Sprintf("%s://%s", scheme, r.Host)
	}
	baseURL = strings.TrimRight(baseURL, "/")
	downloadURL := generateSignedDownloadURL(baseURL, orderID, s.cfg.downloadSecret, 24*time.Hour)

	cmd := contracts.PrintJobCommand{
		JobID:          jobID,
		KioskID:        kioskID,
		DisplayCode:    displayCode,
		DownloadURL:    downloadURL,
		SHA256Checksum: normRes.SHA256Checksum,
		TotalPages:     normRes.TotalPages,
		TotalSheets:    normRes.TotalSheets,
		Copies:         copies,
		Duplex:         duplexMode,
		StartFromPage:  1,
	}

	order := store.Order{
		OrderID:    orderID,
		Command:    cmd,
		Status:     contracts.JobStatusPendingPayment,
		PricePaise: totalPricePaise,
		ColorMode:  colorMode,
	}

	if err := s.db.CreateOrder(r.Context(), order); err != nil {
		_ = os.Remove(outputPath)
		http.Error(w, "failed to persist order", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"order_id":              orderID,
		"job_id":                jobID,
		"display_code":          displayCode,
		"color_mode":            colorMode,
		"total_pages":           normRes.TotalPages,
		"total_sheets":          normRes.TotalSheets,
		"copies":                copies,
		"duplex":                string(duplexMode),
		"price_per_sheet_paise": unitPrice,
		"price_per_side_paise":  unitPrice,
		"unit_price_paise":      unitPrice,
		"total_price_paise":     totalPricePaise,
		"status":                string(contracts.JobStatusPendingPayment),
	})
}

func (s *appServer) handleDownloadPDF(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	prefix := "/api/orders/"
	suffix := "/pdf"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}
	orderID := strings.TrimPrefix(r.URL.Path, prefix)
	orderID = strings.TrimSuffix(orderID, suffix)
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		http.Error(w, "order id required", http.StatusBadRequest)
		return
	}

	expiresStr := r.URL.Query().Get("expires")
	sig := r.URL.Query().Get("sig")
	if expiresStr == "" || sig == "" {
		http.Error(w, "missing authentication signature", http.StatusUnauthorized)
		return
	}

	expires, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil || !verifySignedDownloadURL(orderID, expires, sig, s.cfg.downloadSecret) {
		http.Error(w, "invalid or expired download URL", http.StatusUnauthorized)
		return
	}

	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	h := sha256.Sum256([]byte(orderID))
	pdfPath := filepath.Join(s.cfg.jobsDir, hex.EncodeToString(h[:])+".pdf")

	f, err := os.Open(pdfPath)
	if err != nil {
		http.Error(w, "document artifact not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		http.Error(w, "failed to verify document integrity", http.StatusInternalServerError)
		return
	}
	calcSHA := hex.EncodeToString(hasher.Sum(nil))
	if calcSHA != strings.ToLower(order.Command.SHA256Checksum) {
		http.Error(w, "artifact integrity check failed", http.StatusInternalServerError)
		return
	}

	stat, _ := f.Stat()
	if stat != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	}
	w.Header().Set("Content-Type", "application/pdf")
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "seek error", http.StatusInternalServerError)
		return
	}
	_, _ = io.Copy(w, f)
}

func (s *appServer) handleConvert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	const maxUploadSize = 25 * 1024 * 1024 // 25 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "file exceeds 25 MiB limit or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	filename := header.Filename
	if !pdf.IsSupportedFormat(filename) {
		http.Error(w, fmt.Sprintf("unsupported file format %q: only PDF, Office documents (DOCX, PPTX, XLSX), and Images (JPG, PNG) are supported", filepath.Ext(filename)), http.StatusBadRequest)
		return
	}

	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		ext = ".tmp"
	}
	tmpInput, err := os.CreateTemp("", "conv_in_*"+ext)
	if err != nil {
		http.Error(w, "failed to create temp file", http.StatusInternalServerError)
		return
	}
	tmpInputName := tmpInput.Name()
	defer os.Remove(tmpInputName)
	defer tmpInput.Close()

	if _, err := io.Copy(tmpInput, file); err != nil {
		http.Error(w, "failed to read uploaded file", http.StatusInternalServerError)
		return
	}
	_ = tmpInput.Close()

	tmpOutput, err := os.CreateTemp("", "conv_out_*.pdf")
	if err != nil {
		http.Error(w, "failed to create output file", http.StatusInternalServerError)
		return
	}
	tmpOutputName := tmpOutput.Name()
	defer os.Remove(tmpOutputName)
	defer tmpOutput.Close()

	if err := pdf.ConvertToPDF(r.Context(), tmpInputName, filename, tmpOutputName); err != nil {
		http.Error(w, fmt.Sprintf("conversion failed: %v", err), http.StatusBadRequest)
		return
	}

	pdfBytes, err := os.ReadFile(tmpOutputName)
	if err != nil {
		http.Error(w, "failed to read converted pdf", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Length", strconv.Itoa(len(pdfBytes)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdfBytes)
}

func (s *appServer) handleSimulatePayment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !s.cfg.devMode {
		http.Error(w, "payment simulation only available in development mode", http.StatusForbidden)
		return
	}

	prefix := "/api/orders/"
	suffix := "/simulate-payment"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}
	orderID := strings.TrimPrefix(r.URL.Path, prefix)
	orderID = strings.TrimSuffix(orderID, suffix)
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		http.Error(w, "order id required", http.StatusBadRequest)
		return
	}

	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	if order.Status == contracts.JobStatusCompleted ||
		order.Status == contracts.JobStatusPrinting ||
		order.Status == contracts.JobStatusSpooling ||
		order.Status == contracts.JobStatusDispatched {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id": orderID,
			"status":   string(order.Status),
		})
		return
	}

	syntheticPayload := map[string]interface{}{
		"event": "payment.captured",
		"payload": map[string]interface{}{
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_sim_" + orderID,
					"order_id": orderID,
					"notes": map[string]interface{}{
						"order_id": orderID,
					},
					"status": "captured",
					"amount": order.PricePaise,
				},
			},
		},
	}
	rawPayload, err := json.Marshal(syntheticPayload)
	if err != nil {
		http.Error(w, "failed to marshal payload", http.StatusInternalServerError)
		return
	}

	mac := hmac.New(sha256.New, s.cfg.webhookSecret)
	mac.Write(rawPayload)
	sig := hex.EncodeToString(mac.Sum(nil))

	if !payments.VerifyWebhookSignature(rawPayload, sig, string(s.cfg.webhookSecret)) {
		http.Error(w, "internal signature verification failed", http.StatusInternalServerError)
		return
	}

	webhookID := "evt_sim_" + orderID
	recorded, err := s.db.RecordWebhook(r.Context(), webhookID)
	if err != nil {
		http.Error(w, "failed to record webhook", http.StatusInternalServerError)
		return
	}
	if !recorded {
		// Idempotent repetition
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id": orderID,
			"status":   string(order.Status),
		})
		return
	}

	if err := s.db.UpdateOrderStatus(r.Context(), orderID, contracts.JobStatusPaidWaiting); err != nil {
		http.Error(w, "failed to update order status", http.StatusInternalServerError)
		return
	}

	if err := s.hub.SendJob(r.Context(), order.Command); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"order_id": orderID,
			"status":   string(contracts.JobStatusPaidWaiting),
			"error":    fmt.Sprintf("kiosk offline or dispatch failed: %v", err),
		})
		return
	}

	_ = s.db.UpdateOrderStatus(r.Context(), orderID, contracts.JobStatusDispatched)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"order_id": orderID,
		"status":   string(contracts.JobStatusDispatched),
	})
}

func (s *appServer) handleRazorpayWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	const maxWebhookSize = 1024 * 1024 // 1 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookSize)
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read webhook body", http.StatusBadRequest)
		return
	}

	sig := r.Header.Get("X-Razorpay-Signature")
	if sig == "" || !payments.VerifyWebhookSignature(rawBody, sig, string(s.cfg.webhookSecret)) {
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	var parsed struct {
		Event   string `json:"event"`
		Payload struct {
			Payment struct {
				Entity struct {
					ID      string `json:"id"`
					OrderID string `json:"order_id"`
					Amount  int64  `json:"amount"`
					Status  string `json:"status"`
					Notes   struct {
						OrderID string `json:"order_id"`
					} `json:"notes"`
				} `json:"entity"`
			} `json:"payment"`
		} `json:"payload"`
	}

	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		http.Error(w, "malformed webhook json", http.StatusBadRequest)
		return
	}

	if parsed.Event != "payment.captured" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored"}`))
		return
	}

	orderID := parsed.Payload.Payment.Entity.Notes.OrderID
	if orderID == "" {
		orderID = parsed.Payload.Payment.Entity.OrderID
	}
	if orderID == "" {
		http.Error(w, "order id missing in payment entity", http.StatusBadRequest)
		return
	}

	eventID := parsed.Payload.Payment.Entity.ID
	if eventID == "" {
		eventID = "evt_" + orderID
	}

	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		http.Error(w, "order not found", http.StatusBadRequest)
		return
	}

	// Requirement 10 & 19: Verify the actual Razorpay order amount equals the stored order amount in paise.
	if parsed.Payload.Payment.Entity.Amount > 0 && parsed.Payload.Payment.Entity.Amount != order.PricePaise {
		http.Error(w, fmt.Sprintf("payment amount mismatch: expected %d paise, got %d paise", order.PricePaise, parsed.Payload.Payment.Entity.Amount), http.StatusBadRequest)
		return
	}

	recorded, err := s.db.RecordWebhook(r.Context(), eventID)
	if err != nil {
		http.Error(w, "failed to record webhook", http.StatusInternalServerError)
		return
	}
	if !recorded {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"duplicate"}`))
		return
	}

	_ = s.db.UpdateOrderStatus(r.Context(), orderID, contracts.JobStatusPaidWaiting)

	if err := s.hub.SendJob(r.Context(), order.Command); err == nil {
		_ = s.db.UpdateOrderStatus(r.Context(), orderID, contracts.JobStatusDispatched)
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *appServer) handleOrderStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	prefix := "/api/orders/"
	suffix := "/status"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}
	orderID := strings.TrimPrefix(r.URL.Path, prefix)
	orderID = strings.TrimSuffix(orderID, suffix)
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		http.Error(w, "order id required", http.StatusBadRequest)
		return
	}

	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	pagesPrinted := 0
	sheetsPrinted := 0
	currentStatus := string(order.Status)
	errorReason := ""

	progress, err := s.db.GetJobProgress(r.Context(), order.Command.JobID)
	if err == nil && progress != nil {
		pagesPrinted = progress.PagesPrinted
		sheetsPrinted = progress.SheetsPrinted
		errorReason = progress.ErrorReason
		switch progress.Status {
		case contracts.JobStatusSpooling,
			contracts.JobStatusPrinting,
			contracts.JobStatusCompleted,
			contracts.JobStatusPausedError,
			contracts.JobStatusFailedRefundable:
			currentStatus = string(progress.Status)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"order_id":       order.OrderID,
		"display_code":   order.Command.DisplayCode,
		"status":         currentStatus,
		"total_pages":    order.Command.TotalPages,
		"total_sheets":   order.Command.TotalSheets,
		"copies":         order.Command.Copies,
		"duplex":         string(order.Command.Duplex),
		"pages_printed":  pagesPrinted,
		"sheets_printed": sheetsPrinted,
		"error_reason":   errorReason,
		"price_paise":    order.PricePaise,
	})
}

func main() {
	addr := os.Getenv("XEROXGO_CLOUD_ADDR")
	if addr == "" {
		if p := os.Getenv("PORT"); p != "" {
			addr = ":" + p
		} else {
			addr = ":8080"
		}
	}

	dbPath := os.Getenv("DATABASE_URL")
	if dbPath == "" {
		dbPath = os.Getenv("XEROXGO_CLOUD_DB")
	}
	if dbPath == "" {
		dbPath = "./data/xeroxgo-cloud.db"
	}

	jobsDir := os.Getenv("XEROXGO_JOBS_DIR")
	if jobsDir == "" {
		jobsDir = "./data/jobs"
	}

	devMode := os.Getenv("XEROXGO_DEV_MODE") == "1"

	dlSecretStr := os.Getenv("XEROXGO_DOWNLOAD_SECRET")
	whSecretStr := os.Getenv("XEROXGO_WEBHOOK_SECRET")

	if !devMode {
		if len(dlSecretStr) < 16 || len(whSecretStr) < 16 {
			log.Fatalf("Fatal: production mode requires non-empty XEROXGO_DOWNLOAD_SECRET and XEROXGO_WEBHOOK_SECRET of at least 16 characters")
		}
	} else {
		if dlSecretStr == "" {
			dlSecretStr = "dev_download_secret_32_bytes_long_123"
		}
		if whSecretStr == "" {
			whSecretStr = "dev_webhook_secret_32_bytes_long_123"
		}
	}

	isPostgres := strings.HasPrefix(dbPath, "postgres://") || strings.HasPrefix(dbPath, "postgresql://")
	if !isPostgres {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
			log.Fatalf("failed to create db directory: %v", err)
		}
	}
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		log.Fatalf("failed to create jobs directory: %v", err)
	}

	storeDB, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("failed to open cloud database at %s: %v", dbPath, err)
	}
	defer storeDB.Close()

	ctx := context.Background()
	var pricePaise int64
	priceEnv := os.Getenv("XEROXGO_PRICE_PER_SHEET_PAISE")
	if priceEnv != "" {
		p, err := strconv.ParseInt(priceEnv, 10, 64)
		if err != nil || p <= 0 {
			log.Fatalf("invalid XEROXGO_PRICE_PER_SHEET_PAISE: %s", priceEnv)
		}
		if err := storeDB.SetPricePerSheetPaise(ctx, p); err != nil {
			log.Fatalf("failed to persist price per sheet: %v", err)
		}
		pricePaise = p
	} else {
		p, err := storeDB.GetPricePerSheetPaise(ctx)
		if err == nil && p > 0 {
			pricePaise = p
		} else if devMode {
			pricePaise = 500
			_ = storeDB.SetPricePerSheetPaise(ctx, pricePaise)
		} else {
			log.Fatalf("pricing unconfigured in production mode")
		}
	}

	// Initialize default promo ads & bootstrap admin user
	bootstrapCtx, bootstrapCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer bootstrapCancel()
	if err := storeDB.SeedDefaultAds(bootstrapCtx); err != nil {
		log.Printf("[WARN] Failed to seed default ads: %v", err)
	}
	adminUser := os.Getenv("XEROXGO_ADMIN_USER")
	if adminUser == "" {
		adminUser = "admin"
	}
	adminPass := os.Getenv("XEROXGO_ADMIN_PASSWORD")
	if adminPass == "" {
		adminPass = "xeroxgo2026"
	}
	if err := storeDB.BootstrapAdmin(bootstrapCtx, adminUser, adminPass); err != nil {
		log.Printf("[WARN] Failed to bootstrap admin: %v", err)
	}

	hub := ws.NewHub(storeDB)

	cfg := &serverConfig{
		addr:               addr,
		dbPath:             dbPath,
		jobsDir:            jobsDir,
		pricePerSheetPaise: pricePaise,
		devMode:            devMode,
		downloadSecret:     []byte(dlSecretStr),
		webhookSecret:      []byte(whSecretStr),
	}

	app := &appServer{
		cfg:      cfg,
		db:       storeDB,
		hub:      hub,
		convWake: make(chan struct{}, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", app.handleHealthz)
	mux.HandleFunc("/k/", app.handlePortal)

	subFS, err := fs.Sub(staticEmbedFS, "static")
	if err != nil {
		log.Fatalf("failed to create sub fs: %v", err)
	}
	staticFileServer := http.FileServer(http.FS(subFS))
	mux.Handle("/static/", http.StripPrefix("/static/", staticFileServer))
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/assets/favicon.svg", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/assets/manifest.webmanifest"
		staticFileServer.ServeHTTP(w, r)
	})

	mux.HandleFunc("/api/convert", app.handleConvert)
	mux.HandleFunc("/api/conversions", app.handleCreateConversion)
	mux.HandleFunc("/api/conversions/", app.handleConversionRoutes)
	mux.HandleFunc("/api/k/", app.handleUpload)
	mux.HandleFunc("/api/orders/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/status"):
			app.handleOrderStatus(w, r)
		case strings.HasSuffix(path, "/simulate-payment"):
			app.handleSimulatePayment(w, r)
		case strings.HasSuffix(path, "/pdf"):
			app.handleDownloadPDF(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/api/webhooks/razorpay", app.handleRazorpayWebhook)
	mux.HandleFunc("/ws", hub.ServeHTTP)

	// Public Ads API
	mux.HandleFunc("/api/ads", app.handlePublicAds)
	mux.HandleFunc("/api/ads/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/ads/images/") {
			app.handleServeAdImage(w, r)
			return
		}
		app.handlePublicAds(w, r)
	})

	// Public Pricing API
	mux.HandleFunc("/api/pricing", app.handlePublicPricing)

	// Protected Admin Dashboard API
	mux.HandleFunc("/api/admin/login", app.handleAdminLogin)
	mux.HandleFunc("/api/admin/logout", app.handleAdminLogout)
	mux.HandleFunc("/api/admin/me", app.handleAdminMe)
	mux.HandleFunc("/api/admin/password", app.handleAdminPassword)
	mux.HandleFunc("/api/admin/upload-image", app.handleAdminUploadImage)
	mux.HandleFunc("/api/admin/ads", app.handleAdminAds)
	mux.HandleFunc("/api/admin/ads/", app.handleAdminAds)
	mux.HandleFunc("/api/admin/pricing", app.handleAdminPricing)
	mux.HandleFunc("/api/admin/analytics", app.handleAdminAnalytics)
	mux.HandleFunc("/api/admin/orders", app.handleAdminOrders)
	mux.HandleFunc("/api/admin/orders/export", app.handleAdminOrdersExport)

	corsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && isAllowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			if origin != "" && !isAllowedOrigin(origin) {
				http.Error(w, "Forbidden origin", http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:    addr,
		Handler: corsHandler,
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go app.startConversionWorker(sigCtx)

	serverErrCh := make(chan error, 1)
	go func() {
		log.Printf("XeroxGo cloud backend listening on %s (dev_mode=%v, price=%d paise)", addr, devMode, pricePaise)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
		}
	}()

	select {
	case err := <-serverErrCh:
		log.Fatalf("server failure: %v", err)
	case <-sigCtx.Done():
		log.Printf("shutting down cloud backend...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}
	}
}

// isAllowedOrigin checks if an incoming Origin header is permitted for CORS requests.
func isAllowedOrigin(origin string) bool {
	if origin == "" {
		return false
	}

	// Canonical production and staging domains
	if origin == "https://xeroxgo.vercel.app" || origin == "https://campusprint.vercel.app" {
		return true
	}

	// Local development origins
	if origin == "http://localhost:3000" || origin == "http://localhost:8080" || origin == "http://localhost:5173" ||
		origin == "http://127.0.0.1:3000" || origin == "http://127.0.0.1:8080" || origin == "http://127.0.0.1:5173" {
		return true
	}

	// Preview deployments on Vercel
	if strings.HasPrefix(origin, "https://campusprint-") && strings.HasSuffix(origin, ".vercel.app") {
		return true
	}
	if strings.HasPrefix(origin, "https://xeroxgo-") && strings.HasSuffix(origin, ".vercel.app") {
		return true
	}

	// Custom allowed origins configured via environment variable
	if custom := os.Getenv("XEROXGO_ALLOWED_ORIGINS"); custom != "" {
		for _, o := range strings.Split(custom, ",") {
			if strings.TrimSpace(o) == origin {
				return true
			}
		}
	}

	return false
}
