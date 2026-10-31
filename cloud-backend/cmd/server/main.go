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
	"net/http"
	"os"
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
	cfg *serverConfig
	db  *store.Store
	hub *ws.Hub
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

  <link rel="stylesheet" href="/static/css/portal.css" />
  <script src="/static/vendor/pdf-lib.min.js"></script>
  <script type="module">
    import * as pdfjsLib from '/static/vendor/pdf.min.mjs';
    window.pdfjsLib = pdfjsLib;
    pdfjsLib.GlobalWorkerOptions.workerSrc = '/static/vendor/pdf.worker.min.mjs';
  </script>
  <script src="/static/js/portal.js" defer></script>
</head>
<body>
  <div class="app-container" data-kiosk-id="{{.KioskID}}" data-dev-mode="{{.DevMode}}" data-price-per-sheet-paise="{{.PricePerSheetPaise}}">
    <!-- Header -->
    <header class="header" style="display:flex; flex-direction:column; align-items:center; justify-content:center; padding:1.25rem 1rem 0.75rem;">
      <div class="logo-container" style="display:flex; align-items:center; justify-content:center; margin-bottom:0.35rem;">
        <img
          src="/static/assets/xeroxgo-full-logo-transparent.png"
          alt="XeroxGO"
          class="logo-img"
          style="height:34px; width:min(100%, 340px); object-fit:contain;"
        />
      </div>
      <p class="header-subtitle" style="margin:0; color:var(--text-secondary); font-size:0.95rem;">
        Upload, Print, Collect - It's that simple!
      </p>
    </header>

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

    <!-- 1. Upload Document Section -->
    <div id="upload-section" style="margin-top:1.25rem;">
      <h2 class="section-title" style="padding-left:1rem; margin-bottom:0.5rem;">1. Upload Document</h2>
      <div class="card" style="margin-top:0;">
        <div id="upload-area" class="upload-area">
          <input type="file" id="file-input" accept=".pdf" style="display:none;" />
          <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="upload-icon" style="margin:0 auto 0.5rem; display:block;">
            <path d="M17.5 19H9a7 7 0 1 1 6.71-9h1.79a4.5 4.5 0 1 1 0 9Z"/><polyline points="16 16 12 12 8 16"/><line x1="12" y1="12" x2="12" y2="21"/>
          </svg>
          <div class="upload-text">Click to upload or drag and drop</div>
          <div class="upload-subtext">Supported format: PDF</div>
          <div class="upload-subtext">Max size: 25 MB</div>
        </div>

        <div id="file-preview-card" class="file-preview animate-slide-up hidden" style="flex-direction:column; align-items:stretch; gap:0.75rem;">
          <div style="display:flex; align-items:center; justify-content:space-between;">
            <div class="file-info">
              <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="#2563EB" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/><line x1="16" y1="13" x2="8" y2="13"/><line x1="16" y1="17" x2="8" y2="17"/></svg>
              <div>
                <div id="file-name-display" class="file-name" style="font-weight:700; font-size:1rem;">-</div>
                <div id="file-meta-display" class="file-meta">-</div>
              </div>
            </div>
            <button id="remove-file-btn" class="remove-btn" type="button" title="Remove file">
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
            </button>
          </div>
          <div style="display:flex; gap:0.5rem; margin-top:0.25rem;">
            <button id="btn-view-doc" type="button" class="btn" style="flex:1; padding:0.45rem 0.75rem; border-radius:var(--radius-md); border:1px solid var(--border-color); background:#F8FAFC; color:var(--text-primary); font-weight:600; font-size:0.85rem; display:inline-flex; align-items:center; justify-content:center; gap:0.35rem; cursor:pointer;">
              <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/></svg>
              View
            </button>
            <button id="btn-edit-pages" type="button" class="btn" style="flex:1; padding:0.45rem 0.75rem; border-radius:var(--radius-md); border:1px solid var(--primary-color); background:rgba(37,99,235,0.05); color:var(--primary-color); font-weight:600; font-size:0.85rem; display:inline-flex; align-items:center; justify-content:center; gap:0.35rem; cursor:pointer;">
              <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 20h9"/><path d="M16.5 3.5a2.121 2.121 0 0 1 3 3L7 19l-4 1 1-4L16.5 3.5z"/></svg>
              Edit Pages
            </button>
          </div>
        </div>
        <div id="upload-alert" class="alert alert-error hidden"></div>
      </div>
    </div>

    <!-- 2. Select Print Options Section -->
    <div id="print-options-section" style="margin-top:2rem;">
      <h2 class="section-title" style="padding-left:1rem; margin-bottom:0.5rem;">2. Select Print Options</h2>
      <div class="card" style="margin-top:0;">
        <h3 style="font-size:1.25rem; font-weight:700; margin-bottom:1.75rem; color:var(--text-primary);">Print Options</h3>

        <!-- Page Selection -->
        <div style="margin-bottom:0.75rem; display:flex; align-items:center; gap:0.5rem; color:var(--text-primary); font-weight:600;">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="var(--text-secondary)" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>
          Page Selection
        </div>
        <div class="option-group" style="margin-bottom:1.25rem;">
          <button id="btn-page-selection-all" type="button" class="option-btn active" style="padding:0.75rem;">
            <div style="color:var(--text-primary); font-weight:600;">All Pages</div>
          </button>
          <button id="btn-page-selection-custom" type="button" class="option-btn" style="padding:0.75rem;">
            <div style="color:var(--text-primary); font-weight:600;">Custom Range</div>
          </button>
        </div>

        <div id="custom-range-box" class="hidden" style="margin-bottom:1.75rem;">
          <input
            id="custom-range-input"
            type="text"
            placeholder="e.g. 1-3, 5"
            style="width:100%; padding:0.75rem; border-radius:var(--radius-md); border:1px solid var(--border-color); font-size:1rem; font-family:inherit;"
          />
          <div id="custom-range-feedback" style="color:var(--text-secondary); font-size:0.875rem; margin-top:0.4rem;">
            Enter pages to print
          </div>
        </div>

        <!-- Number of Copies -->
        <div style="margin-bottom:0.75rem; display:flex; align-items:center; gap:0.5rem; color:var(--text-primary); font-weight:600;">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="var(--text-secondary)" stroke-width="2"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/></svg>
          Number of Copies
        </div>
        <div class="counter" style="justify-content:flex-start; margin-bottom:1.75rem;">
          <button id="copies-dec" class="counter-btn" type="button" style="border-radius:8px;">-</button>
          <div id="copies-val" class="counter-value" style="width:40px; font-weight:800;">1</div>
          <button id="copies-inc" class="counter-btn" type="button" style="border-radius:8px;">+</button>
        </div>

        <!-- Print Mode -->
        <div style="margin-bottom:0.75rem; display:flex; align-items:center; gap:0.5rem; color:var(--text-primary); font-weight:600;">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="var(--text-secondary)" stroke-width="2"><circle cx="13.5" cy="6.5" r=".5" fill="currentColor"/><circle cx="17.5" cy="10.5" r=".5" fill="currentColor"/><circle cx="8.5" cy="7.5" r=".5" fill="currentColor"/><circle cx="6.5" cy="12.5" r=".5" fill="currentColor"/><path d="M12 2C6.5 2 2 6.5 2 12s4.5 10 10 10c.926 0 1.648-.746 1.648-1.688 0-.437-.18-.835-.437-1.125-.29-.289-.438-.652-.438-1.125a1.64 1.64 0 0 1 1.668-1.668h1.996c3.051 0 5.555-2.503 5.555-5.554C21.965 6.012 17.461 2 12 2z"/></svg>
          Print Mode
        </div>
        <div class="option-group" style="margin-bottom:1.75rem;">
          <button id="btn-mode-bw" type="button" class="option-btn active" style="flex-direction:row; justify-content:center; padding:1rem;">
            <div style="display:flex; align-items:center; gap:0.75rem;">
              <div style="width:28px; height:28px; border-radius:50%; background:linear-gradient(135deg, #64748B, #334155);"></div>
              <div style="text-align:left;">
                <div style="color:var(--text-primary); font-weight:600;">B&amp;W</div>
                <div style="font-size:0.875rem; color:var(--text-secondary);">₹5.00/sheet</div>
              </div>
            </div>
          </button>
          <button id="btn-mode-color" type="button" class="option-btn" disabled style="flex-direction:row; justify-content:center; padding:1rem; opacity:0.55; cursor:not-allowed;" title="Monochrome kiosk — Color not supported">
            <div style="display:flex; align-items:center; gap:0.75rem;">
              <div style="width:28px; height:28px; border-radius:50%; background:linear-gradient(135deg, #F59E0B, #3B82F6);"></div>
              <div style="text-align:left;">
                <div style="color:var(--text-primary); font-weight:600;">Color</div>
                <div style="font-size:0.75rem; color:var(--text-secondary);">B&amp;W Only Kiosk</div>
              </div>
            </div>
          </button>
        </div>

        <!-- Print Sides -->
        <div style="margin-bottom:0.75rem; display:flex; align-items:center; gap:0.5rem; color:var(--text-primary); font-weight:600;">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="var(--text-secondary)" stroke-width="2"><polygon points="12 2 2 7 12 12 22 7 12 2"/><polyline points="2 17 12 22 22 17"/><polyline points="2 12 12 17 22 12"/></svg>
          Print Sides
        </div>
        <div class="option-group" style="margin-bottom:1.75rem;">
          <button id="btn-side-single" type="button" class="option-btn active" style="padding:1rem;">
            <div style="border:2px solid var(--text-primary); width:32px; height:42px; border-radius:8px; margin-bottom:0.75rem;"></div>
            <div style="color:var(--text-primary); font-weight:600;">Single Side</div>
          </button>
          <button id="btn-side-double" type="button" class="option-btn" style="padding:1rem;">
            <div style="position:relative; width:38px; height:42px; margin-bottom:0.75rem;">
              <div style="position:absolute; top:0; left:0; border:2px solid var(--text-primary); width:32px; height:42px; border-radius:8px; opacity:0.25;"></div>
              <div style="position:absolute; top:4px; left:4px; border:2px solid var(--text-primary); width:32px; height:42px; border-radius:8px; background-color:white;"></div>
            </div>
            <div style="color:var(--text-primary); font-weight:600;">Double Side</div>
            <div style="font-size:0.75rem; color:var(--secondary-color); font-weight:700; margin-top:0.25rem;">Save paper!</div>
          </button>
        </div>

        <!-- Pages Per Sheet -->
        <div style="margin-bottom:0.75rem; display:flex; align-items:center; gap:0.5rem; color:var(--text-primary); font-weight:600;">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="var(--text-secondary)" stroke-width="2"><rect width="7" height="7" x="3" y="3" rx="1"/><rect width="7" height="7" x="14" y="3" rx="1"/><rect width="7" height="7" x="14" y="14" rx="1"/><rect width="7" height="7" x="3" y="14" rx="1"/></svg>
          Pages Per Sheet
        </div>
        <div class="option-group" style="margin-bottom:0.5rem; display:grid; grid-template-columns:repeat(auto-fit, minmax(130px, 1fr)); gap:0.5rem;">
          <button id="btn-nup-1" type="button" class="option-btn active" style="padding:0.75rem 0.5rem; text-align:center;">
            <div style="color:var(--text-primary); font-weight:600; font-size:0.9rem;">1 page/sheet</div>
            <div style="font-size:0.75rem; color:var(--text-secondary); margin-top:0.15rem;">Standard A4 layout</div>
          </button>
          <button id="btn-nup-2" type="button" class="option-btn" disabled style="padding:0.75rem 0.5rem; text-align:center; opacity:0.5; cursor:not-allowed;" title="2-up layout handled via standard document formatting">
            <div style="color:var(--text-primary); font-weight:600; font-size:0.9rem;">2 pages/sheet</div>
            <div style="font-size:0.75rem; color:var(--text-secondary); margin-top:0.15rem;">1-Up Only</div>
          </button>
        </div>
      </div>
    </div>

    <!-- 3. Review & Pay Section -->
    <div id="review-section" style="margin-top:2rem; margin-bottom:2rem;">
      <h2 class="section-title" style="padding-left:1rem; margin-bottom:0.5rem;">3. Review &amp; Pay</h2>
      <div id="review-card" class="card" style="margin-top:0; padding:1.25rem;">
        <div style="background-color:#F8FAFC; border-radius:var(--radius-md); padding:1.15rem; border:1px solid var(--border-color); margin-bottom:1.25rem;">
          <div style="margin-bottom:0.75rem;">
            <div id="summary-file-name" style="font-weight:700; font-size:1.05rem; color:var(--text-primary); word-break:break-all;">-</div>
            <div id="summary-pages" style="font-size:0.875rem; color:var(--text-secondary); margin-top:0.2rem;">-</div>
          </div>
          <div style="height:1px; background-color:var(--border-color); margin:0.75rem 0;"></div>
          <div style="display:flex; flex-direction:column; gap:0.35rem; margin-bottom:1.1rem;">
            <div id="summary-options" style="font-weight:600; font-size:0.95rem; color:var(--text-primary);">B&amp;W · Single Side · 1 page/sheet</div>
            <div id="summary-copies" style="font-size:0.875rem; color:var(--text-secondary);">1 copy</div>
          </div>
          <button id="btn-edit-settings" type="button" style="display:inline-flex; align-items:center; gap:0.4rem; padding:0.45rem 0.85rem; border-radius:var(--radius-md); border:1px solid var(--primary-color); background:rgba(37,99,235,0.06); color:var(--primary-color); font-weight:600; font-size:0.85rem; cursor:pointer;">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="4" y1="21" x2="4" y2="14"/><line x1="4" y1="10" x2="4" y2="3"/><line x1="12" y1="21" x2="12" y2="12"/><line x1="12" y1="8" x2="12" y2="3"/><line x1="20" y1="21" x2="20" y2="16"/><line x1="20" y1="12" x2="20" y2="3"/><line x1="1" y1="14" x2="7" y2="14"/><line x1="9" y1="8" x2="15" y2="8"/><line x1="17" y1="16" x2="23" y2="16"/></svg>
            Edit Settings
          </button>
          <div style="height:1px; background-color:var(--border-color); margin:1rem 0;"></div>
          <div style="display:flex; align-items:center; justify-content:space-between;">
            <span style="font-size:1rem; font-weight:600; color:var(--text-secondary);">Total Price</span>
            <span id="summary-price" style="font-size:1.6rem; font-weight:800; color:var(--primary-color);">₹0</span>
          </div>
        </div>

        <div style="display:flex; flex-direction:column; gap:1.25rem;">
          <div>
            <label for="user-name-input" style="display:block; margin-bottom:0.4rem; font-weight:600; font-size:0.9rem; color:var(--text-primary);">
              Your Name (Optional)
            </label>
            <input
              id="user-name-input"
              type="text"
              placeholder="Enter your name"
              style="width:100%; padding:0.75rem; border-radius:var(--radius-md); border:1px solid var(--border-color); font-size:1rem; font-family:inherit;"
            />
          </div>

          <div id="upload-progress-card" class="hidden" style="background:#FFFFFF; border-radius:var(--radius-lg); border:1px solid var(--border-color); padding:1.25rem 1rem; text-align:center;">
            <div class="progress-bar-bg" style="margin-bottom:0.75rem;">
              <div id="upload-progress-bar" class="progress-bar-fill" style="width:40%;"></div>
            </div>
            <p style="font-weight:600; color:var(--text-primary); margin:0; font-size:0.9rem;">Sending your document...</p>
          </div>

          <button id="btn-pay-upi" type="button" class="btn btn-primary" style="padding:0.85rem; font-size:0.95rem; font-weight:700; gap:0.5rem;">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect width="14" height="20" x="5" y="2" rx="2" ry="2"/><line x1="12" y1="18" x2="12.01" y2="18"/></svg>
            Pay with UPI &amp; Print
          </button>
        </div>
        <div id="review-alert" class="alert alert-error hidden"></div>
      </div>
    </div>

    <!-- 4. Live Print Status & Order Tracking Section -->
    <div id="status-section" class="hidden" style="margin-top:1.5rem; margin-bottom:2rem;">
      <h2 class="section-title" style="padding-left:1rem; margin-bottom:0.5rem;">Print Order Status</h2>
      <div class="card" style="margin-top:0; padding:1.25rem; display:flex; flex-direction:column; gap:1.25rem;">
        <div>
          <label style="display:block; margin-bottom:0.4rem; font-weight:600; font-size:0.85rem; color:var(--text-secondary); text-transform:uppercase; letter-spacing:0.5px;">
            Your Kiosk Pickup Code
          </label>
          <div id="status-display-code" class="code-display">----</div>
          <div style="font-size:0.8rem; color:var(--text-secondary); text-align:center; margin-top:0.35rem;">
            Enter this 4-digit code on the kiosk screen if prompted.
          </div>
        </div>

        <div style="display:flex; justify-content:space-between; align-items:center;">
          <span style="font-weight:600; color:var(--text-secondary); font-size:0.95rem;">Current Status</span>
          <span id="status-badge" class="status-pill status-PENDING_PAYMENT">PENDING_PAYMENT</span>
        </div>

        <div>
          <div class="progress-bar-bg">
            <div id="status-progress-fill" class="progress-bar-fill" style="width:0%;"></div>
          </div>
          <div style="display:flex; justify-content:space-between; font-size:0.85rem; color:var(--text-secondary); margin-top:0.5rem;">
            <span>Pages: <strong id="status-pages-text" style="color:var(--text-primary);">0 / 0</strong></span>
            <span>Sheets: <strong id="status-sheets-text" style="color:var(--text-primary);">0 / 0</strong></span>
          </div>
        </div>

        {{if .DevMode}}
        <button id="dev-simulate-payment-btn" type="button" class="btn btn-primary" style="background:#10B981; border:none; padding:0.75rem; font-weight:700;">
          Simulate Payment (Dev Mode)
        </button>
        {{end}}

        <div id="status-success-alert" class="alert alert-success hidden" style="font-weight:600; text-align:center;">
          🎉 Print job completed! Please collect your document from the printer tray.
        </div>
        <div id="status-error-alert" class="alert alert-error hidden"></div>
      </div>
    </div>

    <!-- PWA Install Guide Modal (for iOS and non-native browsers) -->
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
              <span>Tap the browser <strong>Share</strong> icon (iOS) or <strong>Menu (⋮)</strong> (Android)</span>
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

    <!-- Document Review & Page Editor Modal -->
    <div id="editor-modal-overlay" class="modal-overlay hidden">
      <div class="editor-modal animate-scale-up" onclick="event.stopPropagation();">
        <!-- Modal Header -->
        <div style="padding:1rem 1.25rem; border-bottom:1px solid var(--border-color); display:flex; align-items:center; justify-content:space-between; background:#F8FAFC;">
          <div style="display:flex; align-items:center; gap:0.5rem; overflow:hidden;">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="#2563EB" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>
            <span id="editor-modal-title" style="font-weight:700; font-size:1rem; color:var(--text-primary); white-space:nowrap; overflow:hidden; text-overflow:ellipsis;">Document Preview</span>
          </div>
          <button id="editor-modal-close-btn" class="remove-btn" type="button" aria-label="Close modal">
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
          </button>
        </div>

        <!-- Modal Toolbar -->
        <div style="padding:0.75rem 1.25rem; border-bottom:1px solid var(--border-color); display:flex; flex-wrap:wrap; align-items:center; justify-content:space-between; gap:0.5rem; background:#FFFFFF;">
          <!-- Pager Controls -->
          <div style="display:flex; align-items:center; gap:0.35rem;">
            <button id="btn-prev-page" type="button" style="border:1px solid #CBD5E1; background:#FFFFFF; border-radius:6px; padding:4px 8px; cursor:pointer;" title="Previous Page">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="19" y1="12" x2="5" y2="12"/><polyline points="12 19 5 12 12 5"/></svg>
            </button>
            <span id="modal-page-indicator" style="font-size:0.82rem; font-weight:600; color:#334155; min-width:85px; text-align:center;">Page 1 of 1</span>
            <button id="btn-next-page" type="button" style="border:1px solid #CBD5E1; background:#FFFFFF; border-radius:6px; padding:4px 8px; cursor:pointer;" title="Next Page">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="5" y1="12" x2="19" y2="12"/><polyline points="12 5 19 12 12 19"/></svg>
            </button>
            <button id="btn-toggle-include" type="button" style="margin-left:0.5rem; display:inline-flex; align-items:center; gap:4px; padding:3px 8px; border-radius:12px; font-size:0.75rem; font-weight:700; border:none; background:#DCFCE7; color:#166534; cursor:pointer;">
              <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3"><polyline points="20 6 9 17 4 12"/></svg> Included
            </button>
          </div>

          <!-- Zoom Controls -->
          <div style="display:flex; align-items:center; gap:0.35rem;">
            <button id="btn-zoom-out" type="button" style="border:1px solid #CBD5E1; background:#FFFFFF; border-radius:6px; padding:4px 8px; cursor:pointer;" title="Zoom Out">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/><line x1="8" y1="11" x2="14" y2="11"/></svg>
            </button>
            <button id="btn-zoom-reset" type="button" style="border:1px solid #CBD5E1; background:#FFFFFF; border-radius:6px; padding:3px 8px; font-size:0.75rem; font-weight:600; cursor:pointer;" title="Reset Zoom">
              100%
            </button>
            <button id="btn-zoom-in" type="button" style="border:1px solid #CBD5E1; background:#FFFFFF; border-radius:6px; padding:4px 8px; cursor:pointer;" title="Zoom In">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/><line x1="11" y1="8" x2="11" y2="14"/><line x1="8" y1="11" x2="14" y2="11"/></svg>
            </button>
            <button id="btn-fullscreen" type="button" style="border:1px solid #CBD5E1; background:#FFFFFF; border-radius:6px; padding:4px 8px; cursor:pointer; margin-left:0.25rem;" title="Fullscreen Preview">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="15 3 21 3 21 9"/><polyline points="9 21 3 21 3 15"/><line x1="21" y1="3" x2="14" y2="10"/><line x1="3" y1="21" x2="10" y2="14"/></svg>
            </button>
          </div>
        </div>

        <!-- Canvas Preview Area -->
        <div style="position:relative; background-color:#F1F5F9; min-height:260px; max-height:48vh; display:flex; align-items:center; justify-content:center; overflow:auto; padding:1rem;">
          <div id="canvas-loading-spinner" class="hidden" style="display:flex; flex-direction:column; align-items:center; gap:0.5rem; color:#64748B;">
            <svg class="spin" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg>
            <span style="font-size:0.85rem;">Rendering page preview...</span>
          </div>
          <canvas id="main-preview-canvas" style="display:block; max-width:100%; height:auto; border-radius:4px; box-shadow:0 4px 12px rgba(0,0,0,0.12); background-color:#FFFFFF;"></canvas>
        </div>

        <!-- Action Strip & Thumbnail Carousel -->
        <div style="padding:0.75rem 1.25rem; background:#FFFFFF; border-top:1px solid var(--border-color); display:flex; flex-direction:column; gap:0.5rem;">
          <div style="display:flex; flex-wrap:wrap; align-items:center; justify-content:space-between; gap:0.5rem;">
            <div style="display:flex; gap:0.35rem;">
              <button id="btn-select-all" type="button" style="font-size:0.78rem; font-weight:600; padding:4px 8px; border-radius:4px; border:1px solid #93C5FD; background:#FFFFFF; cursor:pointer;">Select All</button>
              <button id="btn-deselect-all" type="button" style="font-size:0.78rem; font-weight:600; padding:4px 8px; border-radius:4px; border:1px solid #93C5FD; background:#FFFFFF; cursor:pointer;">Deselect All</button>
              <button id="btn-rotate-all" type="button" style="font-size:0.78rem; font-weight:600; padding:4px 8px; border-radius:4px; border:1px solid #93C5FD; background:#FFFFFF; cursor:pointer; display:flex; align-items:center; gap:3px;">
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8"/><polyline points="21 3 21 8 16 8"/></svg>
                Rotate All
              </button>
            </div>
            <div style="display:flex; align-items:center; gap:0.35rem;">
              <span style="font-size:0.78rem; font-weight:600; color:#1E40AF;">Range:</span>
              <input id="modal-custom-range-input" type="text" placeholder="e.g. 1-3, 5" style="width:100px; padding:3px 6px; border-radius:4px; border:1px solid #93C5FD; font-size:0.78rem; font-family:monospace;" />
            </div>
          </div>
          <!-- Horizontal Thumbnails -->
          <div id="thumbnail-carousel" style="display:flex; gap:0.65rem; overflow-x:auto; padding-bottom:0.5rem; padding-top:0.25rem;"></div>
        </div>
      </div>
    </div>
  </div>
