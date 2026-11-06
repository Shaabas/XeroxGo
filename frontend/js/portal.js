/**
 * XeroxGo Mobile Upload & Print Portal
 * Pure vanilla JavaScript implementation reproducing the reference UI flow.
 */

(function () {
  'use strict';

  // --- Configuration from DOM ---
  const container = document.querySelector('.app-container');
  if (!container) return;

  let kioskID = container.dataset.kioskId;
  const pathMatch = window.location.pathname.match(/\/k\/([^/]+)/);
  const searchParams = new URLSearchParams(window.location.search);
  const urlKiosk = (pathMatch && pathMatch[1]) || searchParams.get('kiosk_id');
  if (urlKiosk) {
    kioskID = urlKiosk;
  } else if (!kioskID) {
    kioskID = 'kiosk-demo';
  }
  const devMode = container.dataset.devMode === 'true';
  let bwSimplexPricePaise = parseInt(container.dataset.pricePerSheetPaise, 10) || 200;
  let bwDuplexPricePaise = 150;
  let colorSimplexPricePaise = 1000;
  let colorDuplexPricePaise = 750;
  let colorHardwareEnabled = false;
  let bwPricePerSheetPaise = bwSimplexPricePaise;
  let colorPricePerSheetPaise = colorSimplexPricePaise;

  // --- State Variables ---
  let file = null;
  let fileBuffer = null;
  let convertedPdfBytes = null;
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

  // Live Sheet Preview DOM
  const liveSheetContainer = document.getElementById('live-sheet-container');
  const paperSheet = document.getElementById('paper-sheet');
  const livePreviewCanvas = document.getElementById('live-preview-canvas');
  const sheetLoadingSpinner = document.getElementById('sheet-loading-spinner');
  const previewColorBadge = document.getElementById('preview-color-badge');
  const previewSheetBadge = document.getElementById('preview-sheet-badge');
  const sheetPageIndicator = document.getElementById('sheet-page-indicator');
  const btnSheetPrev = document.getElementById('btn-sheet-prev');
  const btnSheetNext = document.getElementById('btn-sheet-next');
  const btnSheetFlip = document.getElementById('btn-sheet-flip');
  const sheetFlipText = document.getElementById('sheet-flip-text');

  // --- Initialize PDF.js worker if available ---
  if (window.pdfjsLib && !window.pdfjsLib.GlobalWorkerOptions.workerSrc) {
    window.pdfjsLib.GlobalWorkerOptions.workerSrc = '/vendor/pdf.worker.min.mjs';
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
    const totalPrintedPages = selectedCount * copies;

    let activePrice;
    if (mode === 'Color' && colorHardwareEnabled) {
      activePrice = isDouble ? colorDuplexPricePaise : colorSimplexPricePaise;
    } else {
      activePrice = isDouble ? bwDuplexPricePaise : bwSimplexPricePaise;
    }
    const totalPricePaise = totalPrintedPages * activePrice;

    if (summaryFileName) summaryFileName.textContent = file ? file.name : '-';
    if (summaryPages) summaryPages.textContent = `${selectedCount} ${selectedCount === 1 ? 'page' : 'pages'}`;
    if (summaryOptions) {
      summaryOptions.textContent = `${mode} · ${isDouble ? 'Double Side' : 'Single Side'} · 1 page/sheet`;
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
  let totalSlides = 3;
  let bannerTimer = null;

  function setSlide(index) {
    if (totalSlides <= 0) return;
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
    if (totalSlides > 1) {
      bannerTimer = setInterval(() => {
        setSlide(currentSlide + 1);
      }, 5000);
    }
  }

  function bindBannerDots() {
    if (bannerDots) {
      bannerDots.querySelectorAll('.indicator').forEach((dot, idx) => {
        dot.addEventListener('click', () => {
          setSlide(idx);
          startBannerAutoSlide();
        });
      });
    }
  }

  if (bannerCloseBtn && bannerContainer) {
    bannerCloseBtn.addEventListener('click', () => {
      if (bannerTimer) clearInterval(bannerTimer);
      bannerContainer.style.display = 'none';
    });
  }

  bindBannerDots();
  startBannerAutoSlide();

  // Dynamic Ad Fetching & Synchronization
  async function loadDynamicAds() {
    try {
      const res = await fetch('/api/ads');
      if (!res.ok) return;
      const data = await res.json();
      const ads = Array.isArray(data) ? data : (data.ads || []);
      if (!Array.isArray(ads) || ads.length === 0) return;

      if (!bannerTrack || !bannerDots) return;

      // Render dynamic slides
      bannerTrack.innerHTML = '';
      bannerDots.innerHTML = '';

      ads.forEach((ad, idx) => {
        const slide = document.createElement('div');
        slide.className = 'banner-slide';
        if (ad.type === 'image' && ad.image_url) {
          slide.style.background = `url('${ad.image_url}') center/cover no-repeat`;
        } else {
          const c0 = ad.gradient_c0 || '#1E3A8A';
          const c1 = ad.gradient_c1 || '#2563EB';
          const c2 = ad.gradient_c2 || '#4F46E5';
          slide.style.background = `linear-gradient(135deg, ${c0} 0%, ${c1} 50%, ${c2} 100%)`;
        }

        if (ad.link_url && (ad.link_url.startsWith('https://') || ad.link_url.startsWith('http://'))) {
          slide.style.cursor = 'pointer';
          slide.title = `Open ${ad.link_url}`;
          slide.addEventListener('click', () => {
            window.open(ad.link_url, '_blank', 'noopener,noreferrer');
          });
        }

        const titleEl = document.createElement('div');
        titleEl.className = 'banner-title';
        titleEl.textContent = ad.title || '';

        const descEl = document.createElement('div');
        descEl.className = 'banner-desc';
        descEl.textContent = ad.description || '';

        slide.appendChild(titleEl);
        slide.appendChild(descEl);
        bannerTrack.appendChild(slide);

        const dot = document.createElement('div');
        dot.className = `indicator ${idx === 0 ? 'active' : ''}`;
        bannerDots.appendChild(dot);
      });

      totalSlides = ads.length;
      currentSlide = 0;
      setSlide(0);
      bindBannerDots();
      startBannerAutoSlide();
    } catch (_) {
      // Graceful fallback to static HTML slides
    }
  }

  loadDynamicAds();

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
  const defaultUploadAreaHTML = uploadArea ? uploadArea.innerHTML : '';
  const officeExtensions = ['.doc', '.docx', '.ppt', '.pptx', '.xls', '.xlsx', '.odt', '.ods', '.odp', '.rtf', '.txt'];
  const imageExtensions = ['.jpg', '.jpeg', '.png', '.bmp', '.webp', '.tif', '.tiff'];

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

  async function convertImageToPdf(selectedFile) {
    if (!window.PDFLib) {
      throw new Error('PDF processing library not loaded.');
    }
    const pdfDoc = await window.PDFLib.PDFDocument.create();
    const page = pdfDoc.addPage([595.28, 841.89]); // A4 in points (72 DPI)

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
      // For BMP, WEBP, etc: render to canvas and export as PNG
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

    const margin = 36; // 0.5 inch margins
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

  async function convertDocumentOnServer(selectedFile) {
    const formData = new FormData();
    formData.append('file', selectedFile);
    const resp = await fetch('/api/convert', {
      method: 'POST',
      body: formData,
    });
    if (!resp.ok) {
      const errText = await resp.text();
      throw new Error(errText || 'Document conversion failed on server');
    }
    return await resp.arrayBuffer();
  }

  async function handleFile(selectedFile) {
    clearError();
    if (!selectedFile) return;

    const nameLower = selectedFile.name.toLowerCase();
    const isPDF = nameLower.endsWith('.pdf') || selectedFile.type === 'application/pdf';
    const isOffice = officeExtensions.some(ext => nameLower.endsWith(ext));
    const isImage = imageExtensions.some(ext => nameLower.endsWith(ext)) || selectedFile.type.startsWith('image/');

    if (!isPDF && !isOffice && !isImage) {
      showError('Unsupported file format. Please upload PDF, Word (DOCX/DOC), PowerPoint (PPTX), Excel (XLSX), or Images (JPG, PNG).');
      return;
    }

    const maxSizeBytes = 25 * 1024 * 1024; // 25 MiB backend limit
    if (selectedFile.size > maxSizeBytes) {
      showError('File size exceeds the 25 MB limit.');
      return;
    }

    try {
      file = selectedFile;
      let buffer = null;

      if (isPDF) {
        buffer = await selectedFile.arrayBuffer();
        convertedPdfBytes = null;
      } else if (isImage) {
        if (uploadArea) {
          uploadArea.innerHTML = `
            <div style="display:flex; flex-direction:column; align-items:center; justify-content:center; padding:1.5rem 0;">
              <div style="width:36px; height:36px; border:3px solid #E2E8F0; border-top-color:#2563EB; border-radius:50%; animation:spin 0.8s linear infinite; margin-bottom:0.75rem;"></div>
              <div style="font-weight:600; color:var(--text-primary); font-size:1rem;">Formatting image into print page...</div>
            </div>
          `;
        }
        const pdfBytes = await convertImageToPdf(selectedFile);
        buffer = pdfBytes.buffer;
        convertedPdfBytes = pdfBytes;
      } else if (isOffice) {
        if (uploadArea) {
          uploadArea.innerHTML = `
            <div style="display:flex; flex-direction:column; align-items:center; justify-content:center; padding:1.5rem 0;">
              <div style="width:36px; height:36px; border:3px solid #E2E8F0; border-top-color:#2563EB; border-radius:50%; animation:spin 0.8s linear infinite; margin-bottom:0.75rem;"></div>
              <div style="font-weight:600; color:var(--text-primary); font-size:1rem;">Converting document to PDF...</div>
              <div style="font-size:0.85rem; color:var(--text-secondary); margin-top:0.25rem;">This only takes a couple of seconds</div>
            </div>
          `;
        }
        const pdfArrayBuffer = await convertDocumentOnServer(selectedFile);
        buffer = pdfArrayBuffer;
        convertedPdfBytes = new Uint8Array(pdfArrayBuffer);
      }

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
      if (uploadArea) {
        uploadArea.innerHTML = defaultUploadAreaHTML;
        uploadArea.classList.add('hidden');
      }
      if (filePreviewCard) filePreviewCard.classList.remove('hidden');
      if (fileNameDisplay) fileNameDisplay.textContent = file.name;
      if (fileMetaDisplay) {
        fileMetaDisplay.textContent = `${totalDocPages} ${totalDocPages === 1 ? 'page' : 'pages'} • ${(file.size / (1024 * 1024)).toFixed(2)} MB`;
      }

      updatePriceAndReviewSummary();
      activePageIndex = 0;
      renderLiveSheet();
    } catch (err) {
      console.error('File load failed:', err);
      showError(err.message || 'Failed to process document. Please verify the file is valid.');
      resetFile();
    }
  }

  function resetFile() {
    file = null;
    fileBuffer = null;
    convertedPdfBytes = null;
    pdfLibDoc = null;
    pdfJsDoc = null;
    totalDocPages = 0;
    pageList = [];
    customPageRange = '';
    activePageIndex = 0;
    if (fileInput) fileInput.value = '';
    if (uploadSection) uploadSection.classList.remove('hidden');
    if (uploadArea) {
      uploadArea.innerHTML = defaultUploadAreaHTML;
      uploadArea.classList.remove('hidden');
    }
    if (filePreviewCard) filePreviewCard.classList.add('hidden');
    if (livePreviewCanvas) {
      const ctx = livePreviewCanvas.getContext('2d');
      if (ctx) ctx.clearRect(0, 0, livePreviewCanvas.width, livePreviewCanvas.height);
    }
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
      renderLiveSheet();
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
    renderLiveSheet();
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

  if (btnModeBW) {
    btnModeBW.addEventListener('click', () => {
      mode = 'B&W';
      btnModeBW.classList.add('active');
      if (btnModeColor) btnModeColor.classList.remove('active');
      updatePriceAndReviewSummary();
      renderLiveSheet();
    });
  }

  if (btnModeColor) {
    btnModeColor.addEventListener('click', () => {
      mode = 'Color';
      btnModeColor.classList.add('active');
      if (btnModeBW) btnModeBW.classList.remove('active');
      updatePriceAndReviewSummary();
      renderLiveSheet();
    });
  }

  async function fetchPricing() {
    try {
      const res = await fetch('/api/pricing');
      if (res.ok) {
        const data = await res.json();
        bwSimplexPricePaise = data.bw_simplex_price_paise || data.bw_price_per_sheet_paise || 200;
        bwDuplexPricePaise = data.bw_duplex_price_paise || 150;
        colorSimplexPricePaise = data.color_simplex_price_paise || data.color_price_per_sheet_paise || 1000;
        colorDuplexPricePaise = data.color_duplex_price_paise || 750;
        colorHardwareEnabled = Boolean(data.color_enabled);

        bwPricePerSheetPaise = bwSimplexPricePaise;
        colorPricePerSheetPaise = colorSimplexPricePaise;

        // Hardware capability: if kiosk color printing is disabled, lock mobile UI to B&W
        if (!colorHardwareEnabled) {
          if (btnModeColor) btnModeColor.style.display = 'none';
          mode = 'B&W';
          if (btnModeBW) {
            btnModeBW.classList.add('active');
            btnModeBW.style.flex = '1';
          }
        } else {
          if (btnModeColor) btnModeColor.style.display = 'flex';
          if (btnModeBW) btnModeBW.style.flex = '';
        }

        const bwLabel = document.getElementById('price-bw-label');
        if (bwLabel) bwLabel.textContent = `₹${(bwSimplexPricePaise / 100).toFixed(2)}/page`;
        const colorLabel = document.getElementById('price-color-label');
        if (colorLabel) colorLabel.textContent = `₹${(colorSimplexPricePaise / 100).toFixed(2)}/page`;
        updatePriceAndReviewSummary();
      }
    } catch (_) {}
  }
  fetchPricing();

  if (btnSideSingle) {
    btnSideSingle.addEventListener('click', () => {
      sides = 'Single-sided';
      btnSideSingle.classList.add('active');
      btnSideDouble.classList.remove('active');
      updatePriceAndReviewSummary();
      renderLiveSheet();
    });
  }

  if (btnSideDouble) {
    btnSideDouble.addEventListener('click', () => {
      sides = 'Double-sided';
      btnSideDouble.classList.add('active');
      btnSideSingle.classList.remove('active');
      updatePriceAndReviewSummary();
      renderLiveSheet();
    });
  }

  // Edit Settings Button scrolls to Section 2
  if (btnEditSettings && printOptionsSection) {
    btnEditSettings.addEventListener('click', () => {
      printOptionsSection.scrollIntoView({ behavior: 'smooth' });
    });
  }

  // --- Live Print-Ready Sheet Preview Engine ---
  let renderTask = null;

  async function renderLiveSheet() {
    if (!livePreviewCanvas || !pdfJsDoc || pageList.length === 0) return;

    // Determine the list of pages selected to print
    const printablePages = pageList.filter(p => p.selected);
    if (printablePages.length === 0) {
      if (previewSheetBadge) previewSheetBadge.textContent = '0 pages selected';
      if (sheetPageIndicator) sheetPageIndicator.textContent = 'No pages selected';
      if (btnSheetPrev) btnSheetPrev.disabled = true;
      if (btnSheetNext) btnSheetNext.disabled = true;
      if (btnSheetFlip) btnSheetFlip.classList.add('hidden');
      return;
    }

    // Ensure activePageIndex points to a valid selected page
    let activePageObj = pageList[activePageIndex];
    if (!activePageObj || !activePageObj.selected) {
      activePageObj = printablePages[0];
      activePageIndex = activePageObj.originalIndex;
    }

    // Find position in printablePages
    const selIdx = printablePages.findIndex(p => p.originalIndex === activePageIndex);
    const totalPrintable = printablePages.length;

    // 1. Color vs B&W WYSIWYG reaction
    if (mode === 'B&W') {
      livePreviewCanvas.classList.add('preview-bw');
      livePreviewCanvas.classList.remove('preview-color');
      if (previewColorBadge) {
        previewColorBadge.className = 'sheet-badge badge-bw';
        previewColorBadge.innerHTML = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><circle cx="12" cy="12" r="10"/><path d="M12 2a10 10 0 0 1 0 20z" fill="currentColor"/></svg> B&amp;W Preview';
      }
    } else {
      livePreviewCanvas.classList.remove('preview-bw');
      livePreviewCanvas.classList.add('preview-color');
      if (previewColorBadge) {
        previewColorBadge.className = 'sheet-badge badge-color';
        previewColorBadge.innerHTML = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><circle cx="12" cy="12" r="10"/></svg> Color Preview';
      }
    }

    // 2. Duplex vs Simplex Sheet Calculation
    if (sides === 'Double-sided') {
      const sheetNum = Math.floor(selIdx / 2) + 1;
      const totalSheets = Math.ceil(totalPrintable / 2);
      const isFront = (selIdx % 2 === 0);

      if (previewSheetBadge) {
        previewSheetBadge.textContent = `Sheet ${sheetNum} of ${totalSheets} (${isFront ? 'Front' : 'Back'})`;
      }

      // Flip button
      if (btnSheetFlip && sheetFlipText) {
        if (isFront && selIdx + 1 < totalPrintable) {
          btnSheetFlip.classList.remove('hidden');
          sheetFlipText.textContent = `Flip to Back (Pg ${printablePages[selIdx + 1].originalIndex + 1})`;
          btnSheetFlip.onclick = () => {
            activePageIndex = printablePages[selIdx + 1].originalIndex;
            renderLiveSheet();
          };
        } else if (!isFront && selIdx - 1 >= 0) {
          btnSheetFlip.classList.remove('hidden');
          sheetFlipText.textContent = `Flip to Front (Pg ${printablePages[selIdx - 1].originalIndex + 1})`;
          btnSheetFlip.onclick = () => {
            activePageIndex = printablePages[selIdx - 1].originalIndex;
            renderLiveSheet();
          };
        } else {
          btnSheetFlip.classList.add('hidden');
        }
      }
    } else {
      // Simplex (Single-sided)
      const sheetNum = selIdx + 1;
      if (previewSheetBadge) {
        previewSheetBadge.textContent = `Sheet ${sheetNum} of ${totalPrintable}`;
      }
      if (btnSheetFlip) {
        btnSheetFlip.classList.add('hidden');
      }
    }

    // 3. Page Stepper
    if (sheetPageIndicator) {
      if (totalPrintable === totalDocPages) {
        sheetPageIndicator.textContent = `Page ${activePageObj.originalIndex + 1} of ${totalDocPages}`;
      } else {
        sheetPageIndicator.textContent = `Page ${activePageObj.originalIndex + 1} (${selIdx + 1} of ${totalPrintable})`;
      }
    }

    if (btnSheetPrev) btnSheetPrev.disabled = (selIdx <= 0);
    if (btnSheetNext) btnSheetNext.disabled = (selIdx >= totalPrintable - 1);

    // --- Render Canvas Page ---
    if (sheetLoadingSpinner) sheetLoadingSpinner.classList.remove('hidden');

    if (renderTask) {
      try { renderTask.cancel(); } catch (_) {}
      renderTask = null;
    }

    try {
      const pdfPage = await pdfJsDoc.getPage(activePageObj.originalIndex + 1);
      const rotation = pdfPage.rotate % 360;
      const baseViewport = pdfPage.getViewport({ scale: 1.0, rotation });

      const dpr = window.devicePixelRatio || 1;
      const containerWidth = Math.min(340, (paperSheet && paperSheet.clientWidth) || 340);
      const scale = (containerWidth / baseViewport.width) * dpr;
      const viewport = pdfPage.getViewport({ scale, rotation });

      livePreviewCanvas.width = viewport.width;
      livePreviewCanvas.height = viewport.height;
      livePreviewCanvas.style.width = '100%';
      livePreviewCanvas.style.height = 'auto';

      const ctx = livePreviewCanvas.getContext('2d');
      renderTask = pdfPage.render({ canvasContext: ctx, viewport });
      await renderTask.promise;
      renderTask = null;
    } catch (err) {
      if (err && err.name !== 'RenderingCancelledException') {
        console.warn('Live sheet render error:', err);
      }
    } finally {
      if (sheetLoadingSpinner) sheetLoadingSpinner.classList.add('hidden');
    }
  }

  // Stepper Event Listeners
  if (btnSheetPrev) {
    btnSheetPrev.addEventListener('click', () => {
      const printablePages = pageList.filter(p => p.selected);
      const selIdx = printablePages.findIndex(p => p.originalIndex === activePageIndex);
      if (selIdx > 0) {
        activePageIndex = printablePages[selIdx - 1].originalIndex;
        renderLiveSheet();
      }
    });
  }

  if (btnSheetNext) {
    btnSheetNext.addEventListener('click', () => {
      const printablePages = pageList.filter(p => p.selected);
      const selIdx = printablePages.findIndex(p => p.originalIndex === activePageIndex);
      if (selIdx < printablePages.length - 1) {
        activePageIndex = printablePages[selIdx + 1].originalIndex;
        renderLiveSheet();
      }
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

    const pdfName = file.name.replace(/\.[^/.]+$/, "") + ".pdf";

    if (!hasEdits) {
      if (convertedPdfBytes) {
        return new File([convertedPdfBytes], pdfName, { type: 'application/pdf' });
      }
      return file;
    }

    // Extract selected pages and rotations using PDFLib
    try {
      const sourceBuffer = convertedPdfBytes ? convertedPdfBytes : await file.arrayBuffer();
      const srcDoc = await window.PDFLib.PDFDocument.load(sourceBuffer);
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
      return new File([outBytes], pdfName, { type: 'application/pdf' });
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
      formData.append('color_mode', mode === 'Color' ? 'COLOR' : 'BW');

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
