/**
 * XeroxGO Admin Dashboard Controller
 * Synchronized promotional ad manager for mobile portal and kiosk screens.
 */

(function () {
  'use strict';

  // --- Immutable System Default Cards ---
  const SYSTEM_DEFAULT_CARDS = [
    {
      id: 'ad-quick-easy',
      title: 'Quick & Easy',
      description: 'Direct UPI payment. No coins, no cash needed.',
      type: 'gradient',
      gradient_c0: '#1E3A8A',
      gradient_c1: '#2563EB',
      gradient_c2: '#4F46E5'
    },
    {
      id: 'ad-high-quality',
      title: 'High Quality Prints',
      description: 'Crisp laser documents ready in seconds.',
      type: 'gradient',
      gradient_c0: '#065F46',
      gradient_c1: '#10B981',
      gradient_c2: '#059669'
    },
    {
      id: 'ad-skip-queue',
      title: 'Skip The Queue',
      description: 'Pick up your print job with your 4-digit code.',
      type: 'gradient',
      gradient_c0: '#581C87',
      gradient_c1: '#7C3AED',
      gradient_c2: '#D97706'
    }
  ];

  // --- Safe Response Parsing Helper ---
  async function parseResponseJSON(res) {
    const text = await res.text();
    try {
      return JSON.parse(text);
    } catch (_) {
      return { error: text || `HTTP ${res.status}: ${res.statusText}` };
    }
  }

  // --- API Request Helper ---
  async function apiRequest(endpoint, options = {}) {
    options.headers = options.headers || {};
    const token = localStorage.getItem('xeroxgo_admin_token');
    if (token) {
      options.headers['Authorization'] = 'Bearer ' + token;
    }
    if (options.body && typeof options.body === 'object' && !(options.body instanceof FormData)) {
      options.headers['Content-Type'] = 'application/json';
      options.body = JSON.stringify(options.body);
    }

    const res = await fetch(endpoint, options);
    if (res.status === 401) {
      localStorage.removeItem('xeroxgo_admin_token');
      window.location.reload();
      throw new Error('Unauthorized');
    }
    return res;
  }

  // --- Auth Flow & Session Check ---
  async function checkSession() {
    const authPage = document.getElementById('auth-page');
    const loginForm = document.getElementById('login-form');
    const loginError = document.getElementById('login-error');
    const loginUsername = document.getElementById('login-username');
    const loginPassword = document.getElementById('login-password');
    const loginSubmitBtn = document.getElementById('login-submit-btn');

    // 1. Check if existing session or token is valid
    try {
      const res = await fetch('/api/admin/me', {
        headers: {
          'Authorization': 'Bearer ' + (localStorage.getItem('xeroxgo_admin_token') || '')
        }
      });
      if (res.ok) {
        const data = await parseResponseJSON(res);
        mountDashboard(data);
        return;
      }
    } catch (_) {
      // Unauthenticated, continue to show login view
    }

    // 2. Setup login form handlers if unauthenticated
    if (loginForm) {
      loginForm.addEventListener('submit', async (e) => {
        e.preventDefault();
        loginSubmitBtn.disabled = true;
        loginSubmitBtn.textContent = 'Verifying...';
        loginError.classList.add('hidden');

        try {
          const res = await fetch('/api/admin/login', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              username: loginUsername.value.trim(),
              password: loginPassword.value
            })
          });

          const data = await parseResponseJSON(res);
          if (!res.ok) {
            throw new Error(data.error || 'Authentication failed');
          }

          if (data.token) {
            localStorage.setItem('xeroxgo_admin_token', data.token);
          }

          mountDashboard(data);
        } catch (err) {
          loginError.textContent = err.message || 'Login failed';
          loginError.classList.remove('hidden');
        } finally {
          loginSubmitBtn.disabled = false;
          loginSubmitBtn.textContent = 'Sign In';
        }
      });
    }
  }

  // --- Mount Dashboard ---
  function mountDashboard(userData) {
    // Remove login page entirely from DOM
    const authPage = document.getElementById('auth-page');
    if (authPage) authPage.remove();

    // Clone dashboard template into DOM
    const tpl = document.getElementById('dashboard-template');
    if (!tpl) return;
    document.body.appendChild(tpl.content.cloneNode(true));

    initDashboard(userData);
  }

  // --- Dashboard Logic & State ---
  function initDashboard(userData) {
    let ads = [];
    let currentPreviewIdx = 0;
    let previewTimer = null;
    let activeTab = 'mobile';
    let isEditingInModal = false;

    // DOM Elements
    const logoutBtn = document.getElementById('logout-btn');
    const userNameEl = document.getElementById('user-name');

    const adListEl = document.getElementById('ad-list');
    const createAdBtn = document.getElementById('create-ad-btn');

    // Preview elements
    const tabMobileBtn = document.getElementById('tab-mobile');
    const tabKioskBtn = document.getElementById('tab-kiosk');
    const mobilePreviewContainer = document.getElementById('mobile-preview-container');
    const kioskPreviewContainer = document.getElementById('kiosk-preview-container');

    const mobileSlidePreview = document.getElementById('mobile-slide-preview');
    const mobilePreviewTitle = document.getElementById('mobile-preview-title');
    const mobilePreviewDesc = document.getElementById('mobile-preview-desc');
    const mobilePreviewDots = document.getElementById('mobile-preview-dots');

    const kioskSlidePreview = document.getElementById('kiosk-slide-preview');
    const kioskPreviewTitle = document.getElementById('kiosk-preview-title');
    const kioskPreviewDesc = document.getElementById('kiosk-preview-desc');

    // Modal elements
    const adModal = document.getElementById('ad-modal');
    const modalHeading = document.getElementById('modal-heading');
    const modalCloseBtn = document.getElementById('modal-close-btn');
    const modalCancelBtn = document.getElementById('modal-cancel-btn');
    const adForm = document.getElementById('ad-form');
    const adIdInput = document.getElementById('ad-id');
    const adTitleInput = document.getElementById('ad-title');
    const adDescInput = document.getElementById('ad-desc');
    const adLinkInput = document.getElementById('ad-link');
    const adIsActiveInput = document.getElementById('ad-is-active');

    const gradientControlsGroup = document.getElementById('gradient-controls-group');
    const imageControlsGroup = document.getElementById('image-controls-group');

    const adC0 = document.getElementById('ad-c0');
    const adC0Hex = document.getElementById('ad-c0-hex');
    const adC1 = document.getElementById('ad-c1');
    const adC1Hex = document.getElementById('ad-c1-hex');
    const adC2 = document.getElementById('ad-c2');
    const adC2Hex = document.getElementById('ad-c2-hex');

    const adImageFile = document.getElementById('ad-image-file');
    const uploadImageBtn = document.getElementById('upload-image-btn');
    const imagePreviewThumb = document.getElementById('image-preview-thumb');
    const adImageUrlInput = document.getElementById('ad-image-url');

    const toastEl = document.getElementById('toast');
    const toastMessageEl = document.getElementById('toast-message');

    // User display
    if (userNameEl && userData && userData.username) {
      userNameEl.textContent = userData.username;
    }

    // Toast helper
    function showToast(message) {
      if (!toastEl) return;
      toastMessageEl.textContent = message;
      toastEl.classList.remove('hidden');
      clearTimeout(toastEl._timer);
      toastEl._timer = setTimeout(() => {
        toastEl.classList.add('hidden');
      }, 3500);
    }

    // Sign out
    logoutBtn.addEventListener('click', async () => {
      try {
        await apiRequest('/api/admin/logout', { method: 'POST' });
      } catch (_) {}
      localStorage.removeItem('xeroxgo_admin_token');
      window.location.reload();
    });

    // Load ads
    async function loadAds() {
      try {
        const res = await apiRequest('/api/admin/ads');
        if (!res.ok) throw new Error('Failed to load ads');
        const data = await parseResponseJSON(res);
        ads = Array.isArray(data) ? data : (data.ads || []);
        renderAdList();
        initPreviewCarousel();
      } catch (err) {
        adListEl.innerHTML = `
          <div style="text-align:center; padding:2rem; color:var(--danger);">
            Error loading ads: ${escapeHTML(err.message)}
            <br><button class="btn-secondary" style="margin-top:1rem;" onclick="location.reload()">Retry</button>
          </div>`;
      }
    }

    function renderAdList() {
      if (!ads || ads.length === 0) {
        adListEl.innerHTML = `
          <div class="ad-empty-box">
            <p style="font-size:0.95rem; font-weight:600; color:var(--text-main); margin-bottom:1rem;">No ads configured</p>
            <button class="btn-primary" type="button" onclick="document.getElementById('create-ad-btn').click()" style="margin:0 auto;">
              + New Ad
            </button>
          </div>`;
        return;
      }

      adListEl.innerHTML = ads.map((ad, idx) => {
        const isGradient = ad.type === 'gradient' || !ad.type;
        const bgStyle = isGradient
          ? `background: linear-gradient(135deg, ${ad.gradient_c0 || '#1E3A8A'}, ${ad.gradient_c1 || '#2563EB'}, ${ad.gradient_c2 || '#4F46E5'});`
          : `background: url('${escapeHTML(ad.image_url)}') center/cover no-repeat;`;

        return `
          <div class="ad-card-item ${ad.is_active ? '' : 'inactive'}" data-id="${ad.id}" data-index="${idx}">
            <div class="ad-drag-handle" title="Reorder">
              <div style="display:flex; flex-direction:column; gap:2px;">
                <button class="btn-move-up btn-icon" style="width:24px; height:20px; font-size:10px; border:none;" data-index="${idx}" ${idx === 0 ? 'disabled style="opacity:0.2"' : ''}>▲</button>
                <button class="btn-move-down btn-icon" style="width:24px; height:20px; font-size:10px; border:none;" data-index="${idx}" ${idx === ads.length - 1 ? 'disabled style="opacity:0.2"' : ''}>▼</button>
              </div>
            </div>

            <div class="ad-swatch-thumb" style="${bgStyle}">
              ${isGradient ? '' : '<span style="background:rgba(0,0,0,0.4); padding:2px 6px; border-radius:4px; font-size:9px;">IMG</span>'}
            </div>

            <div class="ad-info">
              <div class="ad-info-title">${escapeHTML(ad.title)}</div>
              <div class="ad-info-desc">${escapeHTML(ad.description)}</div>
              <div class="ad-badges">
                <span class="badge-tag ${isGradient ? 'badge-gradient' : 'badge-image'}">${isGradient ? 'Gradient' : 'Image'}</span>
                ${ad.link_url ? `<span class="badge-tag" style="background:#F1F5F9; color:#475569;" title="${escapeHTML(ad.link_url)}">Link</span>` : ''}
                ${ad.is_active ? '<span class="badge-tag" style="background:#ECFDF5; color:#065F46;">Active</span>' : '<span class="badge-tag" style="background:#FEF2F2; color:#991B1B;">Disabled</span>'}
              </div>
            </div>

            <div class="ad-controls">
              <label class="switch" title="Toggle active status">
                <input type="checkbox" class="toggle-active" data-id="${ad.id}" ${ad.is_active ? 'checked' : ''} />
                <span class="slider"></span>
              </label>
              <button class="btn-icon btn-edit" title="Edit" data-id="${ad.id}">
                <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/></svg>
              </button>
              <button class="btn-icon delete btn-delete" title="Delete" data-id="${ad.id}">
                <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>
              </button>
            </div>
          </div>
        `;
      }).join('');

      // Active toggles
      adListEl.querySelectorAll('.toggle-active').forEach(chk => {
        chk.addEventListener('change', async (e) => {
          const id = e.target.dataset.id;
          const active = e.target.checked;
          await updateAdActiveState(id, active);
        });
      });

      // Edit buttons
      adListEl.querySelectorAll('.btn-edit').forEach(btn => {
        btn.addEventListener('click', () => {
          const id = btn.dataset.id;
          const ad = ads.find(a => a.id === id);
          if (ad) openEditModal(ad);
        });
      });

      // Delete buttons
      adListEl.querySelectorAll('.btn-delete').forEach(btn => {
        btn.addEventListener('click', async () => {
          const id = btn.dataset.id;
          const ad = ads.find(a => a.id === id);
          if (ad && confirm(`Are you sure you want to delete "${ad.title}"?`)) {
            await deleteAd(id);
          }
        });
      });

      // Move Up
      adListEl.querySelectorAll('.btn-move-up').forEach(btn => {
        btn.addEventListener('click', async () => {
          const idx = parseInt(btn.dataset.index, 10);
          if (idx > 0) {
            const temp = ads[idx];
            ads[idx] = ads[idx - 1];
            ads[idx - 1] = temp;
            await saveReorder();
          }
        });
      });

      // Move Down
      adListEl.querySelectorAll('.btn-move-down').forEach(btn => {
        btn.addEventListener('click', async () => {
          const idx = parseInt(btn.dataset.index, 10);
          if (idx < ads.length - 1) {
            const temp = ads[idx];
            ads[idx] = ads[idx + 1];
            ads[idx + 1] = temp;
            await saveReorder();
          }
        });
      });
    }

    async function updateAdActiveState(id, isActive) {
      try {
        const ad = ads.find(a => a.id === id);
        if (!ad) return;
        ad.is_active = isActive;

        const res = await apiRequest(`/api/admin/ads/${encodeURIComponent(id)}`, {
          method: 'PUT',
          body: ad
        });

        if (!res.ok) throw new Error('Failed to update status');
        showToast('Ad updated');
        renderAdList();
        initPreviewCarousel();
      } catch (err) {
        showToast('Error: ' + err.message);
        await loadAds();
      }
    }

    async function deleteAd(id) {
      try {
        const res = await apiRequest(`/api/admin/ads/${encodeURIComponent(id)}`, {
          method: 'DELETE'
        });
        if (!res.ok) throw new Error('Failed to delete ad');
        showToast('Ad deleted');
        await loadAds();
      } catch (err) {
        showToast('Error: ' + err.message);
      }
    }

    async function saveReorder() {
      try {
        const order = ads.map(a => a.id);
        const res = await apiRequest('/api/admin/ads/reorder', {
          method: 'POST',
          body: { order }
        });
        if (!res.ok) throw new Error('Failed to save order');
        showToast('Order saved');
        renderAdList();
        initPreviewCarousel();
      } catch (err) {
        showToast('Error: ' + err.message);
        await loadAds();
      }
    }

    // --- Preview Simulator ---
    tabMobileBtn.addEventListener('click', () => {
      activeTab = 'mobile';
      tabMobileBtn.classList.add('active');
      tabKioskBtn.classList.remove('active');
      mobilePreviewContainer.classList.remove('hidden');
      kioskPreviewContainer.classList.add('hidden');
    });

    tabKioskBtn.addEventListener('click', () => {
      activeTab = 'kiosk';
      tabKioskBtn.classList.add('active');
      tabMobileBtn.classList.remove('active');
      kioskPreviewContainer.classList.remove('hidden');
      mobilePreviewContainer.classList.add('hidden');
    });

    function initPreviewCarousel() {
      if (previewTimer) clearInterval(previewTimer);
      const activeCustomAds = ads.filter(a => a.is_active);
      const displayAds = activeCustomAds.length > 0 ? activeCustomAds : SYSTEM_DEFAULT_CARDS;

      currentPreviewIdx = 0;
      renderPreviewSlide(displayAds[currentPreviewIdx]);
      renderDots(displayAds.length, currentPreviewIdx, displayAds);

      if (displayAds.length > 1) {
        previewTimer = setInterval(() => {
          if (isEditingInModal) return; // Freeze while editing
          currentPreviewIdx = (currentPreviewIdx + 1) % displayAds.length;
          renderPreviewSlide(displayAds[currentPreviewIdx]);
          renderDots(displayAds.length, currentPreviewIdx, displayAds);
        }, 4000);
      }
    }

    function renderDots(count, activeIdx, displayAdsList) {
      mobilePreviewDots.innerHTML = Array.from({ length: count }, (_, i) => `
        <span class="preview-dot ${i === activeIdx ? 'active' : ''}" data-idx="${i}"></span>
      `).join('');

      mobilePreviewDots.querySelectorAll('.preview-dot').forEach(dot => {
        dot.addEventListener('click', () => {
          const idx = parseInt(dot.dataset.idx, 10);
          const list = displayAdsList || (ads.filter(a => a.is_active).length > 0 ? ads.filter(a => a.is_active) : SYSTEM_DEFAULT_CARDS);
          if (list[idx]) {
            currentPreviewIdx = idx;
            renderPreviewSlide(list[idx]);
            renderDots(list.length, idx, list);
          }
        });
      });
    }

    function renderPreviewSlide(ad) {
      if (!ad) return;
      const isGradient = ad.type === 'gradient' || !ad.type;
      const bgStyle = isGradient
        ? `linear-gradient(135deg, ${ad.gradient_c0 || '#1E3A8A'}, ${ad.gradient_c1 || '#2563EB'}, ${ad.gradient_c2 || '#4F46E5'})`
        : `url('${ad.image_url}') center/cover no-repeat`;

      mobileSlidePreview.style.background = bgStyle;
      mobilePreviewTitle.textContent = ad.title || '';
      mobilePreviewDesc.textContent = ad.description || '';

      kioskSlidePreview.style.background = bgStyle;
      kioskPreviewTitle.textContent = ad.title || '';
      kioskPreviewDesc.textContent = ad.description || '';
    }

    // --- Modal Form & Live Binding ---
    function openCreateModal() {
      isEditingInModal = true;
      modalHeading.textContent = 'New Ad';
      adIdInput.value = '';
      adTitleInput.value = '';
      adDescInput.value = '';
      adLinkInput.value = '';
      adIsActiveInput.checked = true;

      setGradientColors('#1E3A8A', '#2563EB', '#4F46E5');
      setCardType('gradient');
      imagePreviewThumb.style.backgroundImage = '';
      imagePreviewThumb.textContent = 'No file';
      adImageUrlInput.value = '';

      syncFormToLivePreview();
      adModal.classList.remove('hidden');
      adTitleInput.focus();
    }

    function openEditModal(ad) {
      isEditingInModal = true;
      modalHeading.textContent = 'Edit Ad';
      adIdInput.value = ad.id;
      adTitleInput.value = ad.title;
      adDescInput.value = ad.description;
      adLinkInput.value = ad.link_url || '';
      adIsActiveInput.checked = ad.is_active;

      setGradientColors(ad.gradient_c0 || '#1E3A8A', ad.gradient_c1 || '#2563EB', ad.gradient_c2 || '#4F46E5');
      setCardType(ad.type || 'gradient');

      if (ad.image_url) {
        adImageUrlInput.value = ad.image_url;
        imagePreviewThumb.style.backgroundImage = `url('${ad.image_url}')`;
        imagePreviewThumb.textContent = '';
      } else {
        adImageUrlInput.value = '';
        imagePreviewThumb.style.backgroundImage = '';
        imagePreviewThumb.textContent = 'No file';
      }

      syncFormToLivePreview();
      adModal.classList.remove('hidden');
      adTitleInput.focus();
    }

    function closeModal() {
      isEditingInModal = false;
      adModal.classList.add('hidden');
      initPreviewCarousel();
    }

    createAdBtn.addEventListener('click', openCreateModal);
    modalCloseBtn.addEventListener('click', closeModal);
    modalCancelBtn.addEventListener('click', closeModal);

    function setCardType(type) {
      const radios = document.getElementsByName('ad-type');
      for (const r of radios) {
        r.checked = r.value === type;
      }
      if (type === 'image') {
        gradientControlsGroup.classList.add('hidden');
        imageControlsGroup.classList.remove('hidden');
      } else {
        gradientControlsGroup.classList.remove('hidden');
        imageControlsGroup.classList.add('hidden');
      }
      syncFormToLivePreview();
    }

    document.getElementsByName('ad-type').forEach(radio => {
      radio.addEventListener('change', (e) => {
        setCardType(e.target.value);
      });
    });

    function setGradientColors(c0, c1, c2) {
      adC0.value = c0;
      adC0Hex.value = c0.toUpperCase();
      adC1.value = c1;
      adC1Hex.value = c1.toUpperCase();
      adC2.value = c2;
      adC2Hex.value = c2.toUpperCase();
      syncFormToLivePreview();
    }

    function bindColorPair(picker, textInput) {
      picker.addEventListener('input', () => {
        textInput.value = picker.value.toUpperCase();
        syncFormToLivePreview();
      });
      textInput.addEventListener('input', () => {
        if (/^#[0-9A-Fa-f]{6}$/.test(textInput.value)) {
          picker.value = textInput.value;
          syncFormToLivePreview();
        }
      });
    }

    bindColorPair(adC0, adC0Hex);
    bindColorPair(adC1, adC1Hex);
    bindColorPair(adC2, adC2Hex);

    document.querySelectorAll('.palette-btn').forEach(btn => {
      btn.addEventListener('click', () => {
        setGradientColors(btn.dataset.c0, btn.dataset.c1, btn.dataset.c2);
      });
    });

    adTitleInput.addEventListener('input', syncFormToLivePreview);
    adDescInput.addEventListener('input', syncFormToLivePreview);

    function syncFormToLivePreview() {
      const type = document.querySelector('input[name="ad-type"]:checked')?.value || 'gradient';
      const formAd = {
        title: adTitleInput.value || 'Card Headline',
        description: adDescInput.value || 'Card description text...',
        type: type,
        gradient_c0: adC0.value,
        gradient_c1: adC1.value,
        gradient_c2: adC2.value,
        image_url: adImageUrlInput.value
      };
      renderPreviewSlide(formAd);
    }

    // Banner Image Upload
    uploadImageBtn.addEventListener('click', () => {
      adImageFile.click();
    });

    adImageFile.addEventListener('change', async (e) => {
      const file = e.target.files[0];
      if (!file) return;

      if (file.size > 5 * 1024 * 1024) {
        alert('Image file must be under 5MB');
        return;
      }

      uploadImageBtn.disabled = true;
      uploadImageBtn.textContent = 'Uploading...';

      try {
        const formData = new FormData();
        formData.append('image', file);

        const res = await apiRequest('/api/admin/upload-image', {
          method: 'POST',
          body: formData
        });

        const data = await parseResponseJSON(res);
        if (!res.ok) throw new Error(data.error || 'Upload failed');

        adImageUrlInput.value = data.image_url;
        imagePreviewThumb.style.backgroundImage = `url('${data.image_url}')`;
        imagePreviewThumb.textContent = '';
        syncFormToLivePreview();
        showToast('Image uploaded');
      } catch (err) {
        alert('Upload error: ' + err.message);
      } finally {
        uploadImageBtn.disabled = false;
        uploadImageBtn.textContent = 'Choose Graphic...';
      }
    });

    // Form submit
    adForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      const saveBtn = document.getElementById('modal-save-btn');
      saveBtn.disabled = true;
      saveBtn.textContent = 'Saving...';

      const id = adIdInput.value;
      const type = document.querySelector('input[name="ad-type"]:checked')?.value || 'gradient';

      const payload = {
        title: adTitleInput.value.trim(),
        description: adDescInput.value.trim(),
        type: type,
        image_url: type === 'image' ? adImageUrlInput.value : '',
        gradient_c0: adC0.value,
        gradient_c1: adC1.value,
        gradient_c2: adC2.value,
        link_url: adLinkInput.value.trim(),
        is_active: adIsActiveInput.checked
      };

      try {
        let res;
        if (id) {
          payload.id = id;
          res = await apiRequest(`/api/admin/ads/${encodeURIComponent(id)}`, {
            method: 'PUT',
            body: payload
          });
        } else {
          res = await apiRequest('/api/admin/ads', {
            method: 'POST',
            body: payload
          });
        }

        const data = await parseResponseJSON(res);
        if (!res.ok) {
          throw new Error(data.error || 'Failed to save ad');
        }

        closeModal();
        showToast('Ad saved');
        await loadAds();
      } catch (err) {
        alert('Error saving ad: ' + err.message);
      } finally {
        saveBtn.disabled = false;
        saveBtn.textContent = 'Save';
      }
    });

    // Initial load
    loadAds();
  }

  function escapeHTML(str) {
    if (!str) return '';
    return str
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#039;');
  }

  // Initial session check
  document.addEventListener('DOMContentLoaded', checkSession);

})();
