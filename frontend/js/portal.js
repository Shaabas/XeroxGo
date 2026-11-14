/**
 * XeroxGo Mobile Upload & Print Portal
 * Responsive Mobile UX matching reference design:
 * 1. Files Cart (Multi-file upload, document cards, delete, preview, edit settings, Place Order)
 * 2. Document Settings (Page selection, copies stepper, B&W/Color, single/double sided, orientation, 1-up/2-up)
 * 3. Print Preview (PDF.js canvas rendering, Sheet X of Y (Front/Back) pager, Flip Sheet action)
 * 4. Checkout/PIN (4 large PIN boxes, 15m expiration, Copy PIN, order summary, live kiosk status)
 */

(function () {
  'use strict';

  // --- Configuration from DOM ---
  const container = (typeof document !== 'undefined') ? (document.getElementById('app-container') || document.querySelector('.app-container')) : null;
  if (!container) {
    if (typeof module !== 'undefined' && module.exports) {
      module.exports = { convertDocumentOnServer };
    }
    return;
  }

  let kioskID = container.dataset.kioskId || 'kiosk-demo';
  const pathMatch = window.location.pathname.match(/\/k\/([^/]+)/);
  const searchParams = new URLSearchParams(window.location.search);
  const urlKiosk = (pathMatch && pathMatch[1]) || searchParams.get('kiosk_id');
  if (urlKiosk) {
    kioskID = urlKiosk;
  }
  const devMode = container.dataset.devMode === 'true' || Boolean(window.__PORTAL_TEST__);

  // Rates in paise (Authoritative default fixed rates in code)
  // 3 for single side b&w (300p)
  // 5 for double side b&w (500p per 2 pages)
  // 8 for single side colour (800p)
  // 10 for double side colour (1000p per 2 pages)
  const RATE_BW_SINGLE_PAISE = 300;
  const RATE_BW_DOUBLE_PAISE = 500;
  const RATE_COLOR_SINGLE_PAISE = 800;
  const RATE_COLOR_DOUBLE_PAISE = 1000;

  // --- Views DOM ---
  const viewCart = document.getElementById('view-cart');
  const viewSettings = document.getElementById('view-settings');
  const viewPreview = document.getElementById('view-preview');
  const viewCheckout = document.getElementById('view-checkout');

  // View 1 (Cart) DOM
  const cartUploadCard = document.getElementById('cart-upload-card');
  const fileInput = document.getElementById('file-input');
  const uploadProcessingIndicator = document.getElementById('upload-processing-indicator');
  const uploadProcessingText = document.getElementById('upload-processing-text');
  const cartAlert = document.getElementById('cart-alert');
  const cartDocList = document.getElementById('cart-doc-list');
  const cartEmptyPlaceholder = document.getElementById('cart-empty-placeholder');
  const cartBottomBar = document.getElementById('cart-bottom-bar');
  const cartDocsCount = document.getElementById('cart-docs-count');
  const cartTotalAmount = document.getElementById('cart-total-amount');
  const btnReviewOrder = document.getElementById('btn-review-order');

  // Banner Carousel DOM
  const bannerTrack = document.getElementById('banner-track');
  const bannerDots = document.getElementById('banner-dots');
  const bannerCloseBtn = document.getElementById('banner-close-btn');
  const promoBanner = document.getElementById('promo-banner');

  // PWA DOM
  const pwaInstallBanner = document.getElementById('pwa-install-banner');
  const pwaDismissBtn = document.getElementById('pwa-dismiss-btn');
  const pwaInstallBtn = document.getElementById('pwa-install-btn');
  const pwaMiniPill = document.getElementById('pwa-mini-pill');
  const pwaPillBtn = document.getElementById('pwa-pill-btn');
  const pwaGuideModal = document.getElementById('pwa-guide-modal');
  const pwaModalCloseBtn = document.getElementById('pwa-modal-close-btn');
  const pwaModalGotItBtn = document.getElementById('pwa-modal-gotit-btn');
  let deferredPwaPrompt = null;

  // View 2 (Settings) DOM
  const btnSettingsBack = document.getElementById('btn-settings-back');
  const settingsFilename = document.getElementById('settings-filename');
  const btnSettingsMenu = document.getElementById('btn-settings-menu');
  const settingsMenuDropdown = document.getElementById('settings-menu-dropdown');
  const menuOptReset = document.getElementById('menu-opt-reset');
  const menuOptDelete = document.getElementById('menu-opt-delete');

  const btnOptAll = document.getElementById('btn-opt-all') || document.getElementById('btn-opt-all-pages');
  const btnOptRange = document.getElementById('btn-opt-range') || document.getElementById('btn-opt-custom-range');
  const btnOptSelection = document.getElementById('btn-opt-selection');
  const btnOptAllPages = btnOptAll;
  const btnOptCustomRange = btnOptRange;

  const settingsRangeBoxes = document.getElementById('settings-range-boxes');
  const inputRangeFrom = document.getElementById('input-range-from');
  const inputRangeTo = document.getElementById('input-range-to');
  const rangeErrorText = document.getElementById('range-error-text');

  const settingsSelectionControls = document.getElementById('settings-selection-controls');
  const btnSelectCheckAll = document.getElementById('btn-select-check-all');
  const btnSelectUncheckAll = document.getElementById('btn-select-uncheck-all');
  const selectionCountSummary = document.getElementById('selection-count-summary');
  const selectionErrorText = document.getElementById('selection-error-text');

  const selectionSeatsGrid = document.getElementById('selection-seats-grid');

  const settingsCustomRangeWrapper = document.getElementById('settings-custom-range-wrapper');
  const inputCustomRange = document.getElementById('input-custom-range');
  const customRangeError = document.getElementById('custom-range-error');

  const btnCopiesMinus = document.getElementById('btn-copies-minus');
  const btnCopiesPlus = document.getElementById('btn-copies-plus');
  const copiesCountDisplay = document.getElementById('copies-count-display');

  const cardModeBw = document.getElementById('card-mode-bw');
  const cardModeColor = document.getElementById('card-mode-color');
  const cardSideSingle = document.getElementById('card-side-single');
  const cardSideDouble = document.getElementById('card-side-double');
  const settingsSinglePriceLabel = document.getElementById('settings-single-price-label');
  const settingsDoublePriceLabel = document.getElementById('settings-double-price-label');

  const btnOrientAuto = document.getElementById('btn-orient-auto');
  const btnOrientPortrait = document.getElementById('btn-orient-portrait');
  const btnOrientLandscape = document.getElementById('btn-orient-landscape');

  const cardNup1 = document.getElementById('card-nup-1');
  const cardNup2 = document.getElementById('card-nup-2');
  const cardNup4 = document.getElementById('card-nup-4');
  const cardNup6 = document.getElementById('card-nup-6');

  const btnSaveDocSettings = document.getElementById('btn-save-doc-settings');
  const saveSettingsPrice = document.getElementById('save-settings-price');

  // View 3 (Preview) DOM
  const btnPreviewBack = document.getElementById('btn-preview-back');
  const previewFilename = document.getElementById('preview-filename');
  const btnPreviewMenu = document.getElementById('btn-preview-menu');
  const previewMenuDropdown = document.getElementById('preview-menu-dropdown');
  const previewMenuSettings = document.getElementById('preview-menu-settings');

  const previewLoadingSpinner = document.getElementById('preview-loading-spinner');
  const previewErrorBox = document.getElementById('preview-error-box');
  const previewErrorMsg = document.getElementById('preview-error-msg');
  const btnRetryPreview = document.getElementById('btn-retry-preview');
  const previewCanvas = document.getElementById('preview-canvas');
  const btnSheetLeft = document.getElementById('btn-sheet-left');
  const btnSheetRight = document.getElementById('btn-sheet-right');
  const previewSheetIndicator = document.getElementById('preview-sheet-indicator');
  const previewSideToggle = document.getElementById('preview-side-toggle');
  const toggleSideFront = document.getElementById('toggle-side-front');
  const toggleSideBack = document.getElementById('toggle-side-back');
  const btnSheetFlip = document.getElementById('btn-sheet-flip');

  // View 4 (Checkout) DOM
  const pinBoxes = [
    document.getElementById('pin-box-0'),
    document.getElementById('pin-box-1'),
    document.getElementById('pin-box-2'),
    document.getElementById('pin-box-3')
  ];
  const pinExpiryText = document.getElementById('pin-expiry-text');
  const btnCopyPin = document.getElementById('btn-copy-pin');
  const btnCopyPinLabel = document.getElementById('btn-copy-pin-label');
  const checkoutDocsCount = document.getElementById('checkout-docs-count');
  const checkoutTotalAmount = document.getElementById('checkout-total-amount');
  const checkoutLiveStatusBox = document.getElementById('checkout-live-status-box');
  const checkoutStatusPill = document.getElementById('checkout-status-pill');
  const checkoutProgressBar = document.getElementById('checkout-progress-bar');
  const checkoutStatusMsg = document.getElementById('checkout-status-msg');
  const btnStartNewOrder = document.getElementById('btn-start-new-order');

  // --- Application State ---
  let cartItems = [];
  let activeSettingsDocId = null;
  let draftSettings = null;

  let activePreviewDocId = null;
  let currentPreviewSheet = 1;
  let currentPreviewSide = 'front';
  let activeRenderTask = null;

  let activeOrderId = null;
  let activeDisplayCode = '';
  let activeRecoveryToken = '';
  let orderPollTimer = null;
  let expiryCountdownTimer = null;

  // --- Fast Client-Side Preview Page Helpers ---
  function getPreviewPageList(doc, settings) {
    if (!doc) return [];
    const s = settings || doc.settings;
    if (!s) return Array.from({ length: doc.totalPages }, (_, i) => i + 1);

    if (s.pageSelection === 'Custom Range') {
      const from = Math.max(1, Math.min(s.rangeFrom || 1, doc.totalPages));
      const to = Math.max(from, Math.min(s.rangeTo || doc.totalPages, doc.totalPages));
      const pages = [];
      for (let p = from; p <= to; p++) pages.push(p);
      return pages;
    } else if (s.pageSelection === 'Selection') {
      return Array.isArray(s.selectedPages)
        ? s.selectedPages.slice().sort((a, b) => a - b)
        : [];
    } else if (s.customPageRange) {
      const parsed = parseCustomPages(s.customPageRange, doc.totalPages);
      return (parsed.valid && parsed.pages.length > 0)
        ? parsed.pages
        : Array.from({ length: doc.totalPages }, (_, i) => i + 1);
    }
    return Array.from({ length: doc.totalPages }, (_, i) => i + 1);
  }

  function getPreviewTotalSheets(doc, settings) {
    if (!doc) return 1;
    const s = settings || doc.settings;
    const pages = getPreviewPageList(doc, s);
    if (pages.length === 0) return 0;
    const isDuplex = (s.sides === 'Double-sided');
    const nup = parseInt(s.pagesPerSheet, 10) || 1;
    return Math.max(1, isDuplex ? Math.ceil(pages.length / (nup * 2)) : Math.ceil(pages.length / nup));
  }

  // --- Formatting Helpers ---
  function formatRupees(paise) {
    if (typeof paise !== 'number' || isNaN(paise) || paise < 0) return '₹0.00';
    return '₹' + (paise / 100).toFixed(2);
  }

  function escapeHtml(str) {
    if (!str) return '';
    return str.replace(/[&<>'"]/g, tag => ({
      '&': '&amp;',
      '<': '&lt;',
      '>': '&gt;',
      "'": '&#39;',
      '"': '&quot;'
    }[tag] || tag));
  }

  // Parses ranges like "1-3, 5" into 1-based page numbers [1, 2, 3, 5]
  function parseCustomPages(rangeStr, maxPages) {
    if (!rangeStr || !rangeStr.trim()) {
      return { valid: false, error: 'Please enter a page range (e.g. 1-3, 5)', pages: [] };
    }
    const parts = rangeStr.split(',');
    const selected = [];
    const seen = new Set();

    for (let part of parts) {
      part = part.trim();
      if (!part) continue;

      if (part.includes('-')) {
        const segs = part.split('-');
        if (segs.length !== 2) {
          return { valid: false, error: `Invalid range format: "${part}"`, pages: [] };
        }
        const start = parseInt(segs[0].trim(), 10);
        const end = parseInt(segs[1].trim(), 10);
        if (isNaN(start) || isNaN(end) || start < 1 || start > end) {
          return { valid: false, error: `Invalid range bounds: "${part}"`, pages: [] };
        }
        if (end > maxPages) {
          return { valid: false, error: `Page ${end} exceeds total pages (${maxPages})`, pages: [] };
        }
        for (let i = start; i <= end; i++) {
          if (!seen.has(i)) {
            seen.add(i);
            selected.push(i);
          }
        }
      } else {
        const p = parseInt(part, 10);
        if (isNaN(p) || p < 1) {
          return { valid: false, error: `Invalid page number: "${part}"`, pages: [] };
        }
        if (p > maxPages) {
          return { valid: false, error: `Page ${p} exceeds total pages (${maxPages})`, pages: [] };
        }
        if (!seen.has(p)) {
          seen.add(p);
          selected.push(p);
        }
      }
    }

    if (selected.length === 0) {
      return { valid: false, error: 'No valid pages selected', pages: [] };
    }
    return { valid: true, error: null, pages: selected };
  }

  // Per-document price calculation in integer paise
  // Rates:
  // - 3 for single side b&w (₹3 per page)
  // - 5 for double side b&w (₹5 per 2 pages)
  // - 8 for single side colour (₹8 per page)
  // - 10 for double side colour (₹10 per 2 pages)
  // Odd trailing page on double side: charged at single side rate (₹3 for B&W, ₹8 for Colour)
  function calculateDocPrice(totalPages, settings) {
    if (!settings || totalPages <= 0) return 0;
    let pagesCount = totalPages;
    if (settings.pageSelection === 'Custom Range') {
      const from = Math.max(1, Math.min(settings.rangeFrom || 1, totalPages));
      const to = Math.max(from, Math.min(settings.rangeTo || totalPages, totalPages));
      pagesCount = Math.max(1, to - from + 1);
    } else if (settings.pageSelection === 'Selection') {
      const sel = Array.isArray(settings.selectedPages) ? settings.selectedPages : [];
      pagesCount = sel.length;
    } else if (settings.customPageRange) {
      const parsed = parseCustomPages(settings.customPageRange, totalPages);
      if (parsed.valid && parsed.pages.length > 0) {
        pagesCount = parsed.pages.length;
      }
    }

    if (pagesCount <= 0) return 0;

    const nup = parseInt(settings.pagesPerSheet, 10) || 1;
    const imposedPages = Math.ceil(pagesCount / nup);
    const isColor = (settings.mode === 'Color');
    const isDuplex = (settings.sides === 'Double-sided');
    const copies = Math.max(1, parseInt(settings.copies, 10) || 1);

    let singleCopyPricePaise = 0;
    if (isDuplex) {
      const fullSheets = Math.floor(imposedPages / 2);
      const hasOddPage = (imposedPages % 2 !== 0);
      if (isColor) {
        singleCopyPricePaise = (fullSheets * RATE_COLOR_DOUBLE_PAISE) + (hasOddPage ? RATE_COLOR_SINGLE_PAISE : 0);
      } else {
        singleCopyPricePaise = (fullSheets * RATE_BW_DOUBLE_PAISE) + (hasOddPage ? RATE_BW_SINGLE_PAISE : 0);
      }
    } else {
      if (isColor) {
        singleCopyPricePaise = imposedPages * RATE_COLOR_SINGLE_PAISE;
      } else {
        singleCopyPricePaise = imposedPages * RATE_BW_SINGLE_PAISE;
      }
    }

    return singleCopyPricePaise * copies;
  }

  function getDocSelectedPages(totalPages, settings) {
    if (!settings) return Array.from({ length: totalPages }, (_, i) => i + 1);
    if (settings.pageSelection === 'Custom Range') {
      const from = Math.max(1, Math.min(settings.rangeFrom || 1, totalPages));
      const to = Math.max(from, Math.min(settings.rangeTo || totalPages, totalPages));
      const pages = [];
      for (let p = from; p <= to; p++) pages.push(p);
      return pages;
    } else if (settings.pageSelection === 'Selection') {
      const sel = Array.isArray(settings.selectedPages) ? settings.selectedPages : [];
      return sel.slice().sort((a, b) => a - b);
    } else if (settings.customPageRange) {
      const parsed = parseCustomPages(settings.customPageRange, totalPages);
      if (parsed.valid && parsed.pages.length > 0) {
        return parsed.pages;
      }
    }
    return Array.from({ length: totalPages }, (_, i) => i + 1);
  }

  // --- View Navigation ---
  function switchView(viewName, docId) {
    [viewCart, viewSettings, viewPreview, viewCheckout].forEach(v => {
      if (v) {
        v.classList.remove('active-view');
        v.classList.add('hidden');
      }
    });

    window.scrollTo({ top: 0, behavior: 'smooth' });

    if (viewName === 'cart') {
      if (viewCart) {
        viewCart.classList.remove('hidden');
        viewCart.classList.add('active-view');
      }
      renderCart();
    } else if (viewName === 'settings' || viewName === 'preview') {
      if (viewSettings) {
        viewSettings.classList.remove('hidden');
        viewSettings.classList.add('active-view');
      }
      openSettings(docId);
    } else if (viewName === 'checkout') {
      if (viewCheckout) {
        viewCheckout.classList.remove('hidden');
        viewCheckout.classList.add('active-view');
      }
    }
  }

  // --- View 1: Cart Implementation ---
  function formatFileSize(bytes) {
    if (!bytes || bytes <= 0) return '';
    if (bytes < 1024) return bytes + ' B';
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
    return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
  }

  function renderCart() {
    if (!cartDocList) return;

    if (cartItems.length === 0) {
      cartDocList.innerHTML = '';
      if (cartEmptyPlaceholder) cartEmptyPlaceholder.classList.remove('hidden');
      if (cartBottomBar) cartBottomBar.classList.add('hidden');
      return;
    }

    if (cartEmptyPlaceholder) cartEmptyPlaceholder.classList.add('hidden');
    if (cartBottomBar) cartBottomBar.classList.remove('hidden');

    let totalCartPaise = 0;
    let readyCount = 0;
    let processingCount = 0;

    cartDocList.innerHTML = '';

    cartItems.forEach((item) => {
      const card = document.createElement('div');
      card.className = 'doc-card';
      card.dataset.id = item.id;

      const isImage = item.isImage;
      const iconSvg = isImage
        ? `<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="18" height="18" x="3" y="3" rx="2" ry="2"/><circle cx="9" cy="9" r="2"/><path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21"/></svg>`
        : `<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14.5 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7.5L14.5 2z"/><polyline points="14 2 14 8 20 8"/><line x1="16" y1="13" x2="8" y2="13"/><line x1="16" y1="17" x2="8" y2="17"/><line x1="10" y1="9" x2="8" y2="9"/></svg>`;

      if (item.status === 'processing') {
        processingCount++;
        card.innerHTML = `
          <div class="doc-card-top">
            <div class="doc-icon-box">${iconSvg}</div>
            <div class="doc-info">
              <h4 class="doc-name" title="${escapeHtml(item.name)}">${escapeHtml(item.name)}</h4>
              <div class="doc-meta">
                <span>${formatFileSize(item.size)}</span>
              </div>
              <div class="doc-status-badge badge-processing">
                <div class="spinner-icon-sm"></div>
                <span class="doc-status-text">${escapeHtml(item.statusText || 'Processing document...')}</span>
              </div>
              <div class="doc-progress-track">
                <div class="doc-progress-bar-indeterminate"></div>
              </div>
            </div>
            <div class="doc-price doc-price-calculating">--</div>
          </div>
          <div class="doc-card-bottom">
            <button type="button" class="btn-doc-delete" data-id="${item.id}">
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round">
                <line x1="18" y1="6" x2="6" y2="18"/>
                <line x1="6" y1="6" x2="18" y2="18"/>
              </svg>
              Cancel
            </button>
            <span class="doc-processing-hint">Preparing document...</span>
          </div>
        `;

        card.querySelector('.btn-doc-delete').addEventListener('click', (e) => {
          e.stopPropagation();
          removeDoc(item.id);
        });
      } else if (item.status === 'error') {
        card.classList.add('doc-card-error');
        card.innerHTML = `
          <div class="doc-card-top">
            <div class="doc-icon-box" style="background:#fee2e2; color:#dc2626;">
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg>
            </div>
            <div class="doc-info">
              <h4 class="doc-name" title="${escapeHtml(item.name)}">${escapeHtml(item.name)}</h4>
              <div class="doc-status-badge badge-error">
                <span>${escapeHtml(item.errorMessage || 'Failed to process file')}</span>
              </div>
            </div>
            <div class="doc-price" style="color:#dc2626; font-size:14px; font-weight:600;">Error</div>
          </div>
          <div class="doc-card-bottom">
            <button type="button" class="btn-doc-delete" data-id="${item.id}">
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <path d="M3 6h18"/>
                <path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6"/>
                <path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2"/>
                <line x1="10" y1="11" x2="10" y2="17"/>
                <line x1="14" y1="11" x2="14" y2="17"/>
              </svg>
              Remove
            </button>
            <div class="doc-actions-right">
              <button type="button" class="btn-doc-retry" data-id="${item.id}">
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round">
                  <path d="M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8"/>
                  <path d="M21 3v5h-5"/>
                </svg>
                Retry
              </button>
            </div>
          </div>
        `;

        card.querySelector('.btn-doc-delete').addEventListener('click', (e) => {
          e.stopPropagation();
          removeDoc(item.id);
        });
        card.querySelector('.btn-doc-retry').addEventListener('click', (e) => {
          e.stopPropagation();
          retryDoc(item.id);
        });
      } else {
        // ready
        readyCount++;
        const hasAuthPrice = (typeof item.authoritativePricePaise === 'number' && !isNaN(item.authoritativePricePaise));
        const itemPrice = hasAuthPrice ? item.authoritativePricePaise : calculateDocPrice(item.totalPages, item.settings);
        item.pricePaise = itemPrice;
        totalCartPaise += itemPrice;

        let pageCountText = `${item.totalPages} page${item.totalPages > 1 ? 's' : ''}`;
        if (item.settings.pageSelection === 'Custom Range') {
          const from = item.settings.rangeFrom || 1;
          const to = item.settings.rangeTo || item.totalPages;
          const count = Math.max(1, to - from + 1);
          pageCountText = `${count} page${count > 1 ? 's' : ''} (${from}-${to})`;
        } else if (item.settings.pageSelection === 'Selection') {
          const count = (item.settings.selectedPages || []).length;
          pageCountText = `${count} of ${item.totalPages} page${item.totalPages > 1 ? 's' : ''}`;
        }

        const sidesText = item.settings.sides === 'Double-sided' ? 'Double Side' : 'Single Side';
        let metaText = `${pageCountText} · ${item.settings.mode} · ${sidesText}`;
        const nup = parseInt(item.settings.pagesPerSheet, 10) || 1;
        if (nup > 1) {
          metaText += ` · ${nup} on 1`;
        }
        if (item.settings.orientation && item.settings.orientation !== 'Auto') {
          metaText += ` · ${item.settings.orientation}`;
        }
        if (item.settings.copies && item.settings.copies > 1) {
          metaText += ` · ${item.settings.copies} copies`;
        }
        if (item.totalSheets) {
          metaText += ` · ${item.totalSheets} sheet${item.totalSheets > 1 ? 's' : ''}`;
        }

        const priceDisplayHtml = formatRupees(itemPrice);

        card.innerHTML = `
          <div class="doc-card-top">
            <div class="doc-icon-box">${iconSvg}</div>
            <div class="doc-info">
              <h4 class="doc-name" title="${escapeHtml(item.name)}">${escapeHtml(item.name)}</h4>
              <div class="doc-meta">${metaText}</div>
            </div>
          </div>
          <div class="doc-card-bottom">
            <button type="button" class="btn-doc-delete" data-id="${item.id}">
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <path d="M3 6h18"/>
                <path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6"/>
                <path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2"/>
                <line x1="10" y1="11" x2="10" y2="17"/>
                <line x1="14" y1="11" x2="14" y2="17"/>
              </svg>
              Delete
            </button>
            <div class="doc-price">${priceDisplayHtml}</div>
            <div class="doc-actions-right">
              <button type="button" class="btn-doc-edit" data-id="${item.id}">
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round">
                  <line x1="4" y1="21" x2="4" y2="14"/><line x1="4" y1="10" x2="4" y2="3"/><line x1="12" y1="21" x2="12" y2="12"/><line x1="12" y1="8" x2="12" y2="3"/><line x1="20" y1="21" x2="20" y2="16"/><line x1="20" y1="12" x2="20" y2="3"/><line x1="1" y1="14" x2="7" y2="14"/><line x1="9" y1="8" x2="15" y2="8"/><line x1="17" y1="16" x2="23" y2="16"/>
                </svg>
                Edit print settings
              </button>
            </div>
          </div>
        `;

        const btnDelete = card.querySelector('.btn-doc-delete');
        if (btnDelete) {
          btnDelete.addEventListener('click', (e) => {
            e.stopPropagation();
            removeDoc(item.id);
          });
        }

        const btnEdit = card.querySelector('.btn-doc-edit');
        if (btnEdit) {
          btnEdit.addEventListener('click', (e) => {
            e.stopPropagation();
            switchView('settings', item.id);
          });
        }
      }

      cartDocList.appendChild(card);
    });

    if (cartDocsCount) {
      cartDocsCount.textContent = `${cartItems.length} document${cartItems.length > 1 ? 's' : ''}`;
    }
    const isProcessing = cartItems.some(it => it.status === 'processing');
    if (cartTotalAmount) {
      if (isProcessing && readyCount === 0) {
        cartTotalAmount.innerHTML = `<span class="price-loading-dots" role="status" aria-label="Calculating price"><span class="price-dot"></span><span class="price-dot"></span><span class="price-dot"></span></span>`;
      } else if (readyCount > 0 && totalCartPaise > 0) {
        cartTotalAmount.textContent = formatRupees(totalCartPaise);
      } else if (readyCount === 0 && cartItems.length > 0) {
        cartTotalAmount.textContent = '--';
      } else {
        cartTotalAmount.textContent = formatRupees(0);
      }
    }

    if (btnReviewOrder) {
      btnReviewOrder.innerHTML = `
        <span>Place Order</span>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="5" y1="12" x2="19" y2="12"/><polyline points="12 5 19 12 12 19"/></svg>
      `;
      const canProceed = !isProcessing && readyCount > 0 && totalCartPaise > 0;
      btnReviewOrder.disabled = !canProceed;
    }
  }

  function updateCardDOM(docId) {
    const item = cartItems.find(it => it.id === docId);
    if (!item) return;
    const card = cartDocList ? cartDocList.querySelector(`.doc-card[data-id="${docId}"]`) : null;
    if (!card) return;

    const statusTextSpan = card.querySelector('.doc-status-text');
    if (statusTextSpan && item.statusText) {
      statusTextSpan.textContent = item.statusText;
    }
  }

  function removeDoc(docId) {
    const item = cartItems.find(it => it.id === docId);
    if (item && item.abortController) {
      try {
        item.abortController.abort();
      } catch (_) { }
    }
    cartItems = cartItems.filter(item => item.id !== docId);
    renderCart();
  }

  function retryDoc(docId) {
    const item = cartItems.find(it => it.id === docId);
    if (!item) return;
    item.status = 'processing';
    item.statusText = item.isOffice ? 'Preparing conversion...' : item.isImage ? 'Processing image...' : 'Reading PDF...';
    item.errorMessage = '';
    item.abortController = new AbortController();
    renderCart();
    processDocItem(item);
  }

  // --- Document Ingestion & Conversion ---
  const officeExtensions = ['.doc', '.docx', '.ppt', '.pptx', '.xls', '.xlsx', '.odt', '.ods', '.odp', '.rtf', '.txt'];
  const imageExtensions = ['.jpg', '.jpeg', '.png', '.webp', '.bmp'];

  async function convertImageToPdf(selectedFile) {
    if (!window.PDFLib) {
      throw new Error('PDF processing library not loaded.');
    }
    const pdfDoc = await window.PDFLib.PDFDocument.create();
    const page = pdfDoc.addPage([595.28, 841.89]); // A4 (72 DPI)

    const nameLower = selectedFile.name.toLowerCase();
    const isJpg = nameLower.endsWith('.jpg') || nameLower.endsWith('.jpeg') || selectedFile.type === 'image/jpeg';
    const isPng = nameLower.endsWith('.png') || selectedFile.type === 'image/png';

    let embeddedImage;
    if (isJpg) {
      const bytes = await selectedFile.arrayBuffer();
      embeddedImage = await pdfDoc.embedJpg(bytes);
    } else if (isPng) {
      const bytes = await selectedFile.arrayBuffer();
      embeddedImage = await pdfDoc.embedPng(bytes);
    } else {
      const dataUrl = await new Promise((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(reader.result);
        reader.onerror = reject;
        reader.readAsDataURL(selectedFile);
      });
      const img = new Image();
      await new Promise((resolve, reject) => {
        img.onload = resolve;
        img.onerror = reject;
        img.src = dataUrl;
      });
      const canvas = document.createElement('canvas');
      canvas.width = img.width;
      canvas.height = img.height;
      const ctx = canvas.getContext('2d');
      ctx.drawImage(img, 0, 0);
      const pngDataUrl = canvas.toDataURL('image/png');
      const pngBytes = Uint8Array.from(atob(pngDataUrl.split(',')[1]), c => c.charCodeAt(0));
      embeddedImage = await pdfDoc.embedPng(pngBytes);
    }

    const margin = 36;
    const maxWidth = 595.28 - (margin * 2);
    const maxHeight = 841.89 - (margin * 2);
    const { width, height } = embeddedImage.scaleToFit(maxWidth, maxHeight);

    page.drawImage(embeddedImage, {
      x: (595.28 - width) / 2,
      y: (841.89 - height) / 2,
      width,
      height,
    });

    return await pdfDoc.save();
  }

  async function convertDocumentOnServer(selectedFile, signal, onStatusUpdate) {
    const formData = new FormData();
    formData.append('file', selectedFile);

    let lastStatusMsg = '';
    const reportStatus = (msg) => {
      if (onStatusUpdate && msg !== lastStatusMsg) {
        lastStatusMsg = msg;
        onStatusUpdate(msg);
      }
    };

    // 1. Initial Job Submission (POST /api/conversions)
    reportStatus('Uploading for conversion...');

    let resp;
    try {
      resp = await fetch('/api/conversions', { method: 'POST', body: formData, signal });
    } catch (err) {
      if (signal && signal.aborted) throw new Error('Cancelled');
      // Do not retry POST or fall back to sync endpoint. Ambiguous network failures
      // must not trigger duplicate conversion jobs.
      throw new Error('Network error while uploading document for conversion: ' + (err.message || 'connection failed'));
    }

    if (!resp.ok) {
      let errDetail = '';
      try {
        errDetail = await resp.text();
      } catch (_) { }

      if (resp.status === 413) {
        throw new Error('File exceeds server size limit (25 MiB)');
      } else if (resp.status === 400) {
        throw new Error(errDetail || 'Unsupported file format or invalid document');
      } else if (resp.status >= 500) {
        throw new Error('Conversion service error on server: ' + (errDetail || 'internal error'));
      }
      throw new Error(errDetail || `Conversion submission failed (HTTP ${resp.status})`);
    }

    let createData;
    try {
      createData = await resp.json();
    } catch (err) {
      throw new Error('Invalid JSON response received from conversion server');
    }

    const jobId = createData.job_id;
    const token = createData.token;
    if (!jobId || !token) {
      throw new Error('Conversion server response missing job_id or token');
    }

    const pollUrl = createData.poll_url || `/api/conversions/${encodeURIComponent(jobId)}?token=${encodeURIComponent(token)}`;
    let status = createData.status || 'queued';
    let downloadUrl = null;

    reportStatus('Waiting in conversion queue...');

    // 2. Status Polling Loop (GET pollUrl)
    const startTime = Date.now();
    const maxPollTimeMs = 90000; // 90 seconds
    const pollIntervalMs = 1200;
    let consecutiveNetworkErrors = 0;
    const maxConsecutiveNetworkErrors = 3;

    while (status === 'queued' || status === 'processing') {
      if (signal && signal.aborted) throw new Error('Cancelled');
      if (Date.now() - startTime > maxPollTimeMs) {
        throw new Error('Document conversion timed out after 90 seconds');
      }

      reportStatus(status === 'processing' ? 'Converting pages on server...' : 'Waiting in conversion queue...');

      await new Promise(r => setTimeout(r, pollIntervalMs));
      if (signal && signal.aborted) throw new Error('Cancelled');

      let pollResp;
      try {
        pollResp = await fetch(pollUrl, { signal });
        consecutiveNetworkErrors = 0; // reset on successful network exchange
      } catch (pollErr) {
        if (signal && signal.aborted) throw new Error('Cancelled');
        consecutiveNetworkErrors++;
        // Transient network glitch during polling: do NOT create duplicate job, retry status check
        if (consecutiveNetworkErrors <= maxConsecutiveNetworkErrors) {
          reportStatus('Connection interrupted, retrying status check...');
          continue;
        }
        throw new Error('Network error while checking conversion progress: ' + (pollErr.message || 'connection failed'));
      }

      if (pollResp.status === 404) {
        throw new Error('Conversion job expired or not found on server');
      }
      if (pollResp.status === 401 || pollResp.status === 403) {
        throw new Error('Unauthorized to query conversion job status');
      }
      if (!pollResp.ok) {
        let pollErrDetail = '';
        try { pollErrDetail = await pollResp.text(); } catch (_) { }
        throw new Error(`Failed to query conversion progress (HTTP ${pollResp.status}): ${pollErrDetail || pollResp.statusText}`);
      }

      let pollData;
      try {
        pollData = await pollResp.json();
      } catch (jsonErr) {
        throw new Error('Malformed JSON received while polling conversion progress');
      }

      status = pollData.status;
      if (status === 'completed') {
        downloadUrl = pollData.download_url;
        break;
      } else if (status === 'failed') {
        throw new Error(pollData.error_message || 'Conversion engine failed to convert document');
      } else if (status === 'expired') {
        throw new Error('Conversion job expired before completion');
      }
    }

    // 3. Artifact Download (GET downloadUrl)
    if (!downloadUrl) {
      downloadUrl = `/api/conversions/${encodeURIComponent(jobId)}/pdf?token=${encodeURIComponent(token)}`;
    }

    reportStatus('Downloading converted PDF...');

    let pdfResp;
    try {
      pdfResp = await fetch(downloadUrl, { signal });
    } catch (dlErr) {
      if (signal && signal.aborted) throw new Error('Cancelled');
      throw new Error('Network error while downloading converted PDF: ' + (dlErr.message || 'connection failed'));
    }

    if (!pdfResp.ok) {
      if (pdfResp.status === 404) {
        throw new Error('Converted PDF artifact not found or expired');
      }
      throw new Error(`Failed to download converted PDF (HTTP ${pdfResp.status})`);
    }

    return await pdfResp.arrayBuffer();
  }

  async function processDocItem(item) {
    const file = item.file;
    const nameLower = file.name.toLowerCase();
    const isOffice = officeExtensions.some(ext => nameLower.endsWith(ext));
    const isImage = imageExtensions.some(ext => nameLower.endsWith(ext)) || file.type.startsWith('image/');
    const isPdf = nameLower.endsWith('.pdf') || file.type === 'application/pdf';

    try {
      let pdfBytes = null;
      if (isPdf) {
        item.statusText = 'Reading PDF...';
        updateCardDOM(item.id);
        pdfBytes = await file.arrayBuffer();
      } else if (isImage) {
        item.statusText = 'Optimizing image...';
        updateCardDOM(item.id);
        pdfBytes = await convertImageToPdf(file);
      } else if (isOffice) {
        pdfBytes = await convertDocumentOnServer(file, item.abortController?.signal, (statusMsg) => {
          item.statusText = statusMsg;
          updateCardDOM(item.id);
        });
      }

      // Check if user removed the item while it was processing
      if (!cartItems.some(it => it.id === item.id)) {
        return;
      }

      item.statusText = 'Reading pages...';
      updateCardDOM(item.id);

      if (!window.pdfjsLib) {
        throw new Error('PDF viewing engine not initialized');
      }

      const loadingTask = window.pdfjsLib.getDocument({ data: pdfBytes.slice(0) });
      const pdfDoc = await loadingTask.promise;
      const totalPages = pdfDoc.numPages;

      if (!cartItems.some(it => it.id === item.id)) {
        return;
      }

      const defaultSides = totalPages > 1 ? 'Double-sided' : 'Single-sided';
      item.pdfBytes = pdfBytes;
      item.pdfDoc = pdfDoc;
      item.totalPages = totalPages;
      item.isImage = isImage;
      item.isOffice = isOffice;
      item.settings = {
        pageSelection: 'All',
        rangeFrom: 1,
        rangeTo: totalPages,
        selectedPages: Array.from({ length: totalPages }, (_, idx) => idx + 1),
        customPageRange: '',
        copies: 1,
        mode: 'B&W',
        sides: defaultSides,
        orientation: 'Auto',
        pagesPerSheet: '1'
      };
      item.selectedPages = Array.from({ length: totalPages }, (_, idx) => idx + 1);
      item.status = 'ready';
      item.statusText = '';
      item.previewPending = false;
      item.authoritativePricePaise = null;
      item.totalSheets = Math.ceil(totalPages / (defaultSides === 'Double-sided' ? 2 : 1));
      item.pricePaise = calculateDocPrice(totalPages, item.settings);
      renderCart();
    } catch (err) {
      if (item.abortController?.signal?.aborted) {
        return;
      }
      console.error('File processing error for', file.name, err);
      item.status = 'error';
      item.errorMessage = err.message || 'Processing failed';
    }

    renderCart();
  }

  async function handleFiles(files) {
    if (!files || files.length === 0) return;
    if (cartAlert) cartAlert.classList.add('hidden');

    const newItems = [];
    for (let i = 0; i < files.length; i++) {
      const file = files[i];
      const nameLower = file.name.toLowerCase();
      const isOffice = officeExtensions.some(ext => nameLower.endsWith(ext));
      const isImage = imageExtensions.some(ext => nameLower.endsWith(ext)) || file.type.startsWith('image/');
      const isPdf = nameLower.endsWith('.pdf') || file.type === 'application/pdf';

      if (!isPdf && !isOffice && !isImage) {
        showCartError(`Unsupported file format: ${file.name}`);
        continue;
      }

      const docId = 'doc_' + Date.now() + '_' + Math.random().toString(36).slice(2, 7) + '_' + i;
      const item = {
        id: docId,
        file: file,
        name: file.name,
        size: file.size,
        status: 'processing',
        statusText: isOffice ? 'Preparing conversion...' : isImage ? 'Optimizing image...' : 'Reading PDF...',
        errorMessage: '',
        abortController: new AbortController(),
        pdfBytes: null,
        pdfDoc: null,
        totalPages: 0,
        isImage: isImage,
        isOffice: isOffice,
        settings: {
          pageSelection: 'All',
          rangeFrom: 1,
          rangeTo: 1,
          selectedPages: [1],
          customPageRange: '',
          copies: 1,
          mode: 'B&W',
          sides: 'Single-sided',
          orientation: 'Auto',
          pagesPerSheet: '1'
        },
        selectedPages: [],
        pricePaise: 0,
        previewPending: false,
        authoritativePricePaise: null,
        totalSheets: 0,
        billedSides: 0,
        paddedPages: 0
      };

      cartItems.push(item);
      newItems.push(item);
    }

    // Immediately render all new cards into the DOM
    renderCart();

    // Process all newly added items concurrently
    newItems.forEach(item => {
      processDocItem(item);
    });
  }

  function showCartError(msg) {
    if (!cartAlert) return;
    cartAlert.textContent = msg;
    cartAlert.classList.remove('hidden');
    setTimeout(() => {
      cartAlert.classList.add('hidden');
    }, 6000);
  }

  // --- View 2: Document Settings & Preview Implementation ---
  function openSettings(docId) {
    activeSettingsDocId = docId;
    activePreviewDocId = docId;
    const doc = cartItems.find(d => d.id === docId);
    if (!doc) {
      switchView('cart');
      return;
    }

    draftSettings = JSON.parse(JSON.stringify(doc.settings));
    if (!draftSettings.pageSelection || draftSettings.pageSelection === 'All Pages') {
      draftSettings.pageSelection = 'All';
    }
    if (!draftSettings.rangeFrom) draftSettings.rangeFrom = 1;
    if (!draftSettings.rangeTo) draftSettings.rangeTo = doc.totalPages;
    if (!Array.isArray(draftSettings.selectedPages)) {
      draftSettings.selectedPages = Array.from({ length: doc.totalPages }, (_, idx) => idx + 1);
    }
    if (!draftSettings.pagesPerSheet) {
      draftSettings.pagesPerSheet = '1';
    }
    if (!draftSettings.orientation) {
      draftSettings.orientation = 'Auto';
    }

    if (settingsFilename) settingsFilename.textContent = doc.name;
    if (previewFilename) previewFilename.textContent = doc.name;

    currentPreviewSheet = 1;
    currentPreviewSide = 'front';

    // Dynamic rate labels on Single vs Double side based on Print Mode
    const isColor = (draftSettings.mode === 'Color');
    if (settingsSinglePriceLabel) settingsSinglePriceLabel.textContent = isColor ? '₹8.00/sheet' : '₹3.00/sheet';
    if (settingsDoublePriceLabel) settingsDoublePriceLabel.textContent = isColor ? '₹10.00/sheet' : '₹5.00/sheet';

    syncSettingsUI(doc);
    renderPreviewSheet();
  }

  function syncSelectionState(doc) {
    if (!doc || !draftSettings) return;
    const selCount = Array.isArray(draftSettings.selectedPages) ? draftSettings.selectedPages.length : 0;
    if (selectionCountSummary) {
      selectionCountSummary.textContent = `${selCount} of ${doc.totalPages} pages selected`;
    }
    if (selectionErrorText) {
      selectionErrorText.classList.toggle('hidden', selCount > 0);
    }

    // Sync seat button active classes
    if (selectionSeatsGrid) {
      const seatBtns = selectionSeatsGrid.querySelectorAll('.seat-btn');
      seatBtns.forEach(btn => {
        const p = parseInt(btn.dataset.page, 10);
        const isSel = Array.isArray(draftSettings.selectedPages) && draftSettings.selectedPages.includes(p);
        btn.classList.toggle('active', isSel);
        btn.setAttribute('aria-pressed', isSel ? 'true' : 'false');
      });
    }

    const livePrice = calculateDocPrice(doc.totalPages, draftSettings);
    if (saveSettingsPrice) {
      saveSettingsPrice.textContent = formatRupees(livePrice);
    }
  }

  function renderSelectionSeats(doc) {
    if (!selectionSeatsGrid || !doc || !draftSettings) return;
    selectionSeatsGrid.innerHTML = '';

    if (!Array.isArray(draftSettings.selectedPages)) {
      draftSettings.selectedPages = Array.from({ length: doc.totalPages }, (_, i) => i + 1);
    }

    const isDuplex = (draftSettings.sides === 'Double-sided');
    let currentViewPage = null;
    if (!isDuplex) {
      currentViewPage = currentPreviewSheet;
    } else {
      currentViewPage = (currentPreviewSide === 'front')
        ? (currentPreviewSheet - 1) * 2 + 1
        : (currentPreviewSheet - 1) * 2 + 2;
    }

    for (let p = 1; p <= doc.totalPages; p++) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'seat-btn';
      btn.dataset.page = String(p);
      btn.textContent = String(p);
      btn.title = `Page ${p}`;

      const isSelected = draftSettings.selectedPages.includes(p);
      if (isSelected) {
        btn.classList.add('active');
        btn.setAttribute('aria-pressed', 'true');
      } else {
        btn.setAttribute('aria-pressed', 'false');
      }

      if (p === currentViewPage) {
        btn.classList.add('current-view');
      }

      btn.addEventListener('click', (e) => {
        e.stopPropagation();
        const pageNum = parseInt(btn.dataset.page, 10);
        const alreadySelected = draftSettings.selectedPages.includes(pageNum);
        if (alreadySelected) {
          draftSettings.selectedPages = draftSettings.selectedPages.filter(it => it !== pageNum);
          btn.classList.remove('active');
          btn.setAttribute('aria-pressed', 'false');
        } else {
          draftSettings.selectedPages.push(pageNum);
          draftSettings.selectedPages.sort((a, b) => a - b);
          btn.classList.add('active');
          btn.setAttribute('aria-pressed', 'true');
        }

        // Navigate preview canvas to this page if selected
        const sortedPages = draftSettings.selectedPages.slice().sort((a, b) => a - b);
        if (!alreadySelected) {
          const idx = sortedPages.indexOf(pageNum);
          if (idx !== -1) {
            if (!isDuplex) {
              currentPreviewSheet = idx + 1;
            } else {
              currentPreviewSheet = Math.floor(idx / 2) + 1;
              currentPreviewSide = (idx % 2 === 0) ? 'front' : 'back';
            }
          }
        }

        syncSelectionState(doc);
        renderPreviewSheet();
      });

      selectionSeatsGrid.appendChild(btn);
    }
  }

  function updateRangeBoxes(doc) {
    if (!doc || !draftSettings) return;
    let from = parseInt(inputRangeFrom?.value, 10);
    let to = parseInt(inputRangeTo?.value, 10);

    if (isNaN(from) || from < 1) from = 1;
    if (from > doc.totalPages) from = doc.totalPages;
    if (isNaN(to) || to < from) to = from;
    if (to > doc.totalPages) to = doc.totalPages;

    if (inputRangeFrom) inputRangeFrom.value = from;
    if (inputRangeTo) inputRangeTo.value = to;

    draftSettings.rangeFrom = from;
    draftSettings.rangeTo = to;
    draftSettings.customPageRange = (from === to) ? String(from) : `${from}-${to}`;

    if (rangeErrorText) {
      rangeErrorText.classList.add('hidden');
    }

    const livePrice = calculateDocPrice(doc.totalPages, draftSettings);
    if (saveSettingsPrice) {
      saveSettingsPrice.textContent = formatRupees(livePrice);
    }
  }

  function syncSettingsUI(doc) {
    // 1. Page Selection (3 Options: All, Custom Range, Selection)
    const isAll = (!draftSettings.pageSelection || draftSettings.pageSelection === 'All' || draftSettings.pageSelection === 'All Pages');
    const isRange = (draftSettings.pageSelection === 'Custom Range');
    const isSelection = (draftSettings.pageSelection === 'Selection');

    if (btnOptAll) btnOptAll.classList.toggle('active', isAll);
    if (btnOptRange) btnOptRange.classList.toggle('active', isRange);
    if (btnOptSelection) btnOptSelection.classList.toggle('active', isSelection);

    if (settingsRangeBoxes) {
      settingsRangeBoxes.classList.toggle('hidden', !isRange);
      if (isRange) {
        if (!draftSettings.rangeFrom) draftSettings.rangeFrom = 1;
        if (!draftSettings.rangeTo) draftSettings.rangeTo = doc.totalPages;
        if (inputRangeFrom) {
          inputRangeFrom.max = doc.totalPages;
          inputRangeFrom.value = draftSettings.rangeFrom;
        }
        if (inputRangeTo) {
          inputRangeTo.max = doc.totalPages;
          inputRangeTo.value = draftSettings.rangeTo;
        }
      }
    }

    if (settingsSelectionControls) {
      settingsSelectionControls.classList.toggle('hidden', !isSelection);
      if (isSelection) {
        if (!Array.isArray(draftSettings.selectedPages)) {
          draftSettings.selectedPages = Array.from({ length: doc.totalPages }, (_, i) => i + 1);
        }
        renderSelectionSeats(doc);
        syncSelectionState(doc);
      }
    }

    // 2. Copies
    if (copiesCountDisplay) copiesCountDisplay.textContent = draftSettings.copies;

    // 3. Print Mode
    cardModeBw.classList.toggle('active', draftSettings.mode === 'B&W');
    cardModeColor.classList.toggle('active', draftSettings.mode === 'Color');

    // 4. Print Sides
    cardSideSingle.classList.toggle('active', draftSettings.sides === 'Single-sided');
    cardSideDouble.classList.toggle('active', draftSettings.sides === 'Double-sided');

    // Dynamic rate labels on Single vs Double side based on Print Mode
    const isColorChoice = (draftSettings.mode === 'Color');
    if (settingsSinglePriceLabel) settingsSinglePriceLabel.textContent = isColorChoice ? '₹8.00/sheet' : '₹3.00/sheet';
    if (settingsDoublePriceLabel) settingsDoublePriceLabel.textContent = isColorChoice ? '₹10.00/sheet' : '₹5.00/sheet';

    // 5. Orientation
    btnOrientAuto.classList.toggle('active', draftSettings.orientation === 'Auto');
    btnOrientPortrait.classList.toggle('active', draftSettings.orientation === 'Portrait');
    btnOrientLandscape.classList.toggle('active', draftSettings.orientation === 'Landscape');

    // 6. Pages per sheet
    const curNup = String(draftSettings.pagesPerSheet || '1');
    if (cardNup1) cardNup1.classList.toggle('active', curNup === '1');
    if (cardNup2) cardNup2.classList.toggle('active', curNup === '2');
    if (cardNup4) cardNup4.classList.toggle('active', curNup === '4');
    if (cardNup6) cardNup6.classList.toggle('active', curNup === '6');

    // Live bottom price button
    const livePrice = calculateDocPrice(doc.totalPages, draftSettings);
    if (saveSettingsPrice) saveSettingsPrice.textContent = formatRupees(livePrice);

    if (previewSideToggle) {
      previewSideToggle.classList.toggle('hidden', draftSettings.sides !== 'Double-sided');
    }
    if (btnSheetFlip) {
      btnSheetFlip.classList.toggle('hidden', draftSettings.sides !== 'Double-sided');
    }
  }

  function setupSettingsEventListeners() {
    btnSettingsBack.addEventListener('click', () => switchView('cart'));

    // Dropdown menu
    btnSettingsMenu.addEventListener('click', (e) => {
      e.stopPropagation();
      settingsMenuDropdown.classList.toggle('hidden');
    });
    document.addEventListener('click', () => {
      if (settingsMenuDropdown) settingsMenuDropdown.classList.add('hidden');
    });

    menuOptReset.addEventListener('click', () => {
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (!doc) return;
      draftSettings = {
        pageSelection: 'All',
        rangeFrom: 1,
        rangeTo: doc.totalPages,
        selectedPages: Array.from({ length: doc.totalPages }, (_, idx) => idx + 1),
        customPageRange: '',
        copies: 1,
        mode: 'B&W',
        sides: doc.totalPages > 1 ? 'Double-sided' : 'Single-sided',
        orientation: 'Auto',
        pagesPerSheet: '1'
      };
      currentPreviewSheet = 1;
      currentPreviewSide = 'front';
      syncSettingsUI(doc);
      renderPreviewSheet();
    });

    menuOptDelete.addEventListener('click', () => {
      if (activeSettingsDocId) {
        removeDoc(activeSettingsDocId);
        switchView('cart');
      }
    });

    // 3-Way Page Selection buttons
    if (btnOptAll) {
      btnOptAll.addEventListener('click', () => {
        draftSettings.pageSelection = 'All';
        draftSettings.customPageRange = '';
        currentPreviewSheet = 1;
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (doc) {
          syncSettingsUI(doc);
          renderPreviewSheet();
        }
      });
    }

    if (btnOptRange) {
      btnOptRange.addEventListener('click', () => {
        draftSettings.pageSelection = 'Custom Range';
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (doc) {
          if (!draftSettings.rangeFrom) draftSettings.rangeFrom = 1;
          if (!draftSettings.rangeTo) draftSettings.rangeTo = doc.totalPages;
          currentPreviewSheet = 1;
          syncSettingsUI(doc);
          if (inputRangeFrom) inputRangeFrom.focus();
          renderPreviewSheet();
        }
      });
    }

    if (btnOptSelection) {
      btnOptSelection.addEventListener('click', () => {
        draftSettings.pageSelection = 'Selection';
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (doc) {
          if (!Array.isArray(draftSettings.selectedPages) || draftSettings.selectedPages.length === 0) {
            draftSettings.selectedPages = Array.from({ length: doc.totalPages }, (_, i) => i + 1);
          }
          currentPreviewSheet = 1;
          syncSettingsUI(doc);
          renderPreviewSheet();
        }
      });
    }

    // Direct Range Numeric Input Boxes (From & To)
    if (inputRangeFrom) {
      inputRangeFrom.addEventListener('input', () => {
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (!doc) return;
        let from = parseInt(inputRangeFrom.value, 10);
        if (isNaN(from)) return;
        if (from < 1) from = 1;
        if (from > doc.totalPages) from = doc.totalPages;

        let to = parseInt(inputRangeTo?.value, 10);
        if (isNaN(to) || to < from) {
          to = from;
          if (inputRangeTo) inputRangeTo.value = to;
        }
        draftSettings.rangeFrom = from;
        draftSettings.rangeTo = to;
        draftSettings.customPageRange = (from === to) ? String(from) : `${from}-${to}`;

        const livePrice = calculateDocPrice(doc.totalPages, draftSettings);
        if (saveSettingsPrice) saveSettingsPrice.textContent = formatRupees(livePrice);
        renderPreviewSheet();
      });

      inputRangeFrom.addEventListener('change', () => {
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (doc) {
          updateRangeBoxes(doc);
          currentPreviewSheet = 1;
          renderPreviewSheet();
        }
      });
    }

    if (inputRangeTo) {
      inputRangeTo.addEventListener('input', () => {
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (!doc) return;
        let to = parseInt(inputRangeTo.value, 10);
        if (isNaN(to)) return;
        if (to > doc.totalPages) to = doc.totalPages;

        let from = parseInt(inputRangeFrom?.value, 10);
        if (isNaN(from) || from > to) {
          from = Math.max(1, to);
          if (inputRangeFrom) inputRangeFrom.value = from;
        }
        draftSettings.rangeFrom = from;
        draftSettings.rangeTo = to;
        draftSettings.customPageRange = (from === to) ? String(from) : `${from}-${to}`;

        const livePrice = calculateDocPrice(doc.totalPages, draftSettings);
        if (saveSettingsPrice) saveSettingsPrice.textContent = formatRupees(livePrice);
        renderPreviewSheet();
      });

      inputRangeTo.addEventListener('change', () => {
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (doc) {
          updateRangeBoxes(doc);
          currentPreviewSheet = 1;
          renderPreviewSheet();
        }
      });
    }

    // Selection Controls (Check All / Uncheck All)
    if (btnSelectCheckAll) {
      btnSelectCheckAll.addEventListener('click', () => {
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (!doc || !draftSettings) return;
        draftSettings.selectedPages = Array.from({ length: doc.totalPages }, (_, i) => i + 1);
        currentPreviewSheet = 1;
        syncSelectionState(doc);
        renderPreviewSheet();
      });
    }

    if (btnSelectUncheckAll) {
      btnSelectUncheckAll.addEventListener('click', () => {
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (!doc || !draftSettings) return;
        draftSettings.selectedPages = [];
        currentPreviewSheet = 1;
        syncSelectionState(doc);
        renderPreviewSheet();
      });
    }

    // Copies Stepper
    btnCopiesMinus.addEventListener('click', () => {
      if (draftSettings.copies > 1) {
        draftSettings.copies--;
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (doc) {
          syncSettingsUI(doc);
          renderPreviewSheet();
        }
      }
    });

    btnCopiesPlus.addEventListener('click', () => {
      if (draftSettings.copies < 99) {
        draftSettings.copies++;
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (doc) {
          syncSettingsUI(doc);
          renderPreviewSheet();
        }
      }
    });

    // Print Mode Cards (Instant update)
    cardModeBw.addEventListener('click', () => {
      draftSettings.mode = 'B&W';
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (doc) {
        syncSettingsUI(doc);
        renderPreviewSheet();
      }
    });

    cardModeColor.addEventListener('click', () => {
      draftSettings.mode = 'Color';
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (doc) {
        syncSettingsUI(doc);
        renderPreviewSheet();
      }
    });

    // Print Sides Cards (Instant duplex update)
    cardSideSingle.addEventListener('click', () => {
      draftSettings.sides = 'Single-sided';
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (doc) {
        currentPreviewSide = 'front';
        currentPreviewSheet = 1;
        syncSettingsUI(doc);
        renderPreviewSheet();
      }
    });

    cardSideDouble.addEventListener('click', () => {
      draftSettings.sides = 'Double-sided';
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (doc) {
        syncSettingsUI(doc);
        renderPreviewSheet();
      }
    });

    // Orientation Controls
    btnOrientAuto.addEventListener('click', () => {
      draftSettings.orientation = 'Auto';
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (doc) {
        syncSettingsUI(doc);
        renderPreviewSheet();
      }
    });

    btnOrientPortrait.addEventListener('click', () => {
      draftSettings.orientation = 'Portrait';
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (doc) {
        syncSettingsUI(doc);
        renderPreviewSheet();
      }
    });

    btnOrientLandscape.addEventListener('click', () => {
      draftSettings.orientation = 'Landscape';
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (doc) {
        syncSettingsUI(doc);
        renderPreviewSheet();
      }
    });

    // Pages Per Sheet Controls (1, 2, 4, 6)
    const handleNupChange = (nupVal) => {
      draftSettings.pagesPerSheet = nupVal;
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (doc) {
        currentPreviewSheet = 1;
        syncSettingsUI(doc);
        renderPreviewSheet();
      }
    };

    if (cardNup1) cardNup1.addEventListener('click', () => handleNupChange('1'));
    if (cardNup2) cardNup2.addEventListener('click', () => handleNupChange('2'));
    if (cardNup4) cardNup4.addEventListener('click', () => handleNupChange('4'));
    if (cardNup6) cardNup6.addEventListener('click', () => handleNupChange('6'));

    // Retry Preview Button
    if (btnRetryPreview) {
      btnRetryPreview.addEventListener('click', () => {
        const doc = cartItems.find(d => d.id === (activeSettingsDocId || activePreviewDocId));
        if (doc) {
          renderPreviewSheet();
        }
      });
    }

    // Save Settings Button
    btnSaveDocSettings.addEventListener('click', () => {
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (!doc) return;

      if (draftSettings.pageSelection === 'Selection') {
        const selCount = Array.isArray(draftSettings.selectedPages) ? draftSettings.selectedPages.length : 0;
        if (selCount === 0) {
          if (selectionErrorText) selectionErrorText.classList.remove('hidden');
          return;
        }
      }

      if (draftSettings.pageSelection === 'Custom Range') {
        updateRangeBoxes(doc);
      } else if (draftSettings.customPageRange) {
        const parsed = parseCustomPages(draftSettings.customPageRange, doc.totalPages);
        if (!parsed.valid) {
          if (customRangeError) {
            customRangeError.textContent = parsed.error;
            customRangeError.classList.remove('hidden');
          }
          return;
        }
      }

      doc.settings = JSON.parse(JSON.stringify(draftSettings));
      doc.selectedPages = getDocSelectedPages(doc.totalPages, doc.settings);

      const isDuplex = (doc.settings.sides === 'Double-sided');
      const nup = parseInt(doc.settings.pagesPerSheet, 10) || 1;
      const count = doc.selectedPages.length;
      doc.totalSheets = Math.max(1, isDuplex ? Math.ceil(count / (nup * 2)) : Math.ceil(count / nup));
      doc.pricePaise = calculateDocPrice(doc.totalPages, doc.settings);
      doc.authoritativePricePaise = doc.pricePaise;

      switchView('cart');
    });
  }

  // --- View 3: Print Preview Engine (Unified inside View 2) ---
  function openPreview(docId) {
    // Kept for backward compatibility: redirects directly to unified settings
    openSettings(docId);
  }

  async function renderPreviewSheet() {
    const doc = cartItems.find(d => d.id === (activeSettingsDocId || activePreviewDocId));
    if (!doc || !doc.pdfDoc) return;

    const isDuplex = draftSettings ? (draftSettings.sides === 'Double-sided') : (doc.settings.sides === 'Double-sided');
    const nup = draftSettings ? (parseInt(draftSettings.pagesPerSheet, 10) || 1) : 1;

    // Resolve pages browsable in preview
    const pages = getPreviewPageList(doc, draftSettings);

    if (pages.length === 0) {
      if (btnSheetLeft) btnSheetLeft.disabled = true;
      if (btnSheetRight) btnSheetRight.disabled = true;
      if (previewSideToggle) previewSideToggle.classList.add('hidden');
      if (previewSheetIndicator) previewSheetIndicator.textContent = '0 of 0 sheets';

      if (selectionSeatsGrid) {
        const seatBtns = selectionSeatsGrid.querySelectorAll('.seat-btn');
        seatBtns.forEach(btn => btn.classList.remove('current-view'));
      }

      const canvas = previewCanvas;
      if (canvas) {
        canvas.width = 160;
        canvas.height = 226;
        canvas.style.width = '160px';
        canvas.style.height = '226px';
        const ctx = canvas.getContext('2d');
        ctx.fillStyle = '#ffffff';
        ctx.fillRect(0, 0, canvas.width, canvas.height);
        ctx.strokeStyle = '#e2e8f0';
        ctx.strokeRect(1, 1, canvas.width - 2, canvas.height - 2);

        ctx.fillStyle = '#94a3b8';
        ctx.font = '600 13px system-ui, sans-serif';
        ctx.textAlign = 'center';
        ctx.fillText('No Pages Selected', canvas.width / 2, canvas.height / 2 - 8);
        ctx.font = '11px system-ui, sans-serif';
        ctx.fillText('Select pages below', canvas.width / 2, canvas.height / 2 + 10);
      }
      if (previewLoadingSpinner) previewLoadingSpinner.classList.add('hidden');
      return;
    }

    const totalSheets = Math.max(1, isDuplex ? Math.ceil(pages.length / (nup * 2)) : Math.ceil(pages.length / nup));

    // Bounds check
    if (currentPreviewSheet < 1) currentPreviewSheet = 1;
    if (currentPreviewSheet > totalSheets) currentPreviewSheet = totalSheets;

    // Pager indicators
    if (btnSheetLeft) btnSheetLeft.disabled = (currentPreviewSheet <= 1);
    if (btnSheetRight) btnSheetRight.disabled = (currentPreviewSheet >= totalSheets);

    if (previewSideToggle) {
      previewSideToggle.classList.toggle('hidden', !isDuplex);
    }
    if (toggleSideFront && toggleSideBack) {
      toggleSideFront.classList.toggle('active', currentPreviewSide === 'front');
      toggleSideFront.setAttribute('aria-pressed', currentPreviewSide === 'front');
      toggleSideBack.classList.toggle('active', currentPreviewSide === 'back');
      toggleSideBack.setAttribute('aria-pressed', currentPreviewSide === 'back');
    }
    if (btnSheetFlip) {
      btnSheetFlip.classList.toggle('hidden', !isDuplex);
    }

    if (previewSheetIndicator) {
      previewSheetIndicator.textContent = `Sheet ${currentPreviewSheet} of ${totalSheets}`;
    }

    // Determine 1-based PDF page index
    let pageNumToRender = null;
    if (!isDuplex) {
      pageNumToRender = pages[(currentPreviewSheet - 1) * nup] || null;
    } else {
      if (currentPreviewSide === 'front') {
        pageNumToRender = pages[(currentPreviewSheet - 1) * (nup * 2)] || null;
      } else {
        const backIdx = (currentPreviewSheet - 1) * (nup * 2) + nup;
        pageNumToRender = (backIdx < pages.length) ? pages[backIdx] : null;
      }
    }

    // Synchronize active seat focus ring with currently displayed page
    if (selectionSeatsGrid && pageNumToRender !== null) {
      const seatBtns = selectionSeatsGrid.querySelectorAll('.seat-btn');
      seatBtns.forEach(btn => {
        const p = parseInt(btn.dataset.page, 10);
        btn.classList.toggle('current-view', p === pageNumToRender);
      });
    }

    if (previewCanvas) {
      previewCanvas.classList.remove('page-excluded');
    }

    if (previewLoadingSpinner) previewLoadingSpinner.classList.add('hidden');
    if (previewErrorBox) previewErrorBox.classList.add('hidden');

    try {
      if (activeRenderTask) {
        try { activeRenderTask.cancel(); } catch (_) { }
        activeRenderTask = null;
      }

      const canvas = previewCanvas;
      if (!canvas) return;
      const ctx = canvas.getContext('2d');

      if (pageNumToRender === null) {
        // Blank back sheet for odd duplex
        canvas.width = 160;
        canvas.height = 226;
        canvas.style.width = '160px';
        canvas.style.height = '226px';
        ctx.fillStyle = '#ffffff';
        ctx.fillRect(0, 0, canvas.width, canvas.height);
        ctx.strokeStyle = '#e2e8f0';
        ctx.strokeRect(1, 1, canvas.width - 2, canvas.height - 2);

        ctx.fillStyle = '#94a3b8';
        ctx.font = '600 13px system-ui, sans-serif';
        ctx.textAlign = 'center';
        ctx.fillText('Blank Page', canvas.width / 2, canvas.height / 2 - 8);
        ctx.font = '11px system-ui, sans-serif';
        ctx.fillText('(Duplex back sheet)', canvas.width / 2, canvas.height / 2 + 10);
        if (previewLoadingSpinner) previewLoadingSpinner.classList.add('hidden');
        return;
      }

      const page = await doc.pdfDoc.getPage(pageNumToRender);

      // Determine orientation rotation
      let renderRotation = page.rotate || 0;
      if (draftSettings) {
        if (draftSettings.orientation === 'Landscape' && page.view[2] < page.view[3]) {
          renderRotation = (renderRotation + 90) % 360;
        } else if (draftSettings.orientation === 'Portrait' && page.view[2] > page.view[3]) {
          renderRotation = (renderRotation + 90) % 360;
        }
      }

      const rawViewport = page.getViewport({ scale: 1.0, rotation: renderRotation });

      // Fit preview to the available stage dimensions while keeping aspect ratio
      const stageEl = canvas.parentElement;
      const stageStyle = stageEl ? window.getComputedStyle(stageEl) : null;
      const padX = stageStyle
        ? (parseFloat(stageStyle.paddingLeft) || 0) + (parseFloat(stageStyle.paddingRight) || 0)
        : 16;
      const availableW = (stageEl && stageEl.clientWidth > 0)
        ? (stageEl.clientWidth - padX)
        : Math.min(window.innerWidth - 64, 536);
      const maxStageW = Math.max(220, availableW);
      const maxStageH = (window.innerWidth >= 600)
        ? Math.min(420, Math.max(320, Math.round(window.innerHeight * 0.48)))
        : Math.min(360, Math.max(280, Math.round(window.innerHeight * 0.45)));
      const scale = Math.min(maxStageW / rawViewport.width, maxStageH / rawViewport.height, 3.0);
      const scaledViewport = page.getViewport({ scale: scale, rotation: renderRotation });

      // Crisp rendering support
      const dpr = window.devicePixelRatio || 1;
      canvas.width = Math.round(scaledViewport.width * dpr);
      canvas.height = Math.round(scaledViewport.height * dpr);
      canvas.style.width = `${Math.round(scaledViewport.width)}px`;
      canvas.style.height = 'auto';

      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);

      const renderContext = {
        canvasContext: ctx,
        viewport: scaledViewport
      };

      activeRenderTask = page.render(renderContext);
      await activeRenderTask.promise;
      activeRenderTask = null;

      // Apply grayscale filter if B&W
      const currentMode = draftSettings ? draftSettings.mode : doc.settings.mode;
      if (currentMode === 'B&W') {
        canvas.style.filter = 'grayscale(100%) contrast(105%)';
      } else {
        canvas.style.filter = 'none';
      }

      // Update canvas title tooltip
      if (isDuplex) {
        canvas.title = `Double-sided (${currentPreviewSide === 'front' ? 'Front' : 'Back'}) - Tap sheet to flip`;
      } else {
        canvas.title = 'Single-sided sheet';
      }
    } catch (renderErr) {
      if (renderErr.name !== 'RenderingCancelledException') {
        console.error('Preview render error:', renderErr);
        if (previewErrorBox) previewErrorBox.classList.remove('hidden');
      }
    } finally {
      if (previewLoadingSpinner) previewLoadingSpinner.classList.add('hidden');
    }
  }

  function setupPreviewEventListeners() {
    if (btnPreviewBack) btnPreviewBack.addEventListener('click', () => switchView('cart'));

    if (btnSheetLeft) {
      btnSheetLeft.addEventListener('click', () => {
        if (currentPreviewSheet > 1) {
          currentPreviewSheet--;
          renderPreviewSheet();
        }
      });
    }

    if (btnSheetRight) {
      btnSheetRight.addEventListener('click', () => {
        const doc = cartItems.find(d => d.id === (activeSettingsDocId || activePreviewDocId));
        if (!doc) return;
        const totalSheets = getPreviewTotalSheets(doc, draftSettings);
        if (currentPreviewSheet < totalSheets) {
          currentPreviewSheet++;
          renderPreviewSheet();
        }
      });
    }

    const setPreviewSide = (targetSide, triggerAnim = true) => {
      const doc = cartItems.find(d => d.id === (activeSettingsDocId || activePreviewDocId));
      if (!doc) return;
      const isDuplex = (draftSettings ? draftSettings.sides : doc.settings.sides) === 'Double-sided';
      if (!isDuplex) return;
      if (currentPreviewSide === targetSide) return;
      currentPreviewSide = targetSide;
      if (triggerAnim && previewCanvas) {
        previewCanvas.classList.remove('flipping');
        void previewCanvas.offsetWidth; // trigger reflow
        previewCanvas.classList.add('flipping');
        setTimeout(() => previewCanvas.classList.remove('flipping'), 300);
      }
      renderPreviewSheet();
    };

    if (toggleSideFront) {
      toggleSideFront.addEventListener('click', () => setPreviewSide('front', false));
    }
    if (toggleSideBack) {
      toggleSideBack.addEventListener('click', () => setPreviewSide('back', false));
    }

    const triggerFlip = () => {
      const doc = cartItems.find(d => d.id === (activeSettingsDocId || activePreviewDocId));
      if (!doc) return;
      const isDuplex = (draftSettings ? draftSettings.sides : doc.settings.sides) === 'Double-sided';
      if (isDuplex) {
        setPreviewSide(currentPreviewSide === 'front' ? 'back' : 'front', true);
      } else {
        // Simplex - advance to next sheet if available
        if (btnSheetRight && !btnSheetRight.disabled) {
          btnSheetRight.click();
        }
      }
    };

    if (btnSheetFlip) {
      btnSheetFlip.addEventListener('click', triggerFlip);
    }

    // Direct flip gesture when tapping or clicking on the canvas
    if (previewCanvas) {
      previewCanvas.addEventListener('click', triggerFlip);
    }

    // Re-fit preview when viewport resizes
    let previewResizeTimer = null;
    window.addEventListener('resize', () => {
      clearTimeout(previewResizeTimer);
      previewResizeTimer = setTimeout(() => {
        if (viewSettings && !viewSettings.classList.contains('hidden')) {
          renderPreviewSheet();
        }
      }, 150);
    });

    if (btnPreviewMenu) {
      btnPreviewMenu.addEventListener('click', (e) => {
        e.stopPropagation();
        if (previewMenuDropdown) previewMenuDropdown.classList.toggle('hidden');
      });
    }

    if (previewMenuSettings) {
      previewMenuSettings.addEventListener('click', () => {
        if (activePreviewDocId) {
          switchView('settings', activePreviewDocId);
        }
      });
    }
  }

  // --- View 4: Checkout & Order Submission Implementation ---
  async function submitOrder() {
    if (btnReviewOrder && btnReviewOrder.disabled) return;
    const readyItems = cartItems.filter(it => it.status === 'ready');
    const processingItems = cartItems.filter(it => it.status === 'processing');
    if (processingItems.length > 0) {
      showCartError('Please wait for files to finish loading.');
      return;
    }
    if (readyItems.length === 0) {
      showCartError('Please add at least one document to proceed.');
      return;
    }

    btnReviewOrder.disabled = true;
    const originalText = btnReviewOrder.innerHTML;
    btnReviewOrder.innerHTML = `<span>Generating PIN...</span>`;

    try {
      const formData = new FormData();
      const metadata = [];

      for (let i = 0; i < readyItems.length; i++) {
        const item = readyItems[i];
        let filePayload = item.file;
        if (item.pdfBytes && (item.isImage || item.isOffice)) {
          const pdfBlob = new Blob([item.pdfBytes], { type: 'application/pdf' });
          const safeName = item.name.replace(/\.[^.]+$/, '') + '.pdf';
          filePayload = new File([pdfBlob], safeName, { type: 'application/pdf' });
        }
        formData.append('files', filePayload);

        const isDoubleSided = item.settings.sides === 'Double-sided';
        const duplexModeStr = isDoubleSided ? 'DUPLEX_LONG_EDGE' : 'SIMPLEX';

        let pageRangeVal = '';
        if (item.settings.pageSelection === 'Custom Range') {
          const from = item.settings.rangeFrom || 1;
          const to = item.settings.rangeTo || item.totalPages;
          pageRangeVal = (from === to) ? String(from) : `${from}-${to}`;
        } else if (item.settings.pageSelection === 'Selection') {
          const sel = Array.isArray(item.settings.selectedPages) ? item.settings.selectedPages : [];
          pageRangeVal = sel.slice().sort((a, b) => a - b).join(',');
        } else if (item.settings.customPageRange) {
          pageRangeVal = item.settings.customPageRange;
        }

        metadata.push({
          copies: item.settings.copies || 1,
          duplex: duplexModeStr,
          color_mode: item.settings.mode === 'Color' ? 'COLOR' : 'BW',
          page_range: pageRangeVal,
          orientation: (item.settings.orientation || 'Auto').toUpperCase(),
          pages_per_sheet: parseInt(item.settings.pagesPerSheet, 10) || 1
        });
      }

      formData.append('metadata', JSON.stringify(metadata));

      if (readyItems.length > 0) {
        const first = readyItems[0];
        const isDouble = first.settings.sides === 'Double-sided';
        formData.append('copies', String(first.settings.copies || 1));
        formData.append('duplex', isDouble ? 'DUPLEX_LONG_EDGE' : 'SIMPLEX');
        formData.append('color_mode', first.settings.mode === 'Color' ? 'COLOR' : 'BW');
        formData.append('orientation', (first.settings.orientation || 'Auto').toUpperCase());
        formData.append('pages_per_sheet', String(parseInt(first.settings.pagesPerSheet, 10) || 1));
        let firstPageRange = '';
        if (first.settings.pageSelection === 'Custom Range') {
          const from = first.settings.rangeFrom || 1;
          const to = first.settings.rangeTo || first.totalPages;
          firstPageRange = (from === to) ? String(from) : `${from}-${to}`;
        } else if (first.settings.pageSelection === 'Selection') {
          const sel = Array.isArray(first.settings.selectedPages) ? first.settings.selectedPages : [];
          firstPageRange = sel.slice().sort((a, b) => a - b).join(',');
        } else if (first.settings.customPageRange) {
          firstPageRange = first.settings.customPageRange;
        }
        if (firstPageRange) {
          formData.append('page_range', firstPageRange);
        }
      }

      const uploadUrl = `/api/k/${encodeURIComponent(kioskID)}/upload`;
      const resp = await fetch(uploadUrl, { method: 'POST', body: formData });

      if (!resp.ok) {
        const errText = await resp.text();
        throw new Error(errText || 'Failed to submit order to kiosk');
      }

      const data = await resp.json();
      activeOrderId = data.order_id;
      activeRecoveryToken = (data.recovery_token || data.claim_token || '').trim();
      if (activeOrderId && activeRecoveryToken) {
        try {
          sessionStorage.setItem('xeroxgo_recovery_' + activeOrderId, activeRecoveryToken);
          localStorage.setItem('xeroxgo_recovery_' + activeOrderId, activeRecoveryToken);
        } catch (_) { }
      }
      const pinCode = data.display_code ? String(data.display_code).trim() : '';
      if (!pinCode || !/^\d{4}$/.test(pinCode)) {
        throw new Error('Order was created but server returned an invalid or missing 4-digit PIN. Please try again.');
      }
      activeDisplayCode = pinCode;

      // Populate Checkout View
      for (let i = 0; i < 4; i++) {
        if (pinBoxes[i]) {
          pinBoxes[i].textContent = activeDisplayCode[i] || '-';
        }
      }

      if (checkoutDocsCount) {
        checkoutDocsCount.textContent = `${readyItems.length} file${readyItems.length > 1 ? 's' : ''}`;
      }
      if (checkoutTotalAmount) {
        const totalPaise = data.total_price_paise || readyItems.reduce((acc, it) => acc + it.pricePaise, 0);
        checkoutTotalAmount.textContent = formatRupees(totalPaise);
      }

      startExpiryCountdown(data.pin_expires_at);
      startOrderStatusPolling(activeOrderId);

      switchView('checkout');
    } catch (orderErr) {
      console.error('Order submission error:', orderErr);
      showCartError(`Could not create order: ${orderErr.message || orderErr}`);
    } finally {
      renderCart();
    }
  }

  let authoritativeExpiryMs = 0;

  function startExpiryCountdown(expiryISO) {
    if (expiryCountdownTimer) clearInterval(expiryCountdownTimer);
    if (expiryISO) {
      const parsed = Date.parse(expiryISO);
      if (!isNaN(parsed)) {
        authoritativeExpiryMs = parsed;
      }
    }
    if (!authoritativeExpiryMs) {
      authoritativeExpiryMs = Date.now() + 15 * 60 * 1000;
    }

    function tick() {
      const now = Date.now();
      const diffMs = authoritativeExpiryMs - now;
      const secondsLeft = Math.max(0, Math.ceil(diffMs / 1000));
      const minutes = Math.floor(secondsLeft / 60);
      const seconds = secondsLeft % 60;
      if (pinExpiryText) {
        if (secondsLeft <= 0) {
          pinExpiryText.textContent = 'PIN expired. Please create a new order.';
          if (expiryCountdownTimer) clearInterval(expiryCountdownTimer);
        } else {
          pinExpiryText.textContent = `Expires in ${minutes}:${seconds < 10 ? '0' : ''}${seconds}`;
        }
      }
    }

    tick();
    expiryCountdownTimer = setInterval(tick, 1000);
  }

  function startOrderStatusPolling(orderId) {
    if (orderPollTimer) clearInterval(orderPollTimer);

    orderPollTimer = setInterval(async () => {
      try {
        const resp = await fetch(`/api/orders/${encodeURIComponent(orderId)}/status`);
        if (!resp.ok) return;
        const statusData = await resp.json();
        updateCheckoutStatus(statusData);
      } catch (_) { }
    }, 2500);
  }

  function updateCheckoutStatus(statusData) {
    if (!statusData || !checkoutLiveStatusBox) return;
    const st = statusData.status || '';

    if (statusData.pin_expires_at) {
      const parsed = Date.parse(statusData.pin_expires_at);
      if (!isNaN(parsed)) {
        authoritativeExpiryMs = parsed;
      }
    }

    if (st === 'EXPIRED') {
      if (expiryCountdownTimer) clearInterval(expiryCountdownTimer);
      if (pinExpiryText) {
        pinExpiryText.textContent = 'PIN expired. Please create a new order.';
      }
    }

    if (st && st !== 'PENDING_PAYMENT' && st !== 'EXPIRED') {
      checkoutLiveStatusBox.classList.remove('hidden');
    }

    if (checkoutStatusPill) {
      checkoutStatusPill.className = `status-pill status-${st}`;
      checkoutStatusPill.textContent = st;
    }

    if (checkoutProgressBar) {
      let pct = 0;
      if (st === 'PAID_WAITING') pct = 25;
      else if (st === 'SPOOLING') pct = 50;
      else if (st === 'PRINTING') {
        const pagesPrinted = statusData.pages_printed || 0;
        const totalPages = statusData.total_pages || 1;
        pct = Math.min(95, Math.max(60, Math.round((pagesPrinted / totalPages) * 100)));
      } else if (st === 'COMPLETED') {
        pct = 100;
        if (orderPollTimer) clearInterval(orderPollTimer);
      }
      checkoutProgressBar.style.width = pct + '%';
    }

    if (checkoutStatusMsg) {
      if (st === 'PRINTING') {
        checkoutStatusMsg.textContent = `Printing page ${statusData.pages_printed || 1} of ${statusData.total_pages || 1}...`;
      } else if (st === 'COMPLETED') {
        checkoutStatusMsg.textContent = 'Your documents have finished printing!';
      } else {
        checkoutStatusMsg.textContent = '';
      }
    }
  }

  async function requestRecoveryPIN() {
    if (!activeOrderId) return null;
    let token = activeRecoveryToken;
    if (!token) {
      try {
        token = sessionStorage.getItem('xeroxgo_recovery_' + activeOrderId) ||
          localStorage.getItem('xeroxgo_recovery_' + activeOrderId) || '';
      } catch (_) { }
    }
    if (!token) return null;
    try {
      const resp = await fetch(`/api/orders/${encodeURIComponent(activeOrderId)}/recovery-pin`, {
        method: 'POST',
        headers: {
          'X-Recovery-Token': token
        }
      });
      if (!resp.ok) return null;
      const recData = await resp.json();
      if (recData && recData.display_code) {
        activeDisplayCode = String(recData.display_code).trim();
        for (let i = 0; i < 4; i++) {
          if (pinBoxes[i]) {
            pinBoxes[i].textContent = activeDisplayCode[i] || '-';
          }
        }
        if (recData.pin_expires_at) {
          startExpiryCountdown(recData.pin_expires_at);
        }
        return recData;
      }
    } catch (_) { }
    return null;
  }

  function setupCheckoutEventListeners() {
    btnCopyPin.addEventListener('click', async () => {
      if (!activeDisplayCode) return;
      try {
        await navigator.clipboard.writeText(activeDisplayCode);
        if (btnCopyPinLabel) {
          const original = btnCopyPinLabel.textContent;
          btnCopyPinLabel.textContent = 'Copied!';
          setTimeout(() => {
            btnCopyPinLabel.textContent = original;
          }, 2000);
        }
      } catch (_) {
        alert(`PIN: ${activeDisplayCode}`);
      }
    });

    btnStartNewOrder.addEventListener('click', () => {
      if (orderPollTimer) clearInterval(orderPollTimer);
      if (expiryCountdownTimer) clearInterval(expiryCountdownTimer);
      cartItems.forEach(it => {
        if (it.abortController) {
          try { it.abortController.abort(); } catch (_) { }
        }
      });
      cartItems = [];
      activeOrderId = null;
      activeDisplayCode = '';
      activeRecoveryToken = '';
      switchView('cart');
      renderCart();
    });
  }

  // --- Banner & PWA Setup ---
  function setupBanner() {
    if (!bannerTrack || !bannerDots) return;
    let currentSlide = 0;
    const slides = bannerTrack.children;
    const dots = bannerDots.children;
    if (slides.length === 0) return;

    function setSlide(idx) {
      currentSlide = idx % slides.length;
      bannerTrack.style.transform = `translateX(-${currentSlide * 100}%)`;
      Array.from(dots).forEach((dot, i) => {
        dot.classList.toggle('active', i === currentSlide);
      });
    }

    Array.from(dots).forEach((dot, idx) => {
      dot.addEventListener('click', () => setSlide(idx));
    });

    setInterval(() => {
      setSlide(currentSlide + 1);
    }, 5000);

    if (bannerCloseBtn && promoBanner) {
      bannerCloseBtn.addEventListener('click', () => {
        promoBanner.style.display = 'none';
      });
    }
  }

  function setupPwa() {
    window.addEventListener('beforeinstallprompt', (e) => {
      e.preventDefault();
      deferredPwaPrompt = e;
      if (pwaInstallBanner) pwaInstallBanner.classList.remove('hidden');
    });

    if (pwaDismissBtn && pwaInstallBanner) {
      pwaDismissBtn.addEventListener('click', () => {
        pwaInstallBanner.classList.add('hidden');
        if (pwaMiniPill) pwaMiniPill.classList.remove('hidden');
      });
    }

    if (pwaInstallBtn) {
      pwaInstallBtn.addEventListener('click', async () => {
        if (deferredPwaPrompt) {
          deferredPwaPrompt.prompt();
          deferredPwaPrompt = null;
        } else if (pwaGuideModal) {
          pwaGuideModal.classList.remove('hidden');
        }
      });
    }

    if (pwaModalCloseBtn && pwaGuideModal) {
      pwaModalCloseBtn.addEventListener('click', () => pwaGuideModal.classList.add('hidden'));
    }
    if (pwaModalGotItBtn && pwaGuideModal) {
      pwaModalGotItBtn.addEventListener('click', () => pwaGuideModal.classList.add('hidden'));
    }
  }

  // --- Drag & Drop / File Input Setup ---
  function setupUploadInputs() {
    if (fileInput) {
      fileInput.addEventListener('change', (e) => {
        if (e.target.files && e.target.files.length > 0) {
          handleFiles(Array.from(e.target.files));
          fileInput.value = '';
        }
      });
    }

    if (cartUploadCard) {
      // Prevent opening file picker if clicking on an alert inside the card
      cartUploadCard.addEventListener('click', (e) => {
        if (e.target.closest('#cart-alert')) {
          e.preventDefault();
        }
      });

      // Keyboard accessibility (Enter or Space triggers file picker)
      cartUploadCard.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          if (fileInput) fileInput.click();
        }
      });

      cartUploadCard.addEventListener('dragover', (e) => {
        e.preventDefault();
        cartUploadCard.classList.add('dragover');
      });
      cartUploadCard.addEventListener('dragleave', () => {
        cartUploadCard.classList.remove('dragover');
      });
      cartUploadCard.addEventListener('drop', (e) => {
        e.preventDefault();
        cartUploadCard.classList.remove('dragover');
        if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length > 0) {
          handleFiles(Array.from(e.dataTransfer.files));
        }
      });
    }

    if (btnReviewOrder) {
      btnReviewOrder.addEventListener('click', submitOrder);
    }
  }

  // --- Initialization ---
  function init() {
    if (devMode || window.__PORTAL_TEST__ || window.location.hostname === 'localhost' || window.location.hostname === '127.0.0.1') {
      window.__portal = {
        getCartItems: () => cartItems,
        setCartItems: (items) => {
          cartItems.length = 0;
          items.forEach(it => cartItems.push(it));
        },
        renderCart,
        submitOrder,
        convertDocumentOnServer,
        renderPreviewSheet,
        calculateDocPrice,
        switchView,
        openSettings
      };
    }
    setupBanner();
    setupPwa();
    setupUploadInputs();
    setupSettingsEventListeners();
    setupPreviewEventListeners();
    setupCheckoutEventListeners();
    renderCart();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
