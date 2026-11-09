/**
 * XeroxGo Mobile Upload & Print Portal
 * Responsive Mobile UX matching reference design:
 * 1. Files Cart (Multi-file upload, document cards, delete, preview, edit settings, review order)
 * 2. Document Settings (Page selection, copies stepper, B&W/Color, single/double sided, orientation, 1-up/2-up)
 * 3. Print Preview (PDF.js canvas rendering, Sheet X of Y (Front/Back) pager, Flip Sheet action)
 * 4. Checkout/PIN (4 large PIN boxes, 15m expiration, Copy PIN, order summary, live kiosk status)
 */

(function () {
  'use strict';

  // --- Configuration from DOM ---
  const container = document.getElementById('app-container') || document.querySelector('.app-container');
  if (!container) return;

  let kioskID = container.dataset.kioskId || 'kiosk-demo';
  const pathMatch = window.location.pathname.match(/\/k\/([^/]+)/);
  const searchParams = new URLSearchParams(window.location.search);
  const urlKiosk = (pathMatch && pathMatch[1]) || searchParams.get('kiosk_id');
  if (urlKiosk) {
    kioskID = urlKiosk;
  }
  const devMode = container.dataset.devMode === 'true';

  // Rates in paise (Authoritative defaults)
  let bwSimplexPricePaise = parseInt(container.dataset.pricePerSheetPaise, 10) || 200;
  let bwDuplexPricePaise = 150;
  let colorSimplexPricePaise = 1000;
  let colorDuplexPricePaise = 750;
  let colorHardwareEnabled = false;

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
  const settingsBwPriceLabel = document.getElementById('settings-bw-price-label');
  const settingsColorPriceLabel = document.getElementById('settings-color-price-label');

  const cardSideSingle = document.getElementById('card-side-single');
  const cardSideDouble = document.getElementById('card-side-double');

  const btnOrientAuto = document.getElementById('btn-orient-auto');
  const btnOrientPortrait = document.getElementById('btn-orient-portrait');
  const btnOrientLandscape = document.getElementById('btn-orient-landscape');

  const cardNup1 = document.getElementById('card-nup-1');
  const cardNup2 = document.getElementById('card-nup-2');

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
  let orderPollTimer = null;
  let expiryCountdownTimer = null;

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

  // Authoritative per-document price calculation in integer paise
  function calculateDocPrice(totalPages, settings) {
    if (!settings) return 0;
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

    let ratePaise = bwSimplexPricePaise;
    if (settings.mode === 'Color') {
      ratePaise = (settings.sides === 'Double-sided') ? colorDuplexPricePaise : colorSimplexPricePaise;
    } else {
      ratePaise = (settings.sides === 'Double-sided') ? bwDuplexPricePaise : bwSimplexPricePaise;
    }

    return pagesCount * (settings.copies || 1) * ratePaise;
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
            <button type="button" class="btn-doc-delete" data-id="${item.id}">Cancel</button>
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
            <button type="button" class="btn-doc-delete" data-id="${item.id}">Remove</button>
            <button type="button" class="btn-doc-retry" data-id="${item.id}">Retry</button>
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
        const itemPrice = calculateDocPrice(item.totalPages, item.settings);
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
          pageCountText = `${count} of ${item.totalPages} pages`;
        }

        const sidesText = item.settings.sides === 'Double-sided' ? 'Double Side' : 'Single Side';
        const metaText = `${pageCountText} · ${item.settings.mode} · ${sidesText}`;

        card.innerHTML = `
          <div class="doc-card-top">
            <div class="doc-icon-box">${iconSvg}</div>
            <div class="doc-info">
              <h4 class="doc-name" title="${escapeHtml(item.name)}">${escapeHtml(item.name)}</h4>
              <div class="doc-meta">${metaText}</div>
            </div>
            <div class="doc-price">${formatRupees(itemPrice)}</div>
          </div>
          <div class="doc-card-bottom">
            <button type="button" class="btn-doc-delete" data-id="${item.id}">Delete</button>
            <div class="doc-actions-right">
              <button type="button" class="btn-doc-edit" data-id="${item.id}">
                <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round">
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
      let countText = `${cartItems.length} document${cartItems.length > 1 ? 's' : ''}`;
      if (processingCount > 0) {
        countText += ` (${processingCount} preparing...)`;
      }
      cartDocsCount.textContent = countText;
    }
    if (cartTotalAmount) {
      cartTotalAmount.textContent = formatRupees(totalCartPaise);
    }

    if (btnReviewOrder) {
      if (processingCount > 0) {
        btnReviewOrder.disabled = true;
        btnReviewOrder.innerHTML = `<span>Preparing documents (${readyCount}/${cartItems.length})...</span>`;
      } else if (readyCount === 0) {
        btnReviewOrder.disabled = true;
        btnReviewOrder.innerHTML = `
          <span>Review Order</span>
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="5" y1="12" x2="19" y2="12"/><polyline points="12 5 19 12 12 19"/></svg>
        `;
      } else {
        btnReviewOrder.disabled = false;
        btnReviewOrder.innerHTML = `
          <span>Review Order</span>
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="5" y1="12" x2="19" y2="12"/><polyline points="12 5 19 12 12 19"/></svg>
        `;
      }
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
      } catch (_) {}
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

    let resp;
    try {
      if (onStatusUpdate) onStatusUpdate('Uploading for conversion...');
      resp = await fetch('/api/conversions', { method: 'POST', body: formData, signal });
    } catch (err) {
      if (signal && signal.aborted) throw err;
      if (onStatusUpdate) onStatusUpdate('Converting document on server...');
      resp = await fetch('/api/convert', { method: 'POST', body: formData, signal });
      if (!resp.ok) throw new Error('Document conversion failed');
      return await resp.arrayBuffer();
    }

    if (resp.status === 404) {
      if (onStatusUpdate) onStatusUpdate('Converting document on server...');
      const fallbackResp = await fetch('/api/convert', { method: 'POST', body: formData, signal });
      if (!fallbackResp.ok) throw new Error('Document conversion failed');
      return await fallbackResp.arrayBuffer();
    }

    if (!resp.ok) {
      const err = await resp.text();
      throw new Error(err || 'Document conversion request failed');
    }

    const createData = await resp.json();
    const jobId = createData.job_id;
    const token = createData.token;
    const pollUrl = createData.poll_url || `/api/conversions/${encodeURIComponent(jobId)}?token=${encodeURIComponent(token)}`;
    let status = createData.status;
    let downloadUrl = null;

    if (onStatusUpdate) onStatusUpdate('Waiting in conversion queue...');

    const startTime = Date.now();
    while (status === 'queued' || status === 'processing') {
      if (signal && signal.aborted) throw new Error('Cancelled');
      if (Date.now() - startTime > 90000) {
        throw new Error('Document conversion timed out');
      }
      if (onStatusUpdate) {
        onStatusUpdate(status === 'processing' ? 'Converting pages on server...' : 'Waiting in conversion queue...');
      }
      await new Promise(r => setTimeout(r, 1200));
      if (signal && signal.aborted) throw new Error('Cancelled');
      const pollResp = await fetch(pollUrl, { signal });
      if (!pollResp.ok) throw new Error('Failed to query conversion progress');
      const pollData = await pollResp.json();
      status = pollData.status;
      if (status === 'completed') {
        downloadUrl = pollData.download_url;
        break;
      } else if (status === 'failed') {
        throw new Error(pollData.error_message || 'Conversion engine failed');
      }
    }

    if (!downloadUrl) throw new Error('Conversion result URL missing');
    if (onStatusUpdate) onStatusUpdate('Downloading converted PDF...');
    const pdfResp = await fetch(downloadUrl, { signal });
    if (!pdfResp.ok) throw new Error('Failed to download converted PDF');
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
      item.pricePaise = calculateDocPrice(totalPages, item.settings);
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
        pricePaise: 0
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

    if (settingsFilename) settingsFilename.textContent = doc.name;
    if (previewFilename) previewFilename.textContent = doc.name;

    currentPreviewSheet = 1;
    currentPreviewSide = 'front';

    // Rate labels
    if (settingsBwPriceLabel) settingsBwPriceLabel.textContent = `${formatRupees(bwSimplexPricePaise)}/side`;
    if (settingsColorPriceLabel) settingsColorPriceLabel.textContent = `${formatRupees(colorSimplexPricePaise)}/side`;

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

    // 5. Orientation
    btnOrientAuto.classList.toggle('active', draftSettings.orientation === 'Auto');
    btnOrientPortrait.classList.toggle('active', draftSettings.orientation === 'Portrait');
    btnOrientLandscape.classList.toggle('active', draftSettings.orientation === 'Landscape');

    // 6. Pages per sheet
    cardNup1.classList.add('active');
    cardNup2.classList.remove('active');

    // Live bottom price button
    const livePrice = calculateDocPrice(doc.totalPages, draftSettings);
    if (saveSettingsPrice) {
      saveSettingsPrice.textContent = formatRupees(livePrice);
    }

    // Dynamic visual sync with preview
    if (previewCanvas) {
      previewCanvas.style.filter = (draftSettings.mode === 'B&W') ? 'grayscale(100%) contrast(105%)' : 'none';
    }
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
        syncSelectionState(doc);
        renderPreviewSheet();
      });
    }

    if (btnSelectUncheckAll) {
      btnSelectUncheckAll.addEventListener('click', () => {
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (!doc || !draftSettings) return;
        draftSettings.selectedPages = [];
        syncSelectionState(doc);
        renderPreviewSheet();
      });
    }

    // Copies Stepper
    btnCopiesMinus.addEventListener('click', () => {
      if (draftSettings.copies > 1) {
        draftSettings.copies--;
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (doc) syncSettingsUI(doc);
      }
    });

    btnCopiesPlus.addEventListener('click', () => {
      if (draftSettings.copies < 99) {
        draftSettings.copies++;
        const doc = cartItems.find(d => d.id === activeSettingsDocId);
        if (doc) syncSettingsUI(doc);
      }
    });

    // Print Mode Cards (Instant visual toggle on preview canvas)
    cardModeBw.addEventListener('click', () => {
      draftSettings.mode = 'B&W';
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (doc) {
        syncSettingsUI(doc);
        if (previewCanvas) previewCanvas.style.filter = 'grayscale(100%) contrast(105%)';
      }
    });

    cardModeColor.addEventListener('click', () => {
      draftSettings.mode = 'Color';
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (doc) {
        syncSettingsUI(doc);
        if (previewCanvas) previewCanvas.style.filter = 'none';
      }
    });

    // Print Sides Cards (Instant duplex update)
    cardSideSingle.addEventListener('click', () => {
      draftSettings.sides = 'Single-sided';
      const doc = cartItems.find(d => d.id === activeSettingsDocId);
      if (doc) {
        currentPreviewSide = 'front';
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

    // Orientation Controls (Instant rotation update)
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
      doc.pricePaise = calculateDocPrice(doc.totalPages, doc.settings);

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
    
    // Resolve pages browsable in preview
    let pages = [];
    if (draftSettings && draftSettings.pageSelection === 'Custom Range') {
      const from = Math.max(1, Math.min(draftSettings.rangeFrom || 1, doc.totalPages));
      const to = Math.max(from, Math.min(draftSettings.rangeTo || doc.totalPages, doc.totalPages));
      for (let p = from; p <= to; p++) pages.push(p);
    } else if (draftSettings && draftSettings.pageSelection === 'Selection') {
      // In Selection mode: ONLY the selected pages appear in the preview!
      pages = Array.isArray(draftSettings.selectedPages)
        ? draftSettings.selectedPages.slice().sort((a, b) => a - b)
        : [];
    } else {
      // In 'All' (default):
      pages = Array.from({ length: doc.totalPages }, (_, i) => i + 1);
    }

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

    const totalSheets = isDuplex ? Math.ceil(pages.length / 2) : pages.length;

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
      pageNumToRender = pages[currentPreviewSheet - 1] || null;
    } else {
      if (currentPreviewSide === 'front') {
        pageNumToRender = pages[(currentPreviewSheet - 1) * 2] || null;
      } else {
        const backIdx = (currentPreviewSheet - 1) * 2 + 1;
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

    if (previewLoadingSpinner) previewLoadingSpinner.classList.remove('hidden');
    if (previewErrorBox) previewErrorBox.classList.add('hidden');

    try {
      if (activeRenderTask) {
        try { activeRenderTask.cancel(); } catch (_) {}
        activeRenderTask = null;
      }

      const canvas = previewCanvas;
      if (!canvas) return;
      const ctx = canvas.getContext('2d');

      if (pageNumToRender === null) {
        // Blank back sheet for odd duplex
        canvas.width = 160;
        canvas.height = 226;
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

      // Responsive stage scale: target max dimensions 260px width x 215px height (245px on desktop)
      const maxStageW = (window.innerWidth >= 600) ? 260 : 210;
      const maxStageH = (window.innerWidth >= 600) ? 245 : 210;
      const scale = Math.min(maxStageW / rawViewport.width, maxStageH / rawViewport.height, 1.5);
      const scaledViewport = page.getViewport({ scale: scale, rotation: renderRotation });

      // Crisp rendering support
      const dpr = window.devicePixelRatio || 1;
      canvas.width = Math.round(scaledViewport.width * dpr);
      canvas.height = Math.round(scaledViewport.height * dpr);
      canvas.style.width = `${Math.round(scaledViewport.width)}px`;
      canvas.style.height = `${Math.round(scaledViewport.height)}px`;

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
        const isDuplex = (draftSettings ? draftSettings.sides : doc.settings.sides) === 'Double-sided';
        const pages = (draftSettings && draftSettings.pageSelection === 'Custom Range')
          ? getDocSelectedPages(doc.totalPages, draftSettings)
          : Array.from({ length: doc.totalPages }, (_, i) => i + 1);
        const totalSheets = isDuplex ? Math.ceil(pages.length / 2) : pages.length;
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
    const readyItems = cartItems.filter(it => it.status === 'ready');
    if (readyItems.length === 0) {
      showCartError('Please wait for at least one document to finish preparing.');
      return;
    }

    btnReviewOrder.disabled = true;
    const originalText = btnReviewOrder.innerHTML;
    btnReviewOrder.innerHTML = `<span>Preparing order...</span>`;

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
          page_range: pageRangeVal
        });
      }

      formData.append('metadata', JSON.stringify(metadata));

      if (readyItems.length > 0) {
        const first = readyItems[0];
        const isDouble = first.settings.sides === 'Double-sided';
        formData.append('copies', String(first.settings.copies || 1));
        formData.append('duplex', isDouble ? 'DUPLEX_LONG_EDGE' : 'SIMPLEX');
        formData.append('color_mode', first.settings.mode === 'Color' ? 'COLOR' : 'BW');
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
      activeDisplayCode = String(data.display_code || '0000');

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

      startExpiryCountdown();
      startOrderStatusPolling(activeOrderId);

      switchView('checkout');
    } catch (orderErr) {
      console.error('Order submission error:', orderErr);
      showCartError(`Could not create order: ${orderErr.message || orderErr}`);
    } finally {
      btnReviewOrder.disabled = false;
      btnReviewOrder.innerHTML = originalText;
    }
  }

  function startExpiryCountdown() {
    if (expiryCountdownTimer) clearInterval(expiryCountdownTimer);
    let secondsLeft = 15 * 60;

    function tick() {
      const minutes = Math.floor(secondsLeft / 60);
      const seconds = secondsLeft % 60;
      if (pinExpiryText) {
        pinExpiryText.textContent = `Expires in ${minutes}:${seconds < 10 ? '0' : ''}${seconds}`;
      }
      if (secondsLeft <= 0) {
        clearInterval(expiryCountdownTimer);
        if (pinExpiryText) pinExpiryText.textContent = 'PIN expired. Please create a new order.';
      }
      secondsLeft--;
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
      } catch (_) {}
    }, 2500);
  }

  function updateCheckoutStatus(statusData) {
    if (!statusData || !checkoutLiveStatusBox) return;
    const st = statusData.status || '';

    if (st && st !== 'PENDING_PAYMENT') {
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
          try { it.abortController.abort(); } catch (_) {}
        }
      });
      cartItems = [];
      activeOrderId = null;
      activeDisplayCode = '';
      switchView('cart');
      renderCart();
    });
  }

  // --- Pricing Fetch ---
  async function fetchPricing() {
    try {
      const resp = await fetch(`/api/pricing?kiosk_id=${encodeURIComponent(kioskID)}`);
      if (resp.ok) {
        const data = await resp.json();
        if (typeof data.bw_simplex_price_paise === 'number') bwSimplexPricePaise = data.bw_simplex_price_paise;
        if (typeof data.bw_duplex_price_paise === 'number') bwDuplexPricePaise = data.bw_duplex_price_paise;
        if (typeof data.color_simplex_price_paise === 'number') colorSimplexPricePaise = data.color_simplex_price_paise;
        if (typeof data.color_duplex_price_paise === 'number') colorDuplexPricePaise = data.color_duplex_price_paise;
        colorHardwareEnabled = !!data.color_hardware_enabled;

        // Update settings labels
        if (settingsBwPriceLabel) settingsBwPriceLabel.textContent = `${formatRupees(bwSimplexPricePaise)}/side`;
        if (settingsColorPriceLabel) settingsColorPriceLabel.textContent = `${formatRupees(colorSimplexPricePaise)}/side`;
      }
    } catch (err) {
      console.warn('Could not load remote pricing tiers:', err);
    }
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
    setupBanner();
    setupPwa();
    setupUploadInputs();
    setupSettingsEventListeners();
    setupPreviewEventListeners();
    setupCheckoutEventListeners();
    fetchPricing();
    renderCart();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
