/**
 * XeroxGo Mobile Upload & Print Portal
 * Pure vanilla JavaScript implementation reproducing the reference UI flow.
 */

(function () {
  'use strict';

  // --- Configuration from DOM ---
  const container = document.querySelector('.app-container');
  if (!container) return;

  const kioskID = container.dataset.kioskId || 'kiosk-demo';
  const devMode = container.dataset.devMode === 'true';
  let pricePerSheetPaise = parseInt(container.dataset.pricePerSheetPaise, 10) || 500;

  // --- State Variables ---
  let file = null;
  let fileBuffer = null;
  let pdfLibDoc = null;
  let pdfJsDoc = null;
  let totalDocPages = 0;
  let pageList = [];
  let pageSelection = 'All Pages';
  let customPageRange = '';
  let customRangeError = '';
  let copies = 1;
  let mode = 'B&W';
  let sides = 'Single-sided';
  let pagesPerSheet = '1';
  let userName = '';
  let activeOrderID = null;
  let pollTimer = null;
  let activePageIndex = 0;
  let zoom = 1.0;
  let isFullscreen = false;
  let deferredPwaPrompt = null;

  // --- DOM Elements ---
  const bannerTrack = document.getElementById('banner-track');
  const bannerDots = document.getElementById('banner-dots');
  const bannerCloseBtn = document.getElementById('banner-close-btn');
  const bannerContainer = document.getElementById('promo-banner');

  const pwaInstallBanner = document.getElementById('pwa-install-banner');
  const pwaDismissBtn = document.getElementById('pwa-dismiss-btn');
  const pwaInstallBtn = document.getElementById('pwa-install-btn');
  const pwaMiniPill = document.getElementById('pwa-mini-pill');
  const pwaPillBtn = document.getElementById('pwa-pill-btn');
  const pwaGuideModal = document.getElementById('pwa-guide-modal');
  const pwaModalCloseBtn = document.getElementById('pwa-modal-close-btn');
  const pwaModalGotItBtn = document.getElementById('pwa-modal-gotit-btn');

  const uploadSection = document.getElementById('upload-section');
  const uploadArea = document.getElementById('upload-area');
  const fileInput = document.getElementById('file-input');
  const filePreviewCard = document.getElementById('file-preview-card');
  const fileNameDisplay = document.getElementById('file-name-display');
  const fileMetaDisplay = document.getElementById('file-meta-display');
  const removeFileBtn = document.getElementById('remove-file-btn');
  const btnViewDoc = document.getElementById('btn-view-doc');
  const btnEditPages = document.getElementById('btn-edit-pages');
  const uploadAlert = document.getElementById('upload-alert');

  const printOptionsSection = document.getElementById('print-options-section');
  const btnPageSelectionAll = document.getElementById('btn-page-selection-all');
  const btnPageSelectionCustom = document.getElementById('btn-page-selection-custom');
  const customRangeBox = document.getElementById('custom-range-box');
  const customRangeInput = document.getElementById('custom-range-input');
  const customRangeFeedback = document.getElementById('custom-range-feedback');

  const copiesDecBtn = document.getElementById('copies-dec');
  const copiesIncBtn = document.getElementById('copies-inc');
  const copiesValDisplay = document.getElementById('copies-val');

  const btnModeBW = document.getElementById('btn-mode-bw');
  const btnModeColor = document.getElementById('btn-mode-color');
  const btnSideSingle = document.getElementById('btn-side-single');
  const btnSideDouble = document.getElementById('btn-side-double');

  const btnNUp1 = document.getElementById('btn-nup-1');
  const btnNUp2 = document.getElementById('btn-nup-2');

  const reviewSection = document.getElementById('review-section');
  const reviewCard = document.getElementById('review-card');
  const summaryFileName = document.getElementById('summary-file-name');
  const summaryPages = document.getElementById('summary-pages');
  const summaryOptions = document.getElementById('summary-options');
  const summaryCopies = document.getElementById('summary-copies');
  const summaryPrice = document.getElementById('summary-price');
  const btnEditSettings = document.getElementById('btn-edit-settings');
  const userNameInput = document.getElementById('user-name-input');
  const btnPayUpi = document.getElementById('btn-pay-upi');
  const uploadProgressCard = document.getElementById('upload-progress-card');
  const uploadProgressBar = document.getElementById('upload-progress-bar');
  const reviewAlert = document.getElementById('review-alert');

  // Status & Tracking DOM
  const statusSection = document.getElementById('status-section');
  const statusDisplayCode = document.getElementById('status-display-code');
  const statusBadge = document.getElementById('status-badge');
  const statusProgressFill = document.getElementById('status-progress-fill');
  const statusPagesText = document.getElementById('status-pages-text');
  const statusSheetsText = document.getElementById('status-sheets-text');
  const statusErrorAlert = document.getElementById('status-error-alert');
  const devSimulatePaymentBtn = document.getElementById('dev-simulate-payment-btn');
  const upiPayButtonContainer = document.getElementById('upi-pay-container');
  const statusSuccessAlert = document.getElementById('status-success-alert');

  // Modal DOM
  const editorModalOverlay = document.getElementById('editor-modal-overlay');
  const editorModalCloseBtn = document.getElementById('editor-modal-close-btn');
  const editorModalTitle = document.getElementById('editor-modal-title');
  const btnPrevPage = document.getElementById('btn-prev-page');
  const btnNextPage = document.getElementById('btn-next-page');
  const modalPageIndicator = document.getElementById('modal-page-indicator');
  const btnToggleInclude = document.getElementById('btn-toggle-include');
  const btnZoomOut = document.getElementById('btn-zoom-out');
  const btnZoomReset = document.getElementById('btn-zoom-reset');
  const btnZoomIn = document.getElementById('btn-zoom-in');
  const btnFullscreen = document.getElementById('btn-fullscreen');
  const mainCanvas = document.getElementById('main-preview-canvas');
  const canvasLoadingSpinner = document.getElementById('canvas-loading-spinner');
  const btnSelectAll = document.getElementById('btn-select-all');
  const btnDeselectAll = document.getElementById('btn-deselect-all');
  const btnRotateAll = document.getElementById('btn-rotate-all');
  const modalCustomRangeInput = document.getElementById('modal-custom-range-input');
  const thumbnailCarousel = document.getElementById('thumbnail-carousel');

  // --- Initialize PDF.js worker if available ---
  if (window.pdfjsLib) {
    window.pdfjsLib.GlobalWorkerOptions.workerSrc = '/static/vendor/pdf.worker.min.mjs';
  }

  // --- Helper: Parse Custom Range String ---
  function parseCustomPages(rangeStr, maxPages) {
    if (!rangeStr || !rangeStr.trim()) return [];
    const parts = rangeStr.split(',');
    const selected = new Set();

    for (let part of parts) {
      part = part.trim();
      if (!part) continue;

      if (part.includes('-')) {
        const segs = part.split('-');
        if (segs.length !== 2) return null;
        const start = parseInt(segs[0], 10);
        const end = parseInt(segs[1], 10);
        if (isNaN(start) || isNaN(end) || start < 1 || start > end || start > maxPages) return null;
        for (let i = start; i <= Math.min(end, maxPages); i++) {
          selected.add(i - 1);
        }
      } else {
        const p = parseInt(part, 10);
        if (isNaN(p) || p < 1 || p > maxPages) return null;
        selected.add(p - 1);
      }
    }
    return Array.from(selected);
  }

  // --- Helper: Format Currency ---
  function formatRupees(paise) {
    return '₹' + (paise / 100).toFixed(0);
  }

  // --- Helper: Calculate Selected Count & Price ---
  function getSelectedPagesCount() {
    if (!file) return 0;
    if (pageSelection === 'Custom Range') {
      const parsed = parseCustomPages(customPageRange, totalDocPages);
      if (parsed === null) return 0;
      return parsed.length;
    }
    return pageList.filter(p => p.selected).length;
  }

  function updatePriceAndReviewSummary() {
    const selectedCount = getSelectedPagesCount();
    const isDouble = sides === 'Double-sided';
    const sheetsPerCopy = isDouble ? Math.ceil(selectedCount / 2) : selectedCount;
    const totalSheets = sheetsPerCopy * copies;
    const totalPricePaise = totalSheets * pricePerSheetPaise;

    if (summaryFileName) summaryFileName.textContent = file ? file.name : '-';
    if (summaryPages) summaryPages.textContent = `${selectedCount} ${selectedCount === 1 ? 'page' : 'pages'}`;
    if (summaryOptions) {
      summaryOptions.textContent = `B&W · ${isDouble ? 'Double Side' : 'Single Side'} · 1 page/sheet`;
    }
    if (summaryCopies) summaryCopies.textContent = `${copies} ${copies === 1 ? 'copy' : 'copies'}`;
    if (summaryPrice) summaryPrice.textContent = formatRupees(totalPricePaise);

    // Update feedback for custom range if visible
    if (pageSelection === 'Custom Range') {
      if (!customPageRange.trim()) {
        customRangeFeedback.textContent = 'Enter pages to print (e.g. 1-3, 5)';
        customRangeFeedback.style.color = 'var(--text-secondary)';
      } else {
        const parsed = parseCustomPages(customPageRange, totalDocPages);
        if (parsed === null) {
          customRangeFeedback.textContent = 'Invalid format or page number out of bounds';
          customRangeFeedback.style.color = 'var(--danger-color)';
        } else {
          customRangeFeedback.textContent = `Selected ${parsed.length} page(s)`;
          customRangeFeedback.style.color = 'var(--text-secondary)';
        }
      }
    }
  }

  // --- Promo Banner Carousel ---
  let currentSlide = 0;
  const totalSlides = 3;
  let bannerTimer = null;

  function setSlide(index) {
    currentSlide = (index + totalSlides) % totalSlides;
    if (bannerTrack) {
      bannerTrack.style.transform = `translateX(-${currentSlide * 100}%)`;
    }
    if (bannerDots) {
      const dots = bannerDots.querySelectorAll('.indicator');
      dots.forEach((dot, idx) => {
        dot.classList.toggle('active', idx === currentSlide);
      });
    }
  }

  function startBannerAutoSlide() {
    if (bannerTimer) clearInterval(bannerTimer);
    bannerTimer = setInterval(() => {
      setSlide(currentSlide + 1);
    }, 5000);
  }

  if (bannerDots) {
    bannerDots.querySelectorAll('.indicator').forEach((dot, idx) => {
      dot.addEventListener('click', () => {
        setSlide(idx);
        startBannerAutoSlide();
      });
    });
  }

  if (bannerCloseBtn && bannerContainer) {
    bannerCloseBtn.addEventListener('click', () => {
      if (bannerTimer) clearInterval(bannerTimer);
      bannerContainer.style.display = 'none';
    });
  }

  startBannerAutoSlide();

  // --- PWA Installation Banner & Guide ---
  window.addEventListener('beforeinstallprompt', (e) => {
    e.preventDefault();
    deferredPwaPrompt = e;
    if (pwaInstallBanner) pwaInstallBanner.classList.remove('hidden');
  });

  if (pwaDismissBtn && pwaInstallBanner && pwaMiniPill) {
    pwaDismissBtn.addEventListener('click', () => {
      pwaInstallBanner.classList.add('hidden');
      pwaMiniPill.classList.remove('hidden');
    });
  }

  function triggerPwaInstall() {
    if (deferredPwaPrompt) {
      deferredPwaPrompt.prompt();
      deferredPwaPrompt.userChoice.then(() => {
        deferredPwaPrompt = null;
      });
    } else {
      if (pwaGuideModal) pwaGuideModal.classList.remove('hidden');
    }
  }

  if (pwaInstallBtn) pwaInstallBtn.addEventListener('click', triggerPwaInstall);
  if (pwaPillBtn) pwaPillBtn.addEventListener('click', triggerPwaInstall);

  if (pwaModalCloseBtn && pwaGuideModal) {
    pwaModalCloseBtn.addEventListener('click', () => pwaGuideModal.classList.add('hidden'));
  }
  if (pwaModalGotItBtn && pwaGuideModal) {
    pwaModalGotItBtn.addEventListener('click', () => pwaGuideModal.classList.add('hidden'));
  }
  if (pwaGuideModal) {
    pwaGuideModal.addEventListener('click', (e) => {
      if (e.target === pwaGuideModal) pwaGuideModal.classList.add('hidden');
    });
  }

  // --- File Selection & Inspection ---
  function showError(msg) {
    if (uploadAlert) {
      uploadAlert.textContent = msg;
      uploadAlert.classList.remove('hidden');
    }
  }

  function clearError() {
    if (uploadAlert) {
      uploadAlert.textContent = '';
      uploadAlert.classList.add('hidden');
    }
  }

  async function handleFile(selectedFile) {
    clearError();
    if (!selectedFile) return;

    if (!selectedFile.name.toLowerCase().endsWith('.pdf') && selectedFile.type !== 'application/pdf') {
      showError('Unsupported file format. Please upload a PDF document.');
      return;
    }

    const maxSizeBytes = 25 * 1024 * 1024; // 25 MiB backend limit
    if (selectedFile.size > maxSizeBytes) {
      showError('File size exceeds the 25 MB limit.');
      return;
    }

    try {
      file = selectedFile;
      const buffer = await selectedFile.arrayBuffer();

      // Parse with PDFLib
      if (!window.PDFLib) {
        throw new Error('PDF processing library not loaded.');
      }
      pdfLibDoc = await window.PDFLib.PDFDocument.load(buffer.slice(0));
      totalDocPages = pdfLibDoc.getPageCount();

      // Initialize page list
      pageList = Array.from({ length: totalDocPages }, (_, i) => ({
        originalIndex: i,
        rotation: 0,
        selected: true
      }));

      // Parse with PDF.js for preview canvas
      if (window.pdfjsLib) {
        try {
          const typedArray = new Uint8Array(buffer.slice(0));
          pdfJsDoc = await window.pdfjsLib.getDocument({ data: typedArray }).promise;
        } catch (jsErr) {
          console.warn('PDF.js preview load error:', jsErr);
          pdfJsDoc = null;
        }
      }

      // Update UI
      if (uploadArea) uploadArea.classList.add('hidden');
      if (filePreviewCard) filePreviewCard.classList.remove('hidden');
      if (fileNameDisplay) fileNameDisplay.textContent = file.name;
      if (fileMetaDisplay) {
        fileMetaDisplay.textContent = `${totalDocPages} ${totalDocPages === 1 ? 'page' : 'pages'} • ${(file.size / (1024 * 1024)).toFixed(2)} MB`;
      }

      updatePriceAndReviewSummary();
    } catch (err) {
      console.error('File load failed:', err);
      showError('Failed to parse PDF document. Please verify the file is valid.');
      resetFile();
    }
  }

  function resetFile() {
    file = null;
    fileBuffer = null;
    pdfLibDoc = null;
    pdfJsDoc = null;
    totalDocPages = 0;
    pageList = [];
    customPageRange = '';
    activePageIndex = 0;
    if (fileInput) fileInput.value = '';
    if (uploadSection) uploadSection.classList.remove('hidden');
    if (uploadArea) uploadArea.classList.remove('hidden');
    if (filePreviewCard) filePreviewCard.classList.add('hidden');
    updatePriceAndReviewSummary();
  }

  if (uploadArea) {
    uploadArea.addEventListener('click', () => fileInput && fileInput.click());
    uploadArea.addEventListener('dragover', (e) => {
      e.preventDefault();
      uploadArea.classList.add('drag-active');
    });
    uploadArea.addEventListener('dragleave', () => uploadArea.classList.remove('drag-active'));
    uploadArea.addEventListener('drop', (e) => {
      e.preventDefault();
      uploadArea.classList.remove('drag-active');
      if (e.dataTransfer.files && e.dataTransfer.files[0]) {
        handleFile(e.dataTransfer.files[0]);
      }
    });
  }

  if (fileInput) {
    fileInput.addEventListener('change', (e) => {
      if (e.target.files && e.target.files[0]) {
        handleFile(e.target.files[0]);
      }
    });
  }

  if (removeFileBtn) {
    removeFileBtn.addEventListener('click', resetFile);
  }

  // --- Print Settings Interactions ---
  if (btnPageSelectionAll) {
    btnPageSelectionAll.addEventListener('click', () => {
      pageSelection = 'All Pages';
      btnPageSelectionAll.classList.add('active');
      btnPageSelectionCustom.classList.remove('active');
      if (customRangeBox) customRangeBox.classList.add('hidden');
      pageList.forEach(p => p.selected = true);
      updatePriceAndReviewSummary();
      renderThumbnails();
    });
  }

  if (btnPageSelectionCustom) {
    btnPageSelectionCustom.addEventListener('click', () => {
      pageSelection = 'Custom Range';
      btnPageSelectionCustom.classList.add('active');
      btnPageSelectionAll.classList.remove('active');
      if (customRangeBox) customRangeBox.classList.remove('hidden');
      applyCustomRange(customRangeInput ? customRangeInput.value : '');
    });
  }

  function applyCustomRange(rangeStr) {
    customPageRange = rangeStr;
    if (modalCustomRangeInput && modalCustomRangeInput !== document.activeElement) {
      modalCustomRangeInput.value = rangeStr;
    }
    if (customRangeInput && customRangeInput !== document.activeElement) {
      customRangeInput.value = rangeStr;
    }

    if (!rangeStr.trim()) {
      pageList.forEach(p => p.selected = true);
    } else {
      const selected = parseCustomPages(rangeStr, totalDocPages);
      if (selected !== null) {
        const set = new Set(selected);
        pageList.forEach((p, idx) => {
          p.selected = set.has(idx);
        });
      }
    }
    updatePriceAndReviewSummary();
    renderThumbnails();
    updateModalPill();
  }

  if (customRangeInput) {
    customRangeInput.addEventListener('input', (e) => {
      applyCustomRange(e.target.value);
    });
  }

  if (copiesDecBtn && copiesValDisplay) {
    copiesDecBtn.addEventListener('click', () => {
      if (copies > 1) {
        copies--;
        copiesValDisplay.textContent = copies;
        updatePriceAndReviewSummary();
      }
    });
  }

  if (copiesIncBtn && copiesValDisplay) {
    copiesIncBtn.addEventListener('click', () => {
      if (copies < 99) {
        copies++;
        copiesValDisplay.textContent = copies;
        updatePriceAndReviewSummary();
      }
    });
  }

  if (btnSideSingle) {
    btnSideSingle.addEventListener('click', () => {
      sides = 'Single-sided';
      btnSideSingle.classList.add('active');
      btnSideDouble.classList.remove('active');
      updatePriceAndReviewSummary();
    });
  }

  if (btnSideDouble) {
    btnSideDouble.addEventListener('click', () => {
      sides = 'Double-sided';
      btnSideDouble.classList.add('active');
      btnSideSingle.classList.remove('active');
      updatePriceAndReviewSummary();
    });
  }

  // Edit Settings Button scrolls to Section 2
  if (btnEditSettings && printOptionsSection) {
    btnEditSettings.addEventListener('click', () => {
      printOptionsSection.scrollIntoView({ behavior: 'smooth' });
    });
  }

  // --- Document Review & Page Editor Modal ---
  function openEditorModal() {
    if (!file) {
      showError('Please upload a document first.');
      return;
    }
    if (editorModalTitle) editorModalTitle.textContent = file.name;
    if (editorModalOverlay) editorModalOverlay.classList.remove('hidden');
    renderCurrentPage();
    renderThumbnails();
    updateModalPill();
  }

  function closeEditorModal() {
    if (editorModalOverlay) editorModalOverlay.classList.add('hidden');
  }

  if (btnViewDoc) btnViewDoc.addEventListener('click', openEditorModal);
  if (btnEditPages) btnEditPages.addEventListener('click', openEditorModal);
  if (editorModalCloseBtn) editorModalCloseBtn.addEventListener('click', closeEditorModal);

  function updateModalPill() {
    if (!pageList[activePageIndex]) return;
    const isIncluded = pageList[activePageIndex].selected;
    if (btnToggleInclude) {
      btnToggleInclude.style.backgroundColor = isIncluded ? '#DCFCE7' : '#FEE2E2';
      btnToggleInclude.style.color = isIncluded ? '#166534' : '#991B1B';
      btnToggleInclude.innerHTML = isIncluded
        ? '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3"><polyline points="20 6 9 17 4 12"/></svg> Included'
        : '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg> Excluded';
    }
    if (modalPageIndicator) {
      modalPageIndicator.textContent = `Page ${pageList.length > 0 ? activePageIndex + 1 : 0} of ${pageList.length}`;
    }
    if (btnPrevPage) btnPrevPage.disabled = activePageIndex <= 0;
    if (btnNextPage) btnNextPage.disabled = activePageIndex >= pageList.length - 1;
  }

  async function renderCurrentPage() {
    if (!mainCanvas || !pdfJsDoc || pageList.length === 0) return;
    const pageObj = pageList[activePageIndex];
    if (!pageObj) return;

    if (canvasLoadingSpinner) canvasLoadingSpinner.classList.remove('hidden');

    try {
      const pdfPage = await pdfJsDoc.getPage(pageObj.originalIndex + 1);
      const rotation = (pdfPage.rotate + pageObj.rotation) % 360;
      const baseViewport = pdfPage.getViewport({ scale: 1.0, rotation });
      const maxWidth = isFullscreen ? window.innerWidth * 0.8 : 360;
      const fitScale = Math.min(2.0, Math.max(0.5, (maxWidth / baseViewport.width) * zoom));
      const viewport = pdfPage.getViewport({ scale: fitScale, rotation });

      mainCanvas.width = viewport.width;
      mainCanvas.height = viewport.height;
      const ctx = mainCanvas.getContext('2d');

      await pdfPage.render({ canvasContext: ctx, viewport }).promise;
    } catch (err) {
      console.warn('Canvas page render error:', err);
    } finally {
      if (canvasLoadingSpinner) canvasLoadingSpinner.classList.add('hidden');
    }
  }

  if (btnPrevPage) {
    btnPrevPage.addEventListener('click', () => {
      if (activePageIndex > 0) {
        activePageIndex--;
        updateModalPill();
        renderCurrentPage();
      }
    });
  }

  if (btnNextPage) {
    btnNextPage.addEventListener('click', () => {
      if (activePageIndex < pageList.length - 1) {
        activePageIndex++;
        updateModalPill();
        renderCurrentPage();
      }
    });
  }

  if (btnToggleInclude) {
    btnToggleInclude.addEventListener('click', () => {
      if (pageList[activePageIndex]) {
        pageList[activePageIndex].selected = !pageList[activePageIndex].selected;
        updateModalPill();
        updatePriceAndReviewSummary();
        renderThumbnails();
      }
    });
  }

  if (btnZoomIn) {
    btnZoomIn.addEventListener('click', () => {
      zoom = Math.min(2.0, zoom + 0.2);
      if (btnZoomReset) btnZoomReset.textContent = Math.round(zoom * 100) + '%';
      renderCurrentPage();
    });
  }

  if (btnZoomOut) {
    btnZoomOut.addEventListener('click', () => {
      zoom = Math.max(0.6, zoom - 0.2);
      if (btnZoomReset) btnZoomReset.textContent = Math.round(zoom * 100) + '%';
      renderCurrentPage();
    });
  }

  if (btnZoomReset) {
    btnZoomReset.addEventListener('click', () => {
      zoom = 1.0;
      btnZoomReset.textContent = '100%';
      renderCurrentPage();
    });
  }

  if (btnFullscreen) {
    btnFullscreen.addEventListener('click', () => {
      isFullscreen = !isFullscreen;
      renderCurrentPage();
    });
  }

  if (btnSelectAll) {
    btnSelectAll.addEventListener('click', () => {
      pageList.forEach(p => p.selected = true);
      updateModalPill();
      updatePriceAndReviewSummary();
      renderThumbnails();
    });
  }

  if (btnDeselectAll) {
    btnDeselectAll.addEventListener('click', () => {
      pageList.forEach(p => p.selected = false);
      updateModalPill();
      updatePriceAndReviewSummary();
      renderThumbnails();
    });
  }

  if (btnRotateAll) {
    btnRotateAll.addEventListener('click', () => {
      pageList.forEach(p => p.rotation = (p.rotation + 90) % 360);
      renderCurrentPage();
      renderThumbnails();
    });
  }

  if (modalCustomRangeInput) {
    modalCustomRangeInput.addEventListener('input', (e) => {
      applyCustomRange(e.target.value);
    });
  }

  function renderThumbnails() {
    if (!thumbnailCarousel) return;
    thumbnailCarousel.innerHTML = '';

    pageList.forEach((pageObj, idx) => {
      const thumb = document.createElement('div');
      thumb.className = 'thumb-item';
      thumb.style.flexShrink = '0';
      thumb.style.width = '80px';
      thumb.style.border = `2px solid ${idx === activePageIndex ? 'var(--primary-color)' : pageObj.selected ? '#93C5FD' : '#FCA5A5'}`;
      thumb.style.borderRadius = '6px';
      thumb.style.backgroundColor = '#FFFFFF';
      thumb.style.padding = '4px';
      thumb.style.cursor = 'pointer';
      thumb.style.display = 'flex';
      thumb.style.flexDirection = 'column';
      thumb.style.alignItems = 'center';

      thumb.innerHTML = `
        <div style="display:flex;width:100%;justify-content:space-between;align-items:center;margin-bottom:2px;">
          <span style="font-size:0.7rem;font-weight:700;color:#334155;">P.${idx + 1}</span>
          <input type="checkbox" ${pageObj.selected ? 'checked' : ''} style="cursor:pointer;" class="thumb-check">
        </div>
        <div style="width:100%;height:64px;background-color:${pageObj.selected ? '#F8FAFC' : '#FEF2F2'};border-radius:4px;display:flex;align-items:center;justify-content:center;position:relative;overflow:hidden;">
          <div style="transform:rotate(${pageObj.rotation}deg);transition:transform 0.2s ease;">
            <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="${pageObj.selected ? '#2563EB' : '#EF4444'}" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>
          </div>
          ${pageObj.rotation > 0 ? `<span style="position:absolute;bottom:2px;right:2px;font-size:0.6rem;font-weight:800;background:#DBEAFE;color:#1E40AF;padding:0 3px;border-radius:3px;">${pageObj.rotation}°</span>` : ''}
        </div>
        <div style="display:flex;gap:3px;margin-top:4px;width:100%;justify-content:center;">
          <button type="button" class="thumb-btn-rot" style="border:none;background:#E0F2FE;color:#0369A1;border-radius:3px;padding:2px 4px;cursor:pointer;" title="Rotate 90°">↻</button>
          <button type="button" class="thumb-btn-del" style="border:none;background:#FEE2E2;color:#B91C1C;border-radius:3px;padding:2px 4px;cursor:pointer;" title="Delete Page">🗑</button>
        </div>
      `;

      thumb.addEventListener('click', () => {
        activePageIndex = idx;
        updateModalPill();
        renderCurrentPage();
        renderThumbnails();
      });

      const chk = thumb.querySelector('.thumb-check');
      if (chk) {
        chk.addEventListener('click', (e) => {
          e.stopPropagation();
          pageObj.selected = chk.checked;
          updateModalPill();
          updatePriceAndReviewSummary();
          renderThumbnails();
        });
      }

      const rot = thumb.querySelector('.thumb-btn-rot');
      if (rot) {
        rot.addEventListener('click', (e) => {
          e.stopPropagation();
          pageObj.rotation = (pageObj.rotation + 90) % 360;
          if (idx === activePageIndex) renderCurrentPage();
          renderThumbnails();
        });
      }

      const del = thumb.querySelector('.thumb-btn-del');
      if (del) {
        del.addEventListener('click', (e) => {
          e.stopPropagation();
          if (pageList.length <= 1) {
            alert('Document must have at least 1 page.');
            return;
          }
          pageList.splice(idx, 1);
          if (activePageIndex >= pageList.length) {
            activePageIndex = Math.max(0, pageList.length - 1);
          }
          updateModalPill();
          updatePriceAndReviewSummary();
          renderCurrentPage();
          renderThumbnails();
        });
      }

      thumbnailCarousel.appendChild(thumb);
    });
  }

  // --- Document Upload & Local PDF Extraction ---
  async function prepareUploadDocument() {
    if (!file) return file;

    const selectedPages = pageList.filter(p => p.selected);
    if (selectedPages.length === 0) {
      throw new Error('Please select at least 1 page to print.');
    }

    const hasEdits = (
      selectedPages.length < totalDocPages ||
      selectedPages.some(p => p.rotation !== 0) ||
      selectedPages.some((p, idx) => p.originalIndex !== idx)
    );

    if (!hasEdits) {
      return file;
    }

    // Extract selected pages and rotations using PDFLib
    try {
      const freshBuffer = await file.arrayBuffer();
      const srcDoc = await window.PDFLib.PDFDocument.load(freshBuffer);
      const outDoc = await window.PDFLib.PDFDocument.create();

      for (const p of selectedPages) {
        const [copiedPage] = await outDoc.copyPages(srcDoc, [p.originalIndex]);
        if (p.rotation > 0) {
          const curRot = copiedPage.getRotation().angle;
          copiedPage.setRotation(window.PDFLib.degrees((curRot + p.rotation) % 360));
        }
        outDoc.addPage(copiedPage);
      }

      const outBytes = await outDoc.save();
      return new File([outBytes], file.name, { type: 'application/pdf' });
    } catch (extractErr) {
      console.error('PDF page extraction failed:', extractErr);
      throw new Error('Failed to extract selected PDF pages: ' + (extractErr.message || extractErr));
    }
  }

  async function handleOrderSubmission() {
    if (reviewAlert) reviewAlert.classList.add('hidden');

    if (!file) {
      showError('Please upload a document first.');
      return;
    }

    const selectedCount = getSelectedPagesCount();
    if (selectedCount === 0) {
      showError('Please select at least 1 page to print.');
      return;
    }

    if (btnPayUpi) btnPayUpi.disabled = true;
    if (uploadProgressCard) uploadProgressCard.classList.remove('hidden');

    try {
      // 1. Process custom page range / rotation if present
      const uploadDoc = await prepareUploadDocument();

      // 2. Prepare FormData
      const formData = new FormData();
      formData.append('file', uploadDoc);
      formData.append('copies', copies.toString());
      formData.append('duplex', sides === 'Double-sided' ? 'DUPLEX_LONG_EDGE' : 'SIMPLEX');

      // 3. Submit to server
      const uploadUrl = `/api/k/${encodeURIComponent(kioskID)}/upload`;
      const resp = await fetch(uploadUrl, {
        method: 'POST',
        body: formData
      });

      if (!resp.ok) {
        const errText = await resp.text();
        throw new Error(errText || `Upload failed (${resp.status})`);
      }

      const data = await resp.json();
      activeOrderID = data.order_id;

      // 4. Transition to Status Section
      if (uploadSection) uploadSection.classList.add('hidden');
      if (printOptionsSection) printOptionsSection.classList.add('hidden');
      if (reviewSection) reviewSection.classList.add('hidden');
      if (uploadArea) uploadArea.classList.add('hidden');
      if (filePreviewCard) filePreviewCard.classList.add('hidden');
      if (statusSection) statusSection.classList.remove('hidden');

      statusDisplayCode.textContent = data.display_code || '----';
      updateStatusDisplay(data.status, 0, data.total_pages, 0, data.total_sheets, '');

      // Dev Mode payment button
      if (devMode && devSimulatePaymentBtn) {
        devSimulatePaymentBtn.classList.remove('hidden');
        devSimulatePaymentBtn.onclick = async () => {
          devSimulatePaymentBtn.disabled = true;
          try {
            const simResp = await fetch(`/api/orders/${encodeURIComponent(activeOrderID)}/simulate-payment`, {
              method: 'POST'
            });
            if (!simResp.ok) {
              const errData = await simResp.json().catch(() => ({}));
              throw new Error(errData.error || 'Payment simulation failed');
            }
            const simData = await simResp.json();
            updateStatusDisplay(simData.status, 0, data.total_pages, 0, data.total_sheets, '');
          } catch (simErr) {
            alert(simErr.message);
            devSimulatePaymentBtn.disabled = false;
          }
        };
      }

      // Start live order status polling
      startPolling(activeOrderID);

      statusSection.scrollIntoView({ behavior: 'smooth' });
    } catch (err) {
      console.error('Order submission error:', err);
      if (reviewAlert) {
        reviewAlert.textContent = err.message || 'Submission failed. Please try again.';
        reviewAlert.classList.remove('hidden');
      } else {
        alert(err.message || 'Submission failed');
      }
    } finally {
      if (btnPayUpi) btnPayUpi.disabled = false;
      if (uploadProgressCard) uploadProgressCard.classList.add('hidden');
    }
  }

  if (btnPayUpi) {
    btnPayUpi.addEventListener('click', handleOrderSubmission);
  }

  // --- Live Status Polling ---
  function updateStatusDisplay(status, pagesPrinted, totalPages, sheetsPrinted, totalSheets, errReason) {
    if (statusBadge) {
      statusBadge.textContent = status;
      statusBadge.className = `status-pill status-${status}`;
    }

    if (totalPages > 0) {
      const pct = Math.min(100, Math.round((pagesPrinted / totalPages) * 100));
      if (statusProgressFill) statusProgressFill.style.width = pct + '%';
      if (statusPagesText) statusPagesText.textContent = `${pagesPrinted} / ${totalPages}`;
    }

    if (totalSheets > 0 && statusSheetsText) {
      statusSheetsText.textContent = `${sheetsPrinted} / ${totalSheets}`;
    }

    if (errReason && statusErrorAlert) {
      statusErrorAlert.textContent = errReason;
      statusErrorAlert.classList.remove('hidden');
    } else if (statusErrorAlert) {
      statusErrorAlert.classList.add('hidden');
    }

    if (status === 'COMPLETED' && statusSuccessAlert) {
      statusSuccessAlert.classList.remove('hidden');
      if (devSimulatePaymentBtn) devSimulatePaymentBtn.classList.add('hidden');
    }
  }

  function startPolling(orderID) {
    if (pollTimer) clearInterval(pollTimer);
    pollTimer = setInterval(async () => {
      try {
        const resp = await fetch(`/api/orders/${encodeURIComponent(orderID)}/status`);
        if (!resp.ok) return;
        const data = await resp.json();

        updateStatusDisplay(
          data.status,
          data.pages_printed,
          data.total_pages,
          data.sheets_printed,
          data.total_sheets,
          data.error_reason
        );

        if (data.status === 'COMPLETED' || data.status === 'PAUSED_ERROR' || data.status === 'FAILED_REFUNDABLE') {
          if (data.status === 'COMPLETED') {
            clearInterval(pollTimer);
          }
        }
      } catch (_) {}
    }, 1200);
  }
})();