</body>
</html>`

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

	const maxUploadSize = 25 * 1024 * 1024 // 25 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "request exceeds 25 MiB limit or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

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

	tmpInput, err := os.CreateTemp(s.cfg.jobsDir, "upload_in_*.pdf")
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

	normRes, err := pdf.NormalizeAndCount(r.Context(), tmpInputName, outputPath, copies, duplexMode)
	if err != nil {
		_ = os.Remove(outputPath)
		http.Error(w, fmt.Sprintf("normalization failed: %v", err), http.StatusBadRequest)
		return
	}

	pricePerSheet, err := s.db.GetPricePerSheetPaise(r.Context())
	if err != nil || pricePerSheet <= 0 {
		pricePerSheet = s.cfg.pricePerSheetPaise
	}
	totalPricePaise, err := payments.CalculatePricePaise(normRes.TotalSheets, pricePerSheet)
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
		"total_pages":           normRes.TotalPages,
		"total_sheets":          normRes.TotalSheets,
		"copies":                copies,
		"duplex":                string(duplexMode),
		"price_per_sheet_paise": pricePerSheet,
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

	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		http.Error(w, "order not found", http.StatusBadRequest)
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

	dbPath := os.Getenv("XEROXGO_CLOUD_DB")
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

	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		log.Fatalf("failed to create db directory: %v", err)
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
		cfg: cfg,
		db:  storeDB,
		hub: hub,
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

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
