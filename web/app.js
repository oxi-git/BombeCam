/**
 * BombeCam web page controller
 *
 * Implements:
 * 1. Operator Auth & CSRF token management (X-CSRF-Token).
 * 2. Single Unified Vendor Account Flow (Setup, Saved session reuse, Renewal, Correction, Switching).
 * 3. Camera Selection & Stream Export (Discovery, Selective enrollment, RTSP/HLS 1-click copy, NVR YAML).
 * 4. Auxiliary Camera Controls & Anti-Optimistic Truthful Telemetry (PTZ d-pad, IR, LED, Light, Talkback, Shadow readback).
 * 5. Decoded-Frame Gating (frame callbacks or advancing decoded-frame counters).
 * 6. Block cloud video: "Block all cameras" plus a switch per camera; the
 *    gateway applies it on the cameras' router over SSH (see How it works).
 * 7. STOP / START cameras (never changes the router).
 */

'use strict';

// Global SPA State
const state = {
  csrfToken: '',
  onboardingStatus: null,
  profile: null,
  sessionStatus: null,
  privacy: null,          // GET /api/v1/privacy
  privacyBusy: '',        // '' | 'all' | camera id | 'router' while a change is sent

  discoveredCameras: [],
  enrolledCameras: [],
  selectedCameraId: '',
  selectedDiscoveryIds: new Set(),
  streamDescriptors: {}, // camID -> { rtsp_url, hls_url, streaming }
  shadowTelemetry: {},  // camID -> { IrLedMode, LedOnOff, LightSW }
  currentZoom: 1.0,
  isTalkActive: false,
  whepPeerConnection: null,
  talkStream: null,
  talkEncoder: null,
  talkAudioCtx: null,
  talkProcessor: null,
  talkInterval: null,
  updatingControls: new Set(), // Set of action names currently updating
  lastVideoFrameTime: 0,
  lastDecodedFrameAdvance: 0,
  activeTab: 'tab-dashboard',
  soundOn: false,
  volume: 0.8,
  talkSeq: 0,
  talkStarting: false,
  camerasStopped: false,
  lastSessionStatus: '',
  accounts: null,        // GET /api/v1/accounts: the Osaio login pool
};

// Live video player state (see section 6)
const viewer = {
  camId: '',
  mode: 'none',        // 'none' | 'webrtc' | 'hls'
  attaching: false,
  generation: 0,       // bumped on every teardown to cancel stale async work
  webrtcFailures: 0,
  preferHLS: false,    // set when sound is wanted and the WebRTC copy has none
  videoOnly: false,    // the WebRTC view asked for the video-only copy (sound off)
  onCopy: false,       // the gateway served that copy (X-BombeCam-Stream)
  tickBusy: false,
};


// ============================================================================
// Auto Country & Timezone Detection Routines (R3) & Onboarding Screen State
// ============================================================================

function detectTimezone() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  } catch (_) {
    return 'UTC';
  }
}

function detectTimezoneOffset() {
  try {
    const offset = -new Date().getTimezoneOffset() / 60;
    return isNaN(offset) ? 0 : offset;
  } catch (_) {
    return 0;
  }
}

function inferCountryCode() {
  const countryDialCodes = {
    'US': '1', 'CA': '1',
    'GB': '44', 'UK': '44',
    'FR': '33', 'DE': '49',
    'ES': '34', 'IT': '39',
    'JP': '81', 'CN': '86',
    'AU': '61'
  };

  const list = [];
  if (typeof navigator !== 'undefined') {
    if (Array.isArray(navigator.languages) && navigator.languages.length > 0) {
      list.push(...navigator.languages);
    } else if (navigator.language) {
      list.push(navigator.language);
    }
  }

  for (const item of list) {
    if (!item || typeof item !== 'string') continue;
    try {
      const region = new Intl.Locale(item.replace(/_/g, '-')).region;
      if (countryDialCodes[region]) return countryDialCodes[region];
    } catch (_) {}
  }
  return ''; // language-only and unknown regions need explicit account input

}

function normalizedCountryInput(value) {
  const digits = String(value || '').trim().replace(/^\+/, '');
  return /^[1-9][0-9]{0,2}$/.test(digits) ? digits : '';
}

function suggestedAccountCountry(email) {
  const normalized = String(email || '').trim().toLowerCase();
  const prof = state.profile || {};
  const account = (prof.accounts || []).find(a => String(a.account_email || '').toLowerCase() === normalized);
  if (account && normalizedCountryInput(account.country)) return normalizedCountryInput(account.country);
  return inferCountryCode();
}

function setupCountryFields() {
  for (const [countryId, emailId] of [['input-country', 'input-email'], ['add-cam-country', 'add-cam-email'], ['login-password-country', 'login-password-email']]) {
    const input = document.getElementById(countryId);
    const email = document.getElementById(emailId);
    if (!input) continue;
    if (!input.value) input.value = suggestedAccountCountry(email && email.value);
    input.addEventListener('input', () => { input.dataset.countryEdited = 'true'; });
    if (email) email.addEventListener('blur', () => {
      if (!input.dataset.countryEdited) input.value = suggestedAccountCountry(email.value);
    });
  }
}

function showOnboardingScreen(screenId) {
  const screens = ['screen-login', 'screen-loading', 'screen-discovery', 'screen-admin-create', 'screen-admin-signin', 'screen-admin-elsewhere'];
  screens.forEach(id => {
    const el = document.getElementById(id);
    if (el) el.style.display = (id === screenId) ? 'block' : 'none';
  });
}

function switchToLoginScreen() {
  const viewOnboarding = document.getElementById('view-onboarding');
  const viewWorkspace = document.getElementById('view-workspace');
  const navTabs = document.getElementById('main-nav-tabs');

  teardownVideo();

  if (navTabs) navTabs.style.display = 'none';
  if (viewWorkspace) viewWorkspace.style.display = 'none';
  if (viewOnboarding) viewOnboarding.style.display = 'flex';
  showOnboardingScreen('screen-login');
  const cancelBtn = document.getElementById('btn-login-cancel');
  const signedIn = state.onboardingStatus && state.onboardingStatus.has_profile &&
    ['authenticated', 'authenticating'].includes(state.lastSessionStatus || state.onboardingStatus.session_status);
  if (cancelBtn) cancelBtn.style.display = signedIn ? 'block' : 'none';
}

function setLoadingState(title, desc) {
  const t = document.getElementById('loading-title');
  const d = document.getElementById('loading-desc');
  if (t) t.textContent = title;
  if (d) d.textContent = desc;
}

// ============================================================================
// 1. Operator Auth & CSRF Token Management
// ============================================================================

/**
 * Fetches fresh CSRF token from the gateway operator endpoint.
 */
async function fetchCSRFToken() {
  try {
    const res = await fetch('/api/v1/auth/csrf', { credentials: 'same-origin' });
    if (res.ok) {
      const data = await res.json();
      if (data && data.csrf_token) {
        state.csrfToken = data.csrf_token;
        return state.csrfToken;
      }
    }
  } catch (err) {
    console.warn('[auth] /api/v1/auth/csrf failed, trying fallback:', err);
  }

  try {
    const res = await fetch('/api/v1/operator/csrf', { credentials: 'same-origin' });
    if (res.ok) {
      const data = await res.json();
      if (data && data.csrf_token) {
        state.csrfToken = data.csrf_token;
        return state.csrfToken;
      }
    }
  } catch (err) {
    console.error('[auth] failed to obtain CSRF token:', err);
  }
  return '';
}

/**
 * Robust fetch wrapper that applies CSRF token and credentials.
 */
async function apiRequest(endpoint, method = 'GET', body = null, options = {}) {
  const isMutation = ['POST', 'PUT', 'DELETE', 'PATCH'].includes(method.toUpperCase());

  if (isMutation && !state.csrfToken) {
    await fetchCSRFToken();
  }

  const headers = { 'Accept': 'application/json' };
  let reqBody = null;
  if (body) {
    if (body instanceof Blob || body instanceof ArrayBuffer || ArrayBuffer.isView(body) || (typeof FormData !== 'undefined' && body instanceof FormData)) {
      reqBody = body;
    } else {
      headers['Content-Type'] = 'application/json';
      reqBody = JSON.stringify(body);
    }
  }
  if (isMutation && state.csrfToken) {
    headers['X-CSRF-Token'] = state.csrfToken;
  }

  let res = await fetch(endpoint, {
    method: method,
    headers: headers,
    credentials: 'same-origin',
    signal: options.signal,
    body: reqBody
  });

  // Auto-recovery if CSRF token expired, cookie rotated, or session refreshed
  if ((res.status === 403 || res.status === 401) && isMutation) {
    let errJson = {};
    try {
      errJson = await res.clone().json();
    } catch (_) {}

    if (errJson.error === 'invalid_csrf_token' || errJson.error === 'unauthorized') {
      console.warn('[auth] Session or CSRF token refreshed, retrying mutation...');
      await fetchCSRFToken();
      if (state.csrfToken) {
        headers['X-CSRF-Token'] = state.csrfToken;
        res = await fetch(endpoint, {
          method: method,
          headers: headers,
          credentials: 'same-origin',
          signal: options.signal,
          body: reqBody
        });
      }
    }
  }

  // Signed out (session expired, password changed elsewhere): back to sign-in.
  if (res.status === 401 && !endpoint.startsWith('/api/v1/admin/')) {
    let errJson = {};
    try { errJson = await res.clone().json(); } catch (_) {}
    if (errJson.error === 'unauthorized') onSignedOut();
  }
  return res;
}

// ============================================================================
// 2. Application Bootstrapping & View Routing
// ============================================================================

// showVersion puts the gateway's version next to the name in the header.
async function showVersion() {
  try {
    const res = await apiRequest('/api/v1/gateway/info');
    const info = res.ok ? await res.json() : null;
    const el = document.getElementById('brand-ver');
    if (el && info && info.version) el.textContent = 'v' + info.version;
  } catch (_) {}
}

async function initApp() {
  setupNavigationTabs();
  setupEventListeners();
  setupCountryFields();

  // Populate local timezone in setup form
  const tzInput = document.getElementById('input-timezone');
  if (tzInput) {
    tzInput.value = detectTimezone();
  }

  // Obtain initial CSRF token
  await fetchCSRFToken();

  // BombeCam's administrator comes first: create it, or sign in.
  if (!(await adminGate())) return;
  showVersion();

  // Initial data loads
  await checkOnboardingStatus();
  await refreshPrivacyStatus();

  // Periodic telemetry polling
  state.timers = [
    setInterval(refreshPrivacyStatus, 15000),
    setInterval(refreshSessionAndProfile, 5000),
    setInterval(decodedFrameWatchdog, 250),
    setInterval(viewerTick, 2000),
  ];
}

function setupNavigationTabs() {
  const tabs = document.querySelectorAll('.nav-tab');
  tabs.forEach(tab => {
    tab.addEventListener('click', () => {
      const targetId = tab.getAttribute('data-tab');
      switchTab(targetId);
    });
  });
}

function switchTab(tabId) {
  state.activeTab = tabId;
  document.querySelectorAll('.nav-tab').forEach(t => {
    t.classList.toggle('active', t.getAttribute('data-tab') === tabId);
  });
  document.querySelectorAll('.tab-content').forEach(c => {
    c.classList.toggle('active', c.id === tabId);
  });

  if (tabId === 'tab-streams') {
    loadNVRSnippets();
  } else if (tabId === 'tab-cameras') {
    renderCamerasTab();
  } else if (tabId === 'tab-logins') {
    refreshAccounts().then(renderLoginsTab);
  } else if (tabId === 'tab-safety') {
    refreshPrivacyStatus();
  }
}

// ============================================================================
// 3. Single Unified Vendor Account Flow
// ============================================================================

async function checkOnboardingStatus() {
  try {
    const res = await apiRequest('/api/v1/onboarding/status');
    if (!res.ok) {
      switchToLoginScreen();
      return;
    }

    const data = await res.json();
    state.onboardingStatus = data;
    renderServerKeyPanel(data.server_key);
    renderAppIdPanel(data.app_id);

    updateSessionBadge(data.session_status);

    const viewOnboarding = document.getElementById('view-onboarding');
    const viewWorkspace = document.getElementById('view-workspace');
    const navTabs = document.getElementById('main-nav-tabs');
    const statusPill = document.getElementById('account-status-pill');

    const isAuthenticated = Boolean(data.has_profile && (data.session_status === 'authenticated' || data.session_status === 'authenticating'));
    // Osaio logins form a pool: once one is stored, the cameras page stays
    // up even if a login needs attention (the Osaio logins tab shows which).
    const hasLogins = Boolean(data.has_profile && Number(data.logins_count || 0) > 0);
    const keyMissing = Boolean(data.server_key && !data.server_key.configured);
    const appIdMissing = Boolean(data.app_id && !data.app_id.configured);

    // State 1: nothing stored yet (or no usable server key or app ID) -> Screen 1 (Login)
    if (!hasLogins || keyMissing || appIdMissing) {
      if (statusPill) {
        statusPill.textContent = (data.session_status === 'invalid_credentials') ? 'Invalid Credentials' : (data.session_status === 'error' || data.session_status === 'unavailable' ? 'Session Error' : 'Unconfigured');
        statusPill.className = (data.session_status === 'invalid_credentials' || data.session_status === 'error' || data.session_status === 'unavailable') ? 'badge badge-error' : 'badge badge-secondary';
      }
      switchToLoginScreen();
      return;
    }

    if (statusPill) {
      statusPill.textContent = 'Configured';
      statusPill.className = 'badge badge-success';
    }

    // State 2: Profile exists, but 0 cameras enrolled -> Screen 2 (Discovery)
    // Never auto-scan for cameras unless a login is actually authenticated.
    // (Prevents a stale/half-written profile from dragging the user back into
    // discovery after "Delete all".) Otherwise the workspace opens empty and
    // Add cameras / Osaio logins take it from there.
    if (data.enrolled_cameras_count === 0 && isAuthenticated && data.ready_for_discovery) {
      if (navTabs) navTabs.style.display = 'none';
      if (viewWorkspace) viewWorkspace.style.display = 'none';
      if (viewOnboarding) viewOnboarding.style.display = 'flex';

      showOnboardingScreen('screen-loading');
      setLoadingState('Scanning Cameras...', 'Finding cameras on your Osaio account...');

      const discRes = await apiRequest('/api/v1/onboarding/cameras/discover?refresh=false');
      if (discRes.ok) {
        const d = await discRes.json();
        state.discoveredCameras = d.cameras || [];
      } else {
        state.discoveredCameras = [];
      }
      renderDiscovery(state.discoveredCameras);
      showOnboardingScreen('screen-discovery');
      return;
    }

    // State 3: Fully onboarded -> Post-Enrollment Workspace
    if (viewOnboarding) viewOnboarding.style.display = 'none';
    if (navTabs) navTabs.style.display = 'flex';
    if (viewWorkspace) viewWorkspace.style.display = 'block';

    await loadEnrolledCameras();
    await refreshSessionAndProfile();
    await refreshPrivacyStatus();
    await loadNVRSnippets();
  } catch (err) {
    console.error('[onboarding] checkOnboardingStatus error:', err);
    switchToLoginScreen();
  }
}

// refreshOnboardingSnapshot re-reads the camera/login counts without
// switching screens (checkOnboardingStatus also routes between screens).
async function refreshOnboardingSnapshot() {
  try {
    const res = await apiRequest('/api/v1/onboarding/status');
    if (!res.ok) return;
    state.onboardingStatus = await res.json();
    renderServerKeyPanel(state.onboardingStatus.server_key);
    renderAppIdPanel(state.onboardingStatus.app_id);
    updateSessionBadge(state.onboardingStatus.session_status);
  } catch (err) {
    console.warn('[onboarding] status refresh failed:', err);
  }
}

async function refreshSessionAndProfile() {
  try {
    const [sessRes, profRes] = await Promise.all([
      apiRequest('/api/v1/session/status'),
      apiRequest('/api/v1/onboarding/profile')
    ]);

    if (sessRes.ok) {
      state.sessionStatus = await sessRes.json();
      updateSessionBadge(state.sessionStatus.status);
    }

    if (profRes.ok) {
      state.profile = await profRes.json();
      renderAccountChip();
    }
    await refreshAccounts();
    if (state.activeTab === 'tab-logins') renderLoginsTab();

    // The saved session may finish restoring after the page loaded (slow
    // network, PC just booted): move to the right screen when it does.
    const onboardingVisible = document.getElementById('view-onboarding')?.style.display !== 'none';
    const workspaceVisible = document.getElementById('view-workspace')?.style.display !== 'none';
    const status = state.sessionStatus ? state.sessionStatus.status : '';
    const loginVisible = document.getElementById('screen-login')?.style.display === 'block';
    if (onboardingVisible && loginVisible && status === 'authenticated' && state.onboardingStatus && state.onboardingStatus.has_profile) {
      await checkOnboardingStatus();
    } else if (workspaceVisible && state.enrolledCameras.length === 0 && status === 'authenticated') {
      await loadEnrolledCameras();
    }
  } catch (err) {
    console.warn('[account] session/profile refresh error:', err);
  }
}

function updateSessionBadge(status) {
  if (status !== undefined) state.lastSessionStatus = status;
  renderAccountChip();
}

// Header chip: how many cameras (and Osaio logins) are set up on this computer,
// and whether BombeCam can reach Osaio with them. Never shows an email.
// A login that needs attention is named on the Osaio logins tab; the chip
// opens it.
function renderAccountChip() {
  const chip = document.getElementById('session-badge');
  const text = document.getElementById('session-badge-text');
  if (!chip || !text) return;
  const status = state.lastSessionStatus || '';
  const st = state.onboardingStatus || {};
  // The live camera list wins: the onboarding snapshot is only refreshed on
  // some paths and was stale right after enrolling the first cameras.
  const cams = Math.max((state.enrolledCameras || []).length, Number(st.enrolled_cameras_count || 0));
  const accounts = state.accounts || null;
  const logins = accounts ? accounts.length : Number(st.logins_count || 0);
  const camText = `${cams} camera${cams === 1 ? '' : 's'}`;
  const withLogins = camText + (logins > 1 ? ` · ${logins} Osaio logins` : '');
  const attention = accounts ? accounts.filter(a => !['connected', 'connecting'].includes(a.status)) : [];
  let cls = 'account-chip';
  let label = '';
  let target = '';
  if (status === 'app_id_missing' || attention.some(a => a.status === 'app_id')) {
    cls += ' warn';
    label = 'Osaio app ID needed (Settings)';
  } else if (status === 'server_key_missing' || attention.some(a => a.status === 'server_key')) {
    cls += ' warn';
    label = 'Osaio server key needed (Settings)';
  } else if (attention.some(a => a.status === 'needs_password')) {
    cls += ' error';
    const n = attention.filter(a => a.status === 'needs_password').length;
    label = n === 1 ? 'An Osaio login needs its password' : `${n} Osaio logins need their password`;
    target = 'tab-logins';
  } else if (attention.length && attention.every(a => a.status === 'unreachable')) {
    cls += ' warn';
    label = cams ? `${camText} · Osaio unreachable, retrying...` : 'Osaio unreachable, retrying...';
  } else if (accounts && accounts.some(a => a.status === 'connecting')) {
    cls += ' busy';
    label = cams ? `${camText} · connecting to Osaio...` : 'Connecting to Osaio...';
  } else if (accounts && accounts.length && !attention.length) {
    cls += ' ok';
    label = cams ? withLogins : 'No cameras added yet';
  } else {
    // Before the login list arrives: the main session's state.
    switch (status) {
      case 'authenticated':
        cls += ' ok';
        label = cams ? withLogins : 'No cameras added yet';
        break;
      case 'authenticating':
        cls += ' busy';
        label = cams ? `${camText} · connecting to Osaio...` : 'Connecting to Osaio...';
        break;
      case 'invalid_credentials':
        cls += ' error';
        label = 'An Osaio login needs its password';
        target = 'tab-logins';
        break;
      case 'network_timeout':
      case 'upstream_unavailable':
      case 'unavailable':
        cls += ' warn';
        label = cams ? `${camText} · Osaio unreachable, retrying...` : 'Osaio unreachable, retrying...';
        break;
      case 'credentials_missing':
        label = cams ? `${camText} · sign-in needed` : 'No cameras added yet';
        target = 'tab-logins';
        break;
      default:
        label = 'Loading cameras...';
    }
  }
  chip.className = cls;
  chip.dataset.target = target || 'tab-cameras';
  chip.title = target === 'tab-logins'
    ? 'Open Osaio logins to see which login needs attention.'
    : 'Cameras added on this computer. BombeCam has no account of its own; Osaio logins are used only to find and reach your cameras.';
  text.textContent = label;
  const alertDot = document.getElementById('nav-logins-alert');
  if (alertDot) alertDot.hidden = !attention.length;
  // Add cameras / Settings only make sense once something is stored.
  const controls = document.getElementById('header-account-controls');
  const hasStored = Boolean(st.has_profile) || status === 'authenticated';
  if (controls) controls.style.display = hasStored ? '' : 'none';
}

// refreshAccounts reads the Osaio login pool and each login's state.
async function refreshAccounts() {
  try {
    const res = await apiRequest('/api/v1/accounts');
    if (!res.ok) return state.accounts;
    const data = await res.json();
    state.accounts = data.accounts || [];
    renderAccountChip();
  } catch (_) {}
  return state.accounts;
}

// Setup Form Submission (POST /api/v1/onboarding/setup)
// Setup Form Submission (POST /api/v1/onboarding/setup)
async function handleSetupSubmit(e) {
  if (e && e.preventDefault) e.preventDefault();

  const emailEl = document.getElementById('input-email');
  const passEl = document.getElementById('input-password');
  const email = emailEl ? emailEl.value.trim() : '';
  const password = passEl ? passEl.value : '';
  const countryEl = document.getElementById('input-country') || document.getElementById('select-country');
  const country = normalizedCountryInput(countryEl ? countryEl.value : inferCountryCode());
  if (!country) { showToast('Enter the country calling code for this Osaio login.', 'warning'); return; }
  const forceEl = document.getElementById('chk-force-overwrite');
  const force = forceEl ? forceEl.checked : true;

  const tz = detectTimezone();
  const tzOffset = detectTimezoneOffset();

  const payload = {
    account_email: email,
    password: password,
    country: country,
    timezone: tz,
    timezone_offset: tzOffset,
    force: force,
  };

  const submitBtn = document.getElementById('btn-submit-setup');
  if (submitBtn) {
    submitBtn.disabled = true;
    submitBtn.textContent = 'Connecting to Osaio Cloud...';
  }

  try {
    const res = await apiRequest('/api/v1/onboarding/setup', 'POST', payload);
    const data = await res.json();

    if (res.ok) {
      if (data.csrf_token) {
        state.csrfToken = data.csrf_token;
      }
      showToast('Account connected successfully!', 'success');
      document.getElementById('form-account-setup')?.reset();
      await checkOnboardingStatus();
      await loadDiscoveredCameras(true);
    } else {
      const errMsg = mapSetupError(res.status, data);
      showToast(errMsg, 'error');
    }
  } catch (err) {
    showToast(`Network error: ${err.message}`, 'error');
  } finally {
    if (submitBtn) {
      submitBtn.disabled = false;
      submitBtn.textContent = 'Connect Account';
    }
  }
}

// Sign-in submission (POST /api/v1/onboarding/setup)
async function handleLoginGo(e) {
  if (e && e.preventDefault) e.preventDefault();

  const emailEl = document.getElementById('input-email');
  const passEl = document.getElementById('input-password');
  const email = emailEl ? emailEl.value.trim() : '';
  const password = passEl ? passEl.value : '';

  if (!email || !password) {
    showToast('Please enter both username/email and password.', 'warning');
    return;
  }

  // Without a key or app ID, save the one typed above first (or ask for it).
  if (!(await ensureOsaioValuesSaved())) return;

  const countryInput = document.getElementById('input-country');
  const country = normalizedCountryInput(countryInput ? countryInput.value : inferCountryCode());
  if (!country) { showToast('Enter the country calling code for this Osaio login.', 'warning'); return; }
  const goBtn = document.getElementById('btn-login-go');
  if (goBtn) goBtn.disabled = true;

  // Timezone is detected independently of the account country.
  const timezone = detectTimezone();
  const timezoneOffset = detectTimezoneOffset();

  // Show loading state
  showOnboardingScreen('screen-loading');
  setLoadingState('Connecting to Osaio...', 'Authenticating credentials and creating profile...');

  try {
    const payload = {
      account_email: email,
      password: password,
      country: country,
      timezone: timezone,
      timezone_offset: timezoneOffset,
      force: true,        // Overwrite initial state cleanly
    };

    const res = await apiRequest('/api/v1/onboarding/setup', 'POST', payload);
    const data = await res.json();

    if (!res.ok) {
      showToast(mapSetupError(res.status, data), 'error');
      showOnboardingScreen('screen-login');
      if (goBtn) goBtn.disabled = false;
      return;
    }

    if (data.csrf_token) {
      state.csrfToken = data.csrf_token;
    }

    // The quick start added this login to the pool; list its cameras.
    setLoadingState('Discovering Cameras...', 'Looking for the cameras this Osaio login can see...');

    const discRes = await apiRequest('/api/v1/accounts/cameras', 'POST', { email: data.account_email || email });
    if (!discRes.ok) {
      const discErr = await discRes.json().catch(() => ({}));
      showToast(discErr.error || 'Camera discovery warning.', 'warning');
      state.discoveredCameras = [];
    } else {
      const discData = await discRes.json();
      state.discoveredCameras = discData.cameras || [];
    }

    renderDiscovery(state.discoveredCameras);
    showOnboardingScreen('screen-discovery');
  } catch (err) {
    showToast(`Connection failed: ${err.message}`, 'error');
    showOnboardingScreen('screen-login');
  } finally {
    if (goBtn) goBtn.disabled = false;
  }
}

// ---------------------------------------------------------------------------
// Osaio server key and app ID. Both work the same way: BombeCam ships the
// latest known value, Settings > Server key / App ID says where the one in
// use comes from (never the value itself) and replaces it at once when Osaio
// changes it, and the sign-in page only asks for one if it can't be used.
// ---------------------------------------------------------------------------

const SERVER_KEY_SOURCES = {
  command_line: 'the -server-key-file option',
  environment: 'the BOMBECAM_SERVER_KEY environment variable',
  env_file: 'the file named in BOMBECAM_SERVER_KEY_FILE',
  key_file: 'the key you saved on this computer',
  built_in: 'the key built into this version of BombeCam',
};

const APP_ID_SOURCES = {
  command_line: 'the -app-id-file option',
  environment: 'the BOMBECAM_APP_ID environment variable',
  env_file: 'the file named in BOMBECAM_APP_ID_FILE',
  key_file: 'the app ID you saved on this computer',
  built_in: 'the app ID built into this version of BombeCam',
};

// Element IDs follow one pattern per value: <id>-panel, input-<id>,
// btn-save-<id>, manage-<id>-hint, modal-<id>, ...
const OSAIO_VALUES = {
  serverKey: { id: 'server-key', api: '/api/v1/server-key', field: 'key', reply: 'server_key', name: 'server key', noun: 'key', title: 'Server key', sources: SERVER_KEY_SOURCES },
  appId: { id: 'app-id', api: '/api/v1/app-id', field: 'app_id', reply: 'app_id', name: 'app ID', noun: 'app ID', title: 'App ID', sources: APP_ID_SOURCES },
};

function osaioPanelVisible(v) {
  const panel = document.getElementById(`${v.id}-panel`);
  return Boolean(panel && panel.style.display !== 'none');
}

function renderOsaioPanel(v, st) {
  const panel = document.getElementById(`${v.id}-panel`);
  const problem = document.getElementById(`${v.id}-problem`);
  const input = document.getElementById(`input-${v.id}`);
  const saveBtn = document.getElementById(`btn-save-${v.id}`);
  const missing = Boolean(st && !st.configured);
  if (panel) panel.style.display = missing ? 'block' : 'none';
  if (problem) {
    problem.textContent = st && st.problem
      ? (st.editable ? st.problem : `${st.problem}. It is set by ${v.sources[st.source] || 'BombeCam\'s start-up settings'}; fix it there and restart BombeCam.`)
      : '';
  }
  const editable = !st || st.editable;
  if (input) input.disabled = !editable;
  if (saveBtn) saveBtn.disabled = !editable;

  const hint = document.getElementById(`manage-${v.id}-hint`);
  const openBtn = document.getElementById(`btn-open-${v.id}`);
  if (hint && st) {
    hint.textContent = st.configured
      ? `In use: ${v.sources[st.source] || 'configured'}.` + (st.editable ? ` Replace it here if Osaio changes its ${v.noun}.` : ' Change it where it is set, then restart BombeCam.')
      : 'Not set yet.';
  }
  if (openBtn) openBtn.disabled = false; // the dialog also says where a fixed value is set
}

// openOsaioModal says where the value in use comes from and takes a new one.
// The value itself is never shown: the server does not send it.
async function openOsaioModal(v) {
  const input = document.getElementById(`modal-${v.id}-input`);
  const status = document.getElementById(`modal-${v.id}-status`);
  const resetBtn = document.getElementById(`btn-reset-${v.id}`);
  const saveBtn = document.getElementById(`btn-submit-${v.id}`);
  // Clear what was typed last time before the dialog shows.
  if (input) input.value = '';
  if (status) status.textContent = '';
  closeModal('modal-manage');
  openModal(`modal-${v.id}`);
  try {
    const res = await apiRequest(v.api);
    const st = res.ok ? await res.json() : {};
    if (input) input.readOnly = !st.editable;
    if (status) {
      status.textContent = (st.configured ? `In use: ${v.sources[st.source] || 'configured'}.` : `No working ${v.noun} yet.`) +
        (st.problem ? ` ${st.problem}.` : '') +
        (st.editable ? '' : ' Change it where it is set, then restart BombeCam.');
    }
    if (resetBtn) resetBtn.style.display = (st.editable && st.has_built_in && st.source !== 'built_in') ? '' : 'none';
    if (saveBtn) saveBtn.disabled = !st.editable;
  } catch (err) {
    if (status) status.textContent = `Could not read the ${v.name} from BombeCam.`;
  }
}

async function resetOsaioValue(v) {
  try {
    const res = await apiRequest(v.api, 'DELETE');
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      showToast((data && data.message) || `Could not switch back to the built-in ${v.noun}.`, 'error');
      return;
    }
    renderOsaioPanel(v, data[v.reply]);
    showToast(`Using the built-in ${v.name}.`, 'success');
    closeModal(`modal-${v.id}`);
    setTimeout(refreshSessionAndProfile, 1500);
  } catch (err) {
    showToast(`Could not switch back to the built-in ${v.noun}: ${err.message}`, 'error');
  }
}

// saveOsaioValue sends a value to the gateway; returns true when it was saved.
async function saveOsaioValue(v, value) {
  try {
    const res = await apiRequest(v.api, 'POST', { [v.field]: value });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      showToast((data && data.message) || `The ${v.name} was not saved.`, 'error');
      return false;
    }
    renderOsaioPanel(v, data[v.reply]);
    showToast(`${v.title} saved.`, 'success');
    return true;
  } catch (err) {
    showToast(`Could not save the ${v.name}: ${err.message}`, 'error');
    return false;
  }
}

async function saveOsaioPanel(v) {
  const input = document.getElementById(`input-${v.id}`);
  if (!input || !input.value.trim()) {
    showToast(`Paste the Osaio ${v.name} first.`, 'warning');
    return;
  }
  if (await saveOsaioValue(v, input.value)) {
    input.value = '';
    // A saved profile reconnects by itself; follow it to the camera view.
    setTimeout(checkOnboardingStatus, 1500);
  }
}

async function submitOsaioModal(v) {
  const input = document.getElementById(`modal-${v.id}-input`);
  if (!input || !input.value.trim()) {
    showToast(`Paste the new ${v.name} first.`, 'warning');
    return;
  }
  if (await saveOsaioValue(v, input.value)) {
    input.value = '';
    closeModal(`modal-${v.id}`);
    setTimeout(refreshSessionAndProfile, 1500);
  }
}

// ensureOsaioValuesSaved saves what the sign-in panels hold before a
// sign-in; false (after saying why) when one is still needed.
async function ensureOsaioValuesSaved() {
  for (const v of [OSAIO_VALUES.serverKey, OSAIO_VALUES.appId]) {
    if (!osaioPanelVisible(v)) continue;
    const el = document.getElementById(`input-${v.id}`);
    if (!el || !el.value.trim()) {
      showToast(`Enter the Osaio ${v.name} first.`, 'warning');
      el?.focus();
      return false;
    }
    if (!(await saveOsaioValue(v, el.value))) return false;
    el.value = '';
  }
  return true;
}

function serverKeyPanelVisible() { return osaioPanelVisible(OSAIO_VALUES.serverKey); }
function appIdPanelVisible() { return osaioPanelVisible(OSAIO_VALUES.appId); }
function renderServerKeyPanel(sk) { state.serverKey = sk || null; renderOsaioPanel(OSAIO_VALUES.serverKey, sk); }
function renderAppIdPanel(st) { state.appId = st || null; renderOsaioPanel(OSAIO_VALUES.appId, st); }
function openServerKeyModal() { return openOsaioModal(OSAIO_VALUES.serverKey); }
function openAppIdModal() { return openOsaioModal(OSAIO_VALUES.appId); }
function handleResetServerKey() { return resetOsaioValue(OSAIO_VALUES.serverKey); }
function handleResetAppId() { return resetOsaioValue(OSAIO_VALUES.appId); }
function saveServerKey(value) { return saveOsaioValue(OSAIO_VALUES.serverKey, value); }
function saveAppId(value) { return saveOsaioValue(OSAIO_VALUES.appId, value); }
function handleSaveServerKeyPanel() { return saveOsaioPanel(OSAIO_VALUES.serverKey); }
function handleSaveAppIdPanel() { return saveOsaioPanel(OSAIO_VALUES.appId); }
function handleSubmitServerKeyModal() { return submitOsaioModal(OSAIO_VALUES.serverKey); }
function handleSubmitAppIdModal() { return submitOsaioModal(OSAIO_VALUES.appId); }

function mapSetupError(status, data) {
  if (data && data.error === 'server_key_missing') {
    renderServerKeyPanel({ configured: false, editable: true, source: '' });
    return data.message || 'Enter the Osaio server key first.';
  }
  if (data && data.error === 'app_id_missing') {
    renderAppIdPanel({ configured: false, editable: true, source: '' });
    return data.message || 'Enter the Osaio app ID first.';
  }
  if (data && data.error === 'upstream_unavailable') {
    return data.message || 'Osaio vendor cloud service is temporarily unavailable (HTTP 502). Please try again shortly.';
  }
  if (data && data.error === 'network_timeout') {
    return data.message || 'Connection to vendor cloud timed out. Please verify gateway internet connection.';
  }
  if (data && data.error === 'invalid_credentials') {
    return data.message || 'Invalid email or password. Please verify credentials in the Osaio app.';
  }
  if (status === 401) {
    return (data && (data.message || data.error)) || 'Invalid email or password. Please verify credentials in the Osaio app.';
  }
  if (status === 409) {
    return (data && (data.message || data.error)) || 'A profile is already configured. Use "Switch Account" to change accounts or re-authenticate.';
  }
  if (status === 503) {
    return (data && (data.message || data.error)) || 'Connection to vendor cloud timed out. Please verify gateway internet connection.';
  }
  if (status === 502) {
    return (data && (data.message || data.error)) || 'Osaio vendor cloud service is temporarily unavailable (HTTP 502). Please try again shortly.';
  }
  return (data && (data.message || data.error)) || 'Failed to connect vendor account.';
}

// Account Switching (Modal)
// Reset Profile Action
async function handleResetProfile() {
  if (!confirm('Are you sure you want to reset your local profile? This removes enrolled cameras from streaming. Your router settings are not changed.')) {
    return;
  }

  try {
    const res = await apiRequest('/api/v1/onboarding/reset', 'POST');
    if (res.ok) {
      showToast('Profile reset. Router settings unchanged.', 'warning');
      switchToLoginScreen();
      await checkOnboardingStatus();
    } else {
      showToast('Failed to reset profile.', 'error');
    }
  } catch (err) {
    showToast(`Reset error: ${err.message}`, 'error');
    switchToLoginScreen();
  }
}

// ============================================================================
// 4. Camera Discovery & Selective Enrollment
// ============================================================================

async function loadDiscoveredCameras(refresh = false) {
  const btn = document.getElementById('btn-refresh-discovery');
  if (btn) {
    btn.disabled = true;
    btn.textContent = 'Scanning...';
  }

  try {
    const url = `/api/v1/onboarding/cameras/discover${refresh ? '?refresh=true' : ''}`;
    const res = await apiRequest(url);

    if (res.status === 401) {
      showToast('Account credentials required before discovery.', 'warning');
      switchToLoginScreen();
      return;
    }

    if (!res.ok) {
      showToast('Failed to discover cameras from cloud.', 'error');
      return;
    }

    const data = await res.json();
    state.discoveredCameras = data.cameras || [];
    renderDiscoveryTable(state.discoveredCameras);
    renderDiscovery(state.discoveredCameras);
  } catch (err) {
    showToast(`Discovery error: ${err.message}`, 'error');
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = '🔍 Scan Cameras';
    }
  }
}

function renderDiscoveryTable(cameras) {
  const tbody = document.getElementById('discovery-table-body');
  if (!tbody) return;

  if (cameras.length === 0) {
    tbody.innerHTML = `<tr><td colspan="7" class="text-center text-muted">No cameras found on this Osaio account.</td></tr>`;
    return;
  }

  tbody.innerHTML = '';
  cameras.forEach(cam => {
    const isEnrolled = state.enrolledCameras.some(e => e.id === cam.id);
    const tr = document.createElement('tr');

    const statusBadge = cam.online
      ? '<span class="badge badge-success">Online</span>'
      : '<span class="badge badge-secondary">Offline</span>';

    const enrollBadge = isEnrolled
      ? '<span class="badge badge-active">Enrolled</span>'
      : '<span class="badge badge-secondary">Not Enrolled</span>';

    tr.innerHTML = `
      <td>
        <input type="checkbox" class="chk-cam-select" data-id="${escapeHtml(cam.id)}" ${isEnrolled ? 'checked' : ''}>
      </td>
      <td><strong>${escapeHtml(cam.name || 'Unnamed Camera')}</strong></td>
      <td>${escapeHtml(cam.model || 'Unknown')}</td>
      <td><code>${escapeHtml(cam.id)}</code></td>
      <td>${cam.ip ? `<code>${escapeHtml(cam.ip)}</code>` : '<span class="text-muted">Resolving...</span>'}</td>
      <td>${statusBadge}</td>
      <td>${enrollBadge}</td>
    `;
    tbody.appendChild(tr);
  });

  // Attach checkbox listeners
  document.querySelectorAll('.chk-cam-select').forEach(cb => {
    cb.addEventListener('change', updateSelectedDiscoveryCount);
  });
  updateSelectedDiscoveryCount();
}

function updateSelectedDiscoveryCount() {
  state.selectedDiscoveryIds.clear();
  document.querySelectorAll('.chk-cam-select:checked').forEach(cb => {
    state.selectedDiscoveryIds.add(cb.getAttribute('data-id'));
  });

  const count = state.selectedDiscoveryIds.size;
  const enrollBtn = document.getElementById('btn-enroll-selected');
  if (enrollBtn) {
    enrollBtn.textContent = `Enroll Selected (${count})`;
    enrollBtn.disabled = count === 0;
  }
}

async function handleEnrollSelected() {
  const ids = Array.from(state.selectedDiscoveryIds);
  if (ids.length === 0) return;

  const btn = document.getElementById('btn-enroll-selected');
  btn.disabled = true;
  btn.textContent = 'Enrolling...';

  try {
    const res = await apiRequest('/api/v1/onboarding/cameras/enroll', 'POST', {
      camera_ids: ids
    });
    if (res.ok) {
      showToast(`Successfully enrolled ${ids.length} camera(s)!`, 'success');
      await loadEnrolledCameras();
      await refreshOnboardingSnapshot();
      switchTab('tab-dashboard');
    } else {
      const data = await res.json().catch(() => ({}));
      showToast(data.error || 'Failed to enroll cameras.', 'error');
    }
  } catch (err) {
    showToast(`Enrollment error: ${err.message}`, 'error');
  } finally {
    btn.disabled = false;
    updateSelectedDiscoveryCount();
  }
}

// Camera list rendering
function renderDiscovery(cameras) {
  const list = document.getElementById('discovery-cameras-list');
  const countBadge = document.getElementById('discovery-count-badge');
  const selectAllChk = document.getElementById('chk-discovery-select-all');
  const addBtn = document.getElementById('btn-add-to-bombecam');
  if (!list) return;

  list.innerHTML = '';
  state.selectedDiscoveryIds.clear();

  if (!cameras || cameras.length === 0) {
    if (countBadge) countBadge.textContent = '0 cameras found';
    if (selectAllChk) { selectAllChk.checked = false; selectAllChk.disabled = true; }
    if (addBtn) { addBtn.disabled = true; addBtn.textContent = 'Add to BombeCam'; }
    list.innerHTML = `
      <div class="discovery-empty">
        <p class="text-muted">No cameras found on this Osaio account or network.</p>
        <button id="btn-discovery-retry" class="btn btn-secondary btn-sm" style="margin-top: 8px;">
          🔍 Scan Again
        </button>
      </div>
    `;
    document.getElementById('btn-discovery-retry')?.addEventListener('click', async () => {
      showOnboardingScreen('screen-loading');
      setLoadingState('Scanning Cameras...', 'Querying Osaio cloud and local network...');
      const res = await apiRequest('/api/v1/onboarding/cameras/discover?refresh=true');
      if (res.ok) {
        const d = await res.json();
        state.discoveredCameras = d.cameras || [];
      }
      renderDiscovery(state.discoveredCameras);
      showOnboardingScreen('screen-discovery');
    });
    return;
  }

  if (selectAllChk) {
    selectAllChk.disabled = false;
    selectAllChk.checked = true;
  }

  if (countBadge) {
    countBadge.textContent = `${cameras.length} camera${cameras.length > 1 ? 's' : ''} found`;
  }

  cameras.forEach(cam => {
    // Select all discovered cameras by default
    state.selectedDiscoveryIds.add(cam.id);

    const card = document.createElement('div');
    card.className = 'discovery-camera-item';
    card.setAttribute('data-id', cam.id);

    const statusBadge = cam.online
      ? '<span class="badge badge-success">Online</span>'
      : '<span class="badge badge-secondary">Offline</span>';

    card.innerHTML = `
      <label class="discovery-camera-label">
        <input type="checkbox" class="chk-discovery-item" data-id="${escapeHtml(cam.id)}" checked>
        <div class="discovery-camera-details">
          <div class="discovery-camera-primary">
            <span class="discovery-camera-name">${escapeHtml(cam.name || 'Camera')}</span>
            ${statusBadge}
          </div>
          <div class="discovery-camera-meta text-muted">
            <span>Model: <strong>${escapeHtml(cam.model || 'Unknown')}</strong></span>
            <span>IP: <code>${escapeHtml(cam.ip || 'Resolving...')}</code></span>
            <span>UUID: <code>${escapeHtml(cam.id)}</code></span>
          </div>
        </div>
      </label>
    `;
    list.appendChild(card);
  });

  // Attach individual item checkbox listeners
  document.querySelectorAll('.chk-discovery-item').forEach(cb => {
    cb.addEventListener('change', () => {
      const id = cb.getAttribute('data-id');
      if (cb.checked) {
        state.selectedDiscoveryIds.add(id);
      } else {
        state.selectedDiscoveryIds.delete(id);
      }
      updateDiscoveryActionState();
    });
  });

  updateDiscoveryActionState();
}

function updateDiscoveryActionState() {
  const total = state.discoveredCameras.length;
  const selected = state.selectedDiscoveryIds.size;
  const selectAllChk = document.getElementById('chk-discovery-select-all');
  const addBtn = document.getElementById('btn-add-to-bombecam');

  if (selectAllChk && total > 0) {
    selectAllChk.checked = selected === total;
    selectAllChk.indeterminate = selected > 0 && selected < total;
  }

  if (addBtn) {
    addBtn.disabled = selected === 0;
    addBtn.textContent = selected > 0
      ? `Add to BombeCam (${selected})`
      : 'Add to BombeCam';
  }
}

async function handleAddToBombeCam() {
  const selectedIds = Array.from(state.selectedDiscoveryIds);
  if (selectedIds.length === 0) return;

  const addBtn = document.getElementById('btn-add-to-bombecam');
  if (addBtn) {
    addBtn.disabled = true;
    addBtn.textContent = 'Enrolling Cameras...';
  }

  try {
    // Add the chosen cameras to whatever is already there, each through
    // the login that listed it.
    const byLogin = {};
    selectedIds.forEach(id => {
      const cam = state.discoveredCameras.find(c => (c.id || c.uuid) === id) || {};
      (byLogin[cam.account_email || ''] = byLogin[cam.account_email || ''] || []).push(id);
    });
    let res = { ok: true };
    let data = {};
    for (const [email, ids] of Object.entries(byLogin)) {
      res = email
        ? await apiRequest('/api/v1/cameras/add', 'POST', { email, camera_ids: ids })
        : await apiRequest('/api/v1/onboarding/cameras/enroll', 'POST', { camera_ids: ids });
      data = await res.json();
      if (!res.ok) break;
    }

    if (!res.ok) {
      showToast(data.error || 'Failed to enroll cameras.', 'error');
      if (addBtn) {
        addBtn.disabled = false;
        addBtn.textContent = `Add to BombeCam (${selectedIds.length})`;
      }
      return;
    }

    showToast(`Successfully enrolled ${selectedIds.length} camera(s)!`, 'success');

    // Transition directly to post-enrollment workspace
    const viewOnboarding = document.getElementById('view-onboarding');
    const viewWorkspace = document.getElementById('view-workspace');
    const navTabs = document.getElementById('main-nav-tabs');

    if (viewOnboarding) viewOnboarding.style.display = 'none';
    if (navTabs) navTabs.style.display = 'flex';
    if (viewWorkspace) viewWorkspace.style.display = 'block';

    switchTab('tab-dashboard');
    await loadEnrolledCameras();
    await refreshOnboardingSnapshot();
    await refreshPrivacyStatus();
  } catch (err) {
    showToast(`Enrollment error: ${err.message}`, 'error');
    if (addBtn) {
      addBtn.disabled = false;
      addBtn.textContent = `Add to BombeCam (${selectedIds.length})`;
    }
  }
}

// ============================================================================
// 5. Camera Selection & Active Streams
// ============================================================================

async function loadEnrolledCameras() {
  try {
    const res = await apiRequest('/api/v1/integrations/streams');
    if (!res.ok) return;

    const data = await res.json();
    state.enrolledCameras = data.streams || [];
    renderAccountChip();

    const select = document.getElementById('stream-camera-select');
    if (!select) return;

    select.innerHTML = '';
    if (state.enrolledCameras.length === 0) {
      select.innerHTML = '<option value="">No cameras yet</option>';
      document.getElementById('selected-cam-name').textContent = 'No camera enrolled';
      clearStreamDetails();
      return;
    }

    state.enrolledCameras.forEach((cam, idx) => {
      const opt = document.createElement('option');
      opt.value = cam.uuid || cam.id;
      opt.textContent = cam.name || 'Camera';
      opt.title = cam.uuid || cam.id;
      select.appendChild(opt);
    });

    // Default select first camera if none selected
    if (!state.selectedCameraId || !state.enrolledCameras.some(c => (c.uuid || c.id) === state.selectedCameraId)) {
      state.selectedCameraId = state.enrolledCameras[0].uuid || state.enrolledCameras[0].id;
    }
    select.value = state.selectedCameraId;

    await selectActiveCamera(state.selectedCameraId);
  } catch (err) {
    console.error('[streams] loadEnrolledCameras error:', err);
  }
}

// Pan/tilt and white spotlight by model, as Osaio reports it in the device
// list's "type" (e.g. "WS03", "K1PRO", "GC3_A3S11A3"). The same model codes are
// sold under several brands (Yoton, GNCC, Wolfang, Surfola). Keep this table
// the same as cameraModels in cmd/bombecam-gateway/mqtt_discovery.go; a Go
// test compares them.
const CAMERA_CAPABILITY_MATRIX = {
  // Indoor pan/tilt
  'WS03':   { ptz: true,  spotlight: false, name: 'Indoor 2K pan/tilt (WS03)' },
  'P1':     { ptz: true,  spotlight: false, name: 'Indoor pan/tilt (P1)' },
  'P1PRO':  { ptz: true,  spotlight: false, name: 'Indoor 2K pan/tilt (P1 Pro)' },
  'P5':     { ptz: true,  spotlight: false, name: 'Indoor 2K pan/tilt (P5, GP5)' },
  'P10':    { ptz: true,  spotlight: false, name: 'Indoor pan/tilt (P10)' },

  // Indoor fixed
  'WS01':   { ptz: false, spotlight: false, name: 'Indoor fixed (WS01)' },
  'C1':     { ptz: false, spotlight: false, name: 'Indoor fixed (C1)' },
  'C1PRO':  { ptz: false, spotlight: false, name: 'Indoor 2K fixed (C1 Pro)' },
  'C2':     { ptz: false, spotlight: false, name: 'Indoor fixed (C2)' },
  'GC2':    { ptz: false, spotlight: false, name: 'Indoor fixed (GC2)' },
  'GC3':    { ptz: false, spotlight: false, name: 'Indoor fixed (GC3)' },

  // Outdoor pan/tilt with spotlight
  'K1':     { ptz: true,  spotlight: true,  name: 'Outdoor pan/tilt spotlight (K1)' },
  'K1PRO':  { ptz: true,  spotlight: true,  name: 'Outdoor 2K pan/tilt spotlight (K1 Pro)' },
  'GK1':    { ptz: true,  spotlight: true,  name: 'Outdoor pan/tilt spotlight (GK1)' },
  'GK1PRO': { ptz: true,  spotlight: true,  name: 'Outdoor 2K pan/tilt spotlight (GK1 Pro)' },
  'GK2':    { ptz: true,  spotlight: true,  name: 'Outdoor pan/tilt spotlight (GK2)' },
  'GL1':    { ptz: true,  spotlight: true,  name: 'Light bulb pan/tilt (GL1)' },
  'WS04':   { ptz: true,  spotlight: true,  name: 'Outdoor solar pan/tilt spotlight (WS04)' },
  'GW30':   { ptz: true,  spotlight: true,  name: 'Outdoor solar pan/tilt spotlight (GW30)' },
  'GW40':   { ptz: true,  spotlight: true,  name: 'Outdoor 4G solar pan/tilt spotlight (GW40)' },

  // Outdoor fixed with spotlight
  'GW1':    { ptz: false, spotlight: true,  name: 'Outdoor battery spotlight (GW1)' },

  // Outdoor fixed (infrared only)
  'WS02':   { ptz: false, spotlight: false, name: 'Outdoor 2K fixed (WS02)' },
  'T1':     { ptz: false, spotlight: false, name: 'Outdoor fixed (T1)' },
  'T1PRO':  { ptz: false, spotlight: false, name: 'Outdoor 2K fixed (T1 Pro)' },
  'GT1':    { ptz: false, spotlight: false, name: 'Outdoor fixed (GT1)' },
  'GT1PRO': { ptz: false, spotlight: false, name: 'Outdoor 2K fixed (GT1 Pro)' },
};

// cameraModelKey reduces Osaio's model to a table key: the part before "_"
// (a hardware revision, as in "GC3_A3S11A3"), upper case, letters and digits
// only. An exact key wins; otherwise the longest key inside it ("GP5" -> P5),
// the first alphabetically on a tie (as in mqtt_discovery.go).
function cameraModelKey(modelStr) {
  const base = String(modelStr || '').toUpperCase().split('_')[0].replace(/[^A-Z0-9]/g, '');
  if (!base) return '';
  if (CAMERA_CAPABILITY_MATRIX[base]) return base;
  let best = '';
  for (const key of Object.keys(CAMERA_CAPABILITY_MATRIX)) {
    if (base.includes(key) && (key.length > best.length || (key.length === best.length && key < best))) best = key;
  }
  return best;
}

function getCameraCapabilities(modelStr) {
  if (!modelStr) return { ptz: true, spotlight: false, name: 'Standard Camera' };
  const key = cameraModelKey(modelStr);
  if (key) return CAMERA_CAPABILITY_MATRIX[key];
  // Unknown model: offer pan/tilt (it does nothing on a fixed camera) and hide
  // the spotlight.
  return { ptz: true, spotlight: false, name: modelStr };
}

function applyModelCapabilities(cam) {
  const modelStr = (cam && cam.model) ? cam.model : '';
  const caps = getCameraCapabilities(modelStr);

  // 1. PTZ Controls
  const ptzDpad = document.getElementById('ptz-dpad');
  const ptzBadge = document.getElementById('ptz-status-badge');
  const ptzBtns = ptzDpad ? ptzDpad.querySelectorAll('.ptz-dir, .ptz-stop') : [];

  if (caps.ptz) {
    if (ptzDpad) ptzDpad.classList.remove('control-disabled');
    if (ptzBadge) ptzBadge.classList.add('hidden');
    ptzBtns.forEach(btn => btn.disabled = false);
  } else {
    if (ptzDpad) ptzDpad.classList.add('control-disabled');
    if (ptzBadge) ptzBadge.classList.remove('hidden');
    ptzBtns.forEach(btn => btn.disabled = true);
  }

  // 2. Spotlight / Floodlight Control
  const rowLight = document.getElementById('row-light-control');
  const lightNA = document.getElementById('light-unsupported');
  const lightBtns = ['on', 'off'].map(v => document.getElementById(`btn-light-${v}`)).filter(Boolean);

  if (caps.spotlight) {
    if (rowLight) rowLight.classList.remove('control-disabled');
    if (rowLight) rowLight.style.display = '';
    lightBtns.forEach(b => { b.disabled = false; b.title = b.dataset.onoff === 'on' ? 'The white spotlight is on' : 'The white spotlight is off'; });
    if (lightNA) lightNA.style.display = 'none';
  } else {
    if (rowLight) rowLight.classList.add('control-disabled');
    lightBtns.forEach(b => { b.disabled = true; b.title = 'This camera model has no white spotlight'; b.classList.remove('active'); });
    if (lightNA) lightNA.style.display = '';
  }
  // Only show the spotlight row on cameras that have one.
  if (rowLight) rowLight.style.display = caps.spotlight ? '' : 'none';
}

async function selectActiveCamera(camId) {
  const changed = camId !== state.selectedCameraId || viewer.camId !== camId;
  const stopping = changed ? stopTalkback() : Promise.resolve();
  state.selectedCameraId = camId || '';
  if (!camId) {
    teardownVideo();
    viewer.camId = '';
    clearStreamDetails();
    await stopping;
    return;
  }

  const camObj = state.enrolledCameras.find(c => (c.uuid || c.id) === camId);
  if (camObj) {
    document.getElementById('selected-cam-name').textContent = camObj.name || camId;
    applyModelCapabilities(camObj);
  }


  if (changed) {
    viewer.copySwitches = 0;
    viewer.webrtcFailures = 0;
    viewer.preferHLS = false;
    teardownVideo();
    viewer.camId = camId;
    setOverlay('Checking camera stream...');
  }
  await viewerTick();

  // Fetch initial truthful shadow telemetry readback
  if (camId === state.selectedCameraId) await pollShadowTelemetry(camId, false);
  await stopping;
}

const MEDIA_PATH_HINTS = {
  'Direct (LAN)': 'Video and sound come straight from the camera over your local network.',
  'Direct (internet)': 'Video and sound come straight from the camera, but not over your local network.',
  'Relayed via Osaio': "Video and sound pass through Osaio's relay server, not your local network.",
};

function populateStreamDetails(desc) {
  const rtspInput = document.getElementById('stream-rtsp-url');
  const hlsInput = document.getElementById('stream-hls-url');
  const path = document.getElementById('media-path-badge');
  if (path) {
    const known = MEDIA_PATH_HINTS[desc.transport];
    path.textContent = known ? desc.transport : '';
    path.title = known || '';
    path.classList.toggle('hidden', !known);
    path.classList.toggle('warn', desc.transport === 'Relayed via Osaio');
  }

  // Prefer the LAN address so the URL works when pasted into an NVR/VLC on another device.
  if (rtspInput) rtspInput.value = desc.rtsp_url_lan || desc.rtsp_url || '';
  if (hlsInput) hlsInput.value = desc.hls_url_lan || desc.hls_url || '';
}

function clearStreamDetails() {
  const rtspInput = document.getElementById('stream-rtsp-url');
  const hlsInput = document.getElementById('stream-hls-url');
  if (rtspInput) rtspInput.value = '';
  if (hlsInput) hlsInput.value = '';
  teardownVideo();
  viewer.camId = '';
  setOverlay('Select a camera to view its stream');
  applyModelCapabilities(null);
}

// ============================================================================
// 6. Live Video: readiness-driven playback (WebRTC/WHEP first, HLS fallback)
// ============================================================================
//
// The viewer never attaches a player to a stream that does not exist yet.
// Every 2 s viewerTick() asks the gateway for the camera's stream state
// (which is checked against MediaMTX itself), shows the reason while it is not
// ready, attaches when it is, and re-attaches if frames stop, so the video
// element never waits without an explanation.

function setOverlay(text) {
  const overlay = document.getElementById('preview-overlay');
  const label = document.getElementById('preview-overlay-text');
  if (!overlay) return;
  if (text) {
    if (label) label.textContent = text;
    overlay.classList.remove('hidden');
  } else {
    overlay.classList.add('hidden');
  }
}

function teardownVideo() {
  viewer.generation++;
  const tracker = state.frameTracker;
  if (tracker && tracker.callbackId != null && typeof tracker.player.cancelVideoFrameCallback === 'function') {
    tracker.player.cancelVideoFrameCallback(tracker.callbackId);
  }
  state.frameTracker = null;
  state.lastDecodedFrameAdvance = null;
  state.decodedFrameCount = 0;
  viewer.mode = 'none';
  viewer.attaching = false;
  if (state.whepPeerConnection) {
    try { state.whepPeerConnection.close(); } catch (_) {}
    state.whepPeerConnection = null;
  }
  if (state.hlsInstance) {
    try { state.hlsInstance.destroy(); } catch (_) {}
    state.hlsInstance = null;
  }
  const player = document.getElementById('preview-player');
  if (player) {
    player.ontimeupdate = null;
    try { player.pause(); } catch (_) {}
    player.removeAttribute('src');
    player.srcObject = null;
    try { player.load(); } catch (_) {}
  }
  updateDecodedFrameBadge('paused');
}

// Resolves once the player has a decoded frame with real dimensions.
function waitForFirstFrame(player, gen, timeoutMs) {
  return new Promise((resolve, reject) => {
    let finished = false;
    let poll = null;
    let timer = null;
    const finish = (err) => {
      if (finished) return;
      finished = true;
      clearInterval(poll);
      clearTimeout(timer);
      if (err) reject(err); else resolve();
    };
    timer = setTimeout(() => finish(new Error(`no video frames within ${Math.round(timeoutMs / 1000)}s`)), timeoutMs);
    poll = setInterval(() => {
      if (gen !== viewer.generation) return finish(new Error('cancelled'));
      if (player.videoWidth > 0 && player.readyState >= 2) finish(null);
    }, 150);
  });
}

// Records decoded-frame progress for the badge and the stall detector.
function decodedFrameMetric(player) {
  try {
    if (typeof player.getVideoPlaybackQuality === 'function') {
      const q = player.getVideoPlaybackQuality();
      if (Number.isFinite(q.totalVideoFrames) && Number.isFinite(q.droppedVideoFrames)) {
        return Math.max(0, q.totalVideoFrames - q.droppedVideoFrames);
      }
    }
  } catch (_) {}
  for (const count of [player.webkitDecodedFrameCount, player.mozDecodedFrames]) {
    if (Number.isFinite(count) && count >= 0) return count;
  }
  return null;
}

function frameTrackerMatches(tracker, player) {
  return tracker && tracker === state.frameTracker && tracker.player === player &&
    tracker.generation === viewer.generation && tracker.src === player.src &&
    tracker.srcObject === player.srcObject;
}

function trackFrames(player, gen) {
  const tracker = {
    player, generation: gen, src: player.src, srcObject: player.srcObject,
    metric: decodedFrameMetric(player), count: 0, advancedAt: null, startedAt: performance.now(),
    callbackId: null, presented: null,
    callbacks: typeof player.requestVideoFrameCallback === 'function'
  };
  state.frameTracker = tracker;
  state.decodedFrameCount = 0;
  state.lastDecodedFrameAdvance = null;
  player.ontimeupdate = null; // clock events do not prove decoded-frame progress
  if (tracker.callbacks) {
    const onFrame = (_, metadata) => {
      if (!frameTrackerMatches(tracker, player)) return;
      const presented = metadata && metadata.presentedFrames;
      if (!Number.isFinite(presented) || presented !== tracker.presented) {
        tracker.presented = presented;
        tracker.count++;
        tracker.advancedAt = performance.now();
        state.decodedFrameCount = tracker.count;
        state.lastDecodedFrameAdvance = tracker.advancedAt;
      }
      tracker.callbackId = player.requestVideoFrameCallback(onFrame);
    };
    tracker.callbackId = player.requestVideoFrameCallback(onFrame);
  }
}

function waitForIceGathering(pc, timeoutMs) {
  if (pc.iceGatheringState === 'complete') return Promise.resolve();
  return new Promise(resolve => {
    const t = setTimeout(resolve, timeoutMs);
    pc.addEventListener('icegatheringstatechange', () => {
      if (pc.iceGatheringState === 'complete') {
        clearTimeout(t);
        resolve();
      }
    });
  });
}

async function startWHEP(camId, player, gen) {
  if (typeof RTCPeerConnection === 'undefined') {
    throw new Error('WebRTC not supported in this browser');
  }
  // LAN/localhost only: no third-party STUN servers are contacted.
  const pc = new RTCPeerConnection();
  state.whepPeerConnection = pc;
  // Sound off: ask for the video-only copy, which skips the delay that keeps
  // sound and picture in step (the gateway uses the full stream if the copy
  // isn't available).
  const videoOnly = !state.soundOn;
  pc.addTransceiver('video', { direction: 'recvonly' });
  if (!videoOnly) pc.addTransceiver('audio', { direction: 'recvonly' });

  const stream = new MediaStream();
  pc.ontrack = (event) => {
    if (gen !== viewer.generation) { event.track.stop(); return; }
    stream.addTrack(event.track);
    if (player.srcObject !== stream) player.srcObject = stream;
  };
  pc.onconnectionstatechange = () => {
    if (gen !== viewer.generation) return;
    if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
      scheduleReattach(`WebRTC ${pc.connectionState}`);
    }
  };

  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  // Non-trickle WHEP: send our candidates inside the offer.
  await waitForIceGathering(pc, 1500);

  const res = await apiRequest(`/api/v1/cameras/${encodeURIComponent(camId)}/whep${videoOnly ? '?video=only' : ''}`, 'POST',
    new Blob([pc.localDescription.sdp], { type: 'application/sdp' }));
  if (!res.ok) {
    throw new Error(`WHEP endpoint returned HTTP ${res.status}`);
  }
  viewer.videoOnly = videoOnly;
  viewer.onCopy = res.headers.get('X-BombeCam-Stream') === 'video-only';
  const answer = await res.text();
  if (gen !== viewer.generation) throw new Error('cancelled');
  await pc.setRemoteDescription({ type: 'answer', sdp: answer });

  player.muted = !state.soundOn;
  player.play().catch(() => {});
  await waitForFirstFrame(player, gen, 8000);
}

async function startHLS(hlsUrl, player, gen) {
  if (typeof Hls !== 'undefined' && Hls.isSupported()) {
    const hls = new Hls({
      enableWorker: true,
      lowLatencyMode: false,
      liveSyncDurationCount: 2,
      liveMaxLatencyDurationCount: 6,
      maxLiveSyncPlaybackRate: 1.2,
      backBufferLength: 10,
      manifestLoadingMaxRetry: 6,
      levelLoadingMaxRetry: 6,
      fragLoadingMaxRetry: 6,
    });
    state.hlsInstance = hls;
    hls.on(Hls.Events.ERROR, (event, data) => {
      if (!data.fatal || gen !== viewer.generation) return;
      if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
        hls.recoverMediaError();
        return;
      }
      scheduleReattach(`HLS ${data.details || data.type}`);
    });
    hls.loadSource(hlsUrl);
    hls.attachMedia(player);
  } else if (player.canPlayType('application/vnd.apple.mpegurl')) {
    player.src = hlsUrl; // Safari native HLS
  } else {
    throw new Error('this browser cannot play HLS');
  }
  player.muted = !state.soundOn;
  player.play().catch(() => {});
  await waitForFirstFrame(player, gen, 15000);
}

async function attachVideo(camId, desc) {
  const player = document.getElementById('preview-player');
  if (!player || viewer.attaching) return;
  teardownVideo();
  const gen = viewer.generation;
  viewer.attaching = true;
  viewer.camId = camId;
  setOverlay('Starting video...');

  let lastErr = null;
  if (viewer.webrtcFailures < 2 && !viewer.preferHLS) {
    try {
      await startWHEP(camId, player, gen);
      if (gen !== viewer.generation) return;
      viewer.mode = 'webrtc';
      viewer.webrtcFailures = 0;
    } catch (err) {
      if (gen !== viewer.generation) return;
      lastErr = err;
      viewer.webrtcFailures++;
      console.warn('[video] WebRTC playback failed, falling back to HLS:', err);
      if (state.whepPeerConnection) {
        try { state.whepPeerConnection.close(); } catch (_) {}
        state.whepPeerConnection = null;
      }
      player.srcObject = null;
    }
  }

  if (viewer.mode === 'none' && desc && desc.hls_url) {
    try {
      await startHLS(desc.hls_url, player, gen);
      if (gen !== viewer.generation) return;
      viewer.mode = 'hls';
    } catch (err) {
      if (gen !== viewer.generation) return;
      lastErr = err;
      console.warn('[video] HLS playback failed:', err);
    }
  }

  viewer.attaching = false;
  if (viewer.mode === 'none') {
    teardownVideo();
    setOverlay(`The camera is streaming, but playback did not start (${lastErr ? lastErr.message : 'unknown error'}). Retrying...`);
    return;
  }
  trackFrames(player, gen); // source is established; cached dimensions are not live evidence
  console.log(`[video] playing ${camId} via ${viewer.mode}`);
  setOverlay('');
  applySoundState();
  ensureSoundPath();
}

// Whether camera sound is actually arriving. For WebRTC this is measured:
// MediaMTX answers an audio section even when the stream has no
// WebRTC-compatible audio (built-in publisher = AAC only), so the SDP and the
// track list can't tell. HLS always carries the camera's AAC audio.
async function streamHasAudio() {
  if (viewer.mode !== 'webrtc') return true;
  const pc = state.whepPeerConnection;
  if (!pc) return false;
  const packets = async () => {
    let n = 0;
    (await pc.getStats()).forEach(r => {
      if (r.type === 'inbound-rtp' && (r.kind === 'audio' || r.mediaType === 'audio')) n += r.packetsReceived || 0;
    });
    return n;
  };
  if (await packets() > 0) return true;
  await sleep(1500);
  return (await packets()) > 0;
}

async function ensureSoundPath() {
  if (!state.soundOn || viewer.mode !== 'webrtc') return;
  const gen = viewer.generation;
  const hasAudio = await streamHasAudio();
  if (!hasAudio && gen === viewer.generation && state.soundOn) {
    viewer.preferHLS = true;
    showToast('Switching to the stream that carries sound (a couple of seconds more delay).', 'info');
    scheduleReattach('sound needs the HLS stream');
  }
}

function applySoundState() {
  const player = document.getElementById('preview-player');
  const btn = document.getElementById('btn-sound');
  const slider = document.getElementById('volume-slider');
  if (player) {
    player.muted = !state.soundOn;
    player.volume = state.volume;
  }
  if (slider) slider.value = String(Math.round(state.volume * 100));
  if (btn) {
    btn.textContent = !state.soundOn || state.volume === 0 ? '🔇' : (state.volume < 0.5 ? '🔉' : '🔊');
    btn.title = state.soundOn ? 'Mute camera sound' : 'Turn camera sound on';
  }
}

function setVolume(fraction) {
  state.volume = Math.max(0, Math.min(1, fraction));
  try { localStorage.setItem('bombecam.volume', String(state.volume)); } catch (_) {}
  // Moving the slider up means "I want to hear it".
  if (state.volume > 0 && !state.soundOn) {
    toggleSound();
    return;
  }
  applySoundState();
}

function toggleSound() {
  state.soundOn = !state.soundOn;
  const player = document.getElementById('preview-player');
  applySoundState();
  if (state.soundOn && viewer.mode === 'webrtc' && viewer.videoOnly) {
    // the muted view has no sound track: reconnect with sound
    scheduleReattach('sound on');
    return;
  }
  if (state.soundOn && player) {
    player.play().catch(() => {});
    ensureSoundPath();
  }
}

function scheduleReattach(reason) {
  if (viewer.mode === 'none' && !viewer.attaching) return;
  console.warn('[video] re-attaching:', reason);
  teardownVideo();
  setOverlay('Reconnecting video...');
}

async function viewerTick() {
  const camId = state.selectedCameraId;
  const workspace = document.getElementById('view-workspace');
  if (!camId || viewer.tickBusy || (workspace && workspace.style.display === 'none')) return;
  viewer.tickBusy = true;
  try {
    const res = await apiRequest(`/api/v1/cameras/${encodeURIComponent(camId)}/stream`);
    if (camId !== state.selectedCameraId) return;
    if (!res.ok) {
      if (viewer.mode === 'none' && !viewer.attaching) setOverlay(`Camera not available (HTTP ${res.status})`);
      return;
    }
    const desc = await res.json();
    if (camId !== state.selectedCameraId) return;
    state.streamDescriptors[camId] = desc;
    populateStreamDetails(desc);

    const tracker = state.frameTracker;
    const sinceFrame = performance.now() - (state.lastDecodedFrameAdvance ?? (tracker && tracker.startedAt) ?? performance.now());
    renderStopButton(desc.stopped);
    const retryBtn = document.getElementById('btn-overlay-retry');
    if (retryBtn) retryBtn.style.display = desc.state === 'locked_out' ? '' : 'none';
    if (!desc.publication_ready) {
      if (viewer.mode !== 'none' && sinceFrame > 4000) teardownVideo(); // stream went away
      if (viewer.mode === 'none' && !viewer.attaching) {
        setOverlay(desc.state_detail || 'Waiting for the camera...');
      }
      return;
    }
    if (viewer.attaching) return;
    if (viewer.mode === 'none') {
      attachVideo(camId, desc);
      return;
    }
    if (sinceFrame > 10000) {
      scheduleReattach('no frames for 10s');
      return;
    }
    // Muted view on the full stream because the video-only copy wasn't
    // ready yet: move to the copy once, for the shorter delay.
    if (viewer.mode === 'webrtc' && viewer.videoOnly && !viewer.onCopy && desc.video_only_copy && !state.soundOn &&
        (viewer.copySwitches || 0) < 2) {
      viewer.copySwitches = (viewer.copySwitches || 0) + 1;
      scheduleReattach('video-only copy ready');
    }
  } catch (err) {
    console.warn('[video] stream status check failed:', err);
  } finally {
    viewer.tickBusy = false;
  }
}

/**
 * 250ms interval watchdog: continuously evaluates decoded-frame progress.
 * Invariant: Video displays "Stream Active (Decoded)" ONLY when genuine video frames are actively decoded
 * and video dimensions (width & height) are greater than zero.
 */
function decodedFrameWatchdog() {
  const player = document.getElementById('preview-player');
  if (!player || (!player.src && !player.srcObject && !state.hlsInstance) || player.paused || player.ended) {
    updateDecodedFrameBadge('paused');
    return;
  }

  // Video must have valid rendered frame dimensions
  const hasDimensions = (player.videoWidth > 0 && player.videoHeight > 0);
  if (!hasDimensions) {
    updateDecodedFrameBadge('buffering');
    return;
  }

  const tracker = state.frameTracker;
  if (!frameTrackerMatches(tracker, player)) {
    updateDecodedFrameBadge('buffering');
    return;
  }
  if (!tracker.callbacks) {
    const metric = decodedFrameMetric(player);
    if (metric != null && tracker.metric != null && metric > tracker.metric) {
      tracker.count++;
      tracker.advancedAt = performance.now();
      state.lastDecodedFrameAdvance = tracker.advancedAt;
      state.decodedFrameCount = tracker.count;
    } else if (metric != null && tracker.metric != null && metric < tracker.metric) {
      tracker.count = 0;
      tracker.advancedAt = null;
      state.lastDecodedFrameAdvance = null;
    }
    tracker.metric = metric;
  }
  const fresh = tracker.advancedAt != null && performance.now() - tracker.advancedAt < 1500;
  updateDecodedFrameBadge(tracker.count > 0 && fresh ? 'active' : 'buffering');
}

function updateDecodedFrameBadge(status) {
  // Only "live" is worth showing; every other state is explained by the overlay.
  const badge = document.getElementById('decoded-frame-badge');
  if (!badge) return;
  badge.classList.toggle('hidden', status !== 'active');
}

// Digital Zoom Controls
function applyZoom(delta, reset = false) {
  const player = document.getElementById('preview-player');
  if (!player) return;

  if (reset) {
    state.currentZoom = 1.0;
  } else {
    state.currentZoom = Math.min(3.0, Math.max(1.0, state.currentZoom + delta));
  }

  player.style.transform = `scale(${state.currentZoom})`;
  document.getElementById('zoom-level-text').textContent = `${state.currentZoom.toFixed(1)}x`;
}

// ============================================================================
// 7. Auxiliary Controls & Anti-Optimistic Truthful Telemetry
// ============================================================================

/**
 * Sends a PTZ pulse (400ms duration).
 */
async function sendPTZ(direction) {
  if (!state.selectedCameraId) {
    showToast('Select an active camera first.', 'warning');
    return;
  }

  try {
    const res = await apiRequest(`/api/v1/cameras/${encodeURIComponent(state.selectedCameraId)}/ptz`, 'POST', {
      direction: direction,
      duration_ms: 400
    });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      showToast(controlErrorMessage(err.error, 'Pan/tilt command failed.'), 'warning');
    }
  } catch (err) {
    console.error('[ptz] sendPTZ error:', err);
  }
}

/**
 * Turns a gateway control error into something a person can act on.
 * Controls travel over the camera's Osaio connection, which reconnects on its
 * own after a network drop; a click while it's down makes it retry at once.
 */
function controlErrorMessage(raw, fallback) {
  const text = String(raw || '');
  if (/reconnecting/i.test(text)) {
    return "Controls are reconnecting to Osaio's server. Try again in a few seconds.";
  }
  if (/not connected|transport unavailable|no signaling|signaling closed|channel is closed/i.test(text)) {
    return "Controls work while the camera's stream is running. Wait for the stream to start, then try again.";
  }
  return text || fallback;
}

/**
 * Sends an auxiliary command without waiting for optional camera telemetry.
 * A completed send is reported as Sent; genuine dispatch failures stay visible.
 */
async function sendAuxControl(action, mode) {
  if (!state.selectedCameraId) {
    showToast('Select an active camera first.', 'warning');
    return;
  }
  const camId = state.selectedCameraId;

  // Mark action updating
  setControlUpdating(action, true);

  try {
    const payload = { action: action };
    if (mode !== undefined) {
      payload.mode = mode;
      // The gateway reads "value" before "mode". IR has three states
      // (auto/off/on), so a 0/1 value would turn "off" into "auto".
      if (action !== 'ir') {
        payload.value = (mode === 'on' || mode === 1 ? 1 : 0);
      }
    }

    const res = await apiRequest(`/api/v1/cameras/${encodeURIComponent(camId)}/control`, 'POST', payload);
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      showToast(controlErrorMessage(err.error, `Failed to dispatch ${action}`), 'error');
      setControlUpdating(action, false);
      return;
    }

    const label = { ir: 'Night vision', led: 'Status light', light: 'Spotlight', motion: 'Motion detection', sound: 'Sound detection' }[action] || action;
    showToast(`${label}: command sent.`, 'success');
  } catch (err) {
    showToast(`Control error: ${err.message}`, 'error');
  } finally {
    setControlUpdating(action, false);
  }
}

function setControlUpdating(action, updating) {
  const statusElem = document.getElementById('telemetry-readback-status');

  if (updating) {
    state.updatingControls.add(action);
    if (statusElem) {
      statusElem.textContent = 'Sending...';
      statusElem.className = 'telemetry-status updating';
    }
  } else {
    state.updatingControls.delete(action);
    if (state.updatingControls.size === 0 && statusElem) {
      statusElem.textContent = '';
      statusElem.className = 'telemetry-status';
    }
  }

  // Update specific buttons
  if (action === 'ir') {
    ['btn-ir-auto', 'btn-ir-off'].forEach(id => {
      const btn = document.getElementById(id);
      if (btn) btn.disabled = updating;
    });
  } else if (ONOFF_KEYS[action]) {
    const unsupported = action === 'light' && !getCameraCapabilities((selectedCameraObject() || {}).model || '').spotlight;
    ['on', 'off'].forEach(v => {
      const btn = document.getElementById(`btn-${action}-${v}`);
      if (btn) btn.disabled = updating || unsupported;
    });
  }
}

/**
 * Polls fresh reported shadow document from GET /api/v1/cameras/{id}/shadow.
 */
async function pollShadowTelemetry(camId, isBackground = false) {
  try {
    const res = await apiRequest(`/api/v1/cameras/${encodeURIComponent(camId)}/shadow`);
    if (!res.ok) return null;

    const doc = await res.json();
    const reported = (doc.state && doc.state.reported) ? doc.state.reported : doc;
    if (camId !== state.selectedCameraId) return reported;
    const controlLabel = document.getElementById('local-control-status');
    if (controlLabel) {
      const labels = { broker_unavailable: 'Local service unavailable', waiting_for_camera: 'Local: waiting for camera', camera_report_received: 'Local: camera report received', camera_report_stale: 'Local: camera report stale' };
      controlLabel.textContent = doc.control ? (labels[doc.control.status] || 'Local: unconfirmed') : 'Controls via Osaio';
      controlLabel.title = doc.control ? 'Local test mode. Cloud fallback is disabled. This does not verify firewall blocking.' : '';
    }

    state.shadowTelemetry[camId] = reported;
    renderShadowTelemetry(reported);
    return reported;
  } catch (err) {
    if (!isBackground) console.warn('[shadow] pollShadowTelemetry error:', err);
    return null;
  }
}

// Camera IrLedMode values (see pkg/bridge/ir_mode.go): 0 = infrared off,
// 1 or 2 = automatic (the camera's light sensor decides). There is no
// "force on": in a lit room the camera stays in color whatever is chosen.
const IR_MODE_NAMES = { 0: 'off', 1: 'auto', 2: 'auto' };
const IR_HINTS = {
  auto: 'Auto: infrared switches on by itself when the room gets dark.',
  off: 'Off: infrared stays off, so the picture goes dark at night.',
};

function selectedCameraObject() {
  return (state.enrolledCameras || []).find(c => (c.uuid || c.id) === state.selectedCameraId);
}

function renderShadowTelemetry(reported) {
  if (!reported) return;

  const cam = selectedCameraObject();
  const caps = getCameraCapabilities(cam ? cam.model : '');

  // Night vision: the camera's reported mode is highlighted
  if (reported.IrLedMode === undefined) {
    ['auto','off'].forEach(m => document.getElementById(`btn-ir-${m}`)?.classList.remove('active'));
  }
  if (reported.IrLedMode !== undefined) {
    const mode = IR_MODE_NAMES[reported.IrLedMode] || 'auto';
    ['auto', 'off'].forEach(m => {
      const btn = document.getElementById(`btn-ir-${m}`);
      if (btn) btn.classList.toggle('active', m === mode);
    });
    const hint = document.getElementById('ir-hint');
    if (hint) hint.textContent = IR_HINTS[mode];
  }

  // On/Off selectors (0=off, 1=on): the camera's reported state is
  // highlighted; nothing is highlighted until the camera has reported.
  Object.entries(ONOFF_KEYS).forEach(([action, key]) => {
    const known = reported[key] === 0 || reported[key] === 1;
    const on = reported[key] === 1;
    const shown = known && (action !== 'light' || caps.spotlight);
    ['on', 'off'].forEach(v => {
      const btn = document.getElementById(`btn-${action}-${v}`);
      if (btn) btn.classList.toggle('active', shown && (v === 'on') === on);
    });
  });
}

// Shadow keys behind the On/Off selectors.
const ONOFF_KEYS = { led: 'LedOnOff', motion: 'MotionDetectSW', sound: 'SoundDetectSW', light: 'LightSW' };

// handleOnOffChoice sends the chosen state (not a flip), so a stale readback
// can never turn a switch the wrong way.
function handleOnOffChoice(action, value) {
  if (action === 'light') {
    const caps = getCameraCapabilities((selectedCameraObject() || {}).model || '');
    if (!caps.spotlight) {
      showToast('This camera model has no white spotlight.', 'info');
      return;
    }
  }
  sendAuxControl(action, value);
}

// Talkback ADTS AAC Packaging & Push-to-Talk Handlers
// ============================================================================

/**
 * Builds standard 7-byte MPEG-4 AAC-LC ADTS header (16kHz mono, no CRC).
 * Syncword: 0xFFF (12 bits)
 * ID: 0 (MPEG-4), Layer: 00, Protection Absent: 1 (7-byte header, no CRC)
 * Profile: 1 (AAC-LC)
 * Sampling Frequency Index: 8 (16000 Hz)
 * Channel Config: 1 (mono)
 * Frame Length: 7 + payloadLength (13 bits)
 */
function createADTSHeader(payloadLength) {
  const frameLength = 7 + payloadLength;
  const header = new Uint8Array(7);
  header[0] = 0xFF; // Syncword [11:4]
  header[1] = 0xF1; // Syncword [3:0]=0xF, ID=0 (MPEG-4), Layer=00, ProtAbsent=1 (no CRC)
  header[2] = 0x60; // Profile=1 (AAC-LC) << 6 | srIdx=8 (16kHz) << 2 | private=0 | channel MSB=0
  header[3] = 0x40 | ((frameLength >> 11) & 0x03); // channel LSBs=01 (mono) << 6 | frameLen [12:11]
  header[4] = (frameLength >> 3) & 0xFF;           // frameLen [10:3]
  header[5] = ((frameLength & 0x07) << 5) | 0x1F;  // frameLen [2:0] << 5 | buffer fullness MSBs (0x1F)
  header[6] = 0xFC;                                // buffer fullness LSBs (0x3F) << 2 | num frames - 1 (0)
  return header;
}

// Canonical 4-byte MPEG-4 AAC-LC silence payload (SCE with max_sfb=0 + END tag)
const AAC_SILENCE_PAYLOAD = new Uint8Array([0x01, 0x18, 0x20, 0x07]);

/**
 * Builds a valid 11-byte 16kHz mono AAC-LC ADTS silence frame for fallback streaming.
 */
function createFallbackADTSFrame() {
  const hdr = createADTSHeader(AAC_SILENCE_PAYLOAD.length);
  const frame = new Uint8Array(7 + AAC_SILENCE_PAYLOAD.length);
  frame.set(hdr, 0);
  frame.set(AAC_SILENCE_PAYLOAD, 7);
  return frame;
}

/**
 * Streams raw binary ADTS chunk to gateway, inspecting response ok.
 * Reverts status pill to "Standby" and stops talkback if gateway rejects chunk.
 */
async function deliverTalkbackChunk(camId, chunkData, mimeType = 'audio/l16;rate=16000;channels=1', seq = state.talkSeq) {
  const current = () => state.isTalkActive && seq === state.talkSeq && camId === state.talkCameraId;
  if (!current()) return;
  try {
    const blob = (chunkData instanceof Blob) ? chunkData : new Blob([chunkData], { type: mimeType });
    const res = await apiRequest(`/api/v1/cameras/${encodeURIComponent(camId)}/talk/stream`, 'POST', blob, { signal: state.talkAbortController && state.talkAbortController.signal });
    if (!current()) return;
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      if (!current()) return;
      console.warn(`[talk] audio rejected with HTTP ${res.status}`, err);
      showToast(`Talk: ${err.error || 'audio rejected by the gateway'}`, 'error');
      await stopTalkback();
      return;
    }
  } catch (err) {
    if (!current()) return;
    console.warn('[talk] error delivering chunk:', err);
    await stopTalkback();
  }
}

// Talkback chunks are sent strictly in order, one request at a time, with
// whatever accumulated while the previous request was in flight merged into
// the next one. (Firing one fetch per 64 ms AAC frame let requests overtake
// each other, which scrambled the audio at the camera.)
const talkQueue = { items: [], sending: false };

function enqueueTalkChunk(camId, data, mimeType) {
  if (!state.isTalkActive) return;
  const bytes = data instanceof Uint8Array ? data : new Uint8Array(data);
  talkQueue.items.push({ camId, bytes, mimeType, seq: state.talkSeq });
  pumpTalkQueue();
}

async function pumpTalkQueue() {
  if (talkQueue.sending) return;
  talkQueue.sending = true;
  try {
    while (talkQueue.items.length > 0 && state.isTalkActive) {
      const first = talkQueue.items[0];
      if (first.seq !== state.talkSeq || first.camId !== state.talkCameraId) {
        talkQueue.items.shift();
        continue;
      }
      let n = 0;
      let total = 0;
      while (n < talkQueue.items.length && talkQueue.items[n].mimeType === first.mimeType && talkQueue.items[n].camId === first.camId && talkQueue.items[n].seq === first.seq) {
        total += talkQueue.items[n].bytes.byteLength;
        n++;
      }
      const batch = talkQueue.items.splice(0, n);
      const merged = new Uint8Array(total);
      let offset = 0;
      for (const item of batch) {
        merged.set(item.bytes, offset);
        offset += item.bytes.byteLength;
      }
      await deliverTalkbackChunk(first.camId, merged, first.mimeType, first.seq);
    }
  } finally {
    talkQueue.sending = false;
  }
}

function updateTalkLevel(rms) {
  const wrap = document.getElementById('talk-level');
  const bar = document.getElementById('talk-level-bar');
  if (!wrap || !bar) return;
  wrap.classList.toggle('visible', state.isTalkActive);
  bar.style.width = `${Math.min(100, Math.round(rms * 400))}%`;
}

function setTalkUI(status) {
  const btn = document.getElementById('btn-talkback');
  const pill = document.getElementById('talk-status-val');
  const talking = status === 'talking' || status === 'starting';
  if (btn) {
    btn.classList.toggle('active', talking);
    btn.textContent = talking ? '■ Stop talking' : '🎙 Talk';
  }
  if (pill) pill.textContent = { starting: 'Starting...', talking: 'Talking', off: 'Off' }[status] || 'Off';
  if (!talking) updateTalkLevel(0);
}

// Talkback: the microphone is captured at the device's own rate, downsampled
// to 16 kHz mono PCM here, and encoded to the camera's AAC format by the
// gateway. (Browsers don't reliably offer a 16 kHz AAC encoder, and the camera
// may use another rate.)
async function startTalkback() {
  if (!state.selectedCameraId || state.isTalkActive || state.talkStarting || state.talkStopping) return;
  const camId = state.selectedCameraId;
  const seq = (state.talkSeq = (state.talkSeq || 0) + 1);
  const cancelled = () => seq !== state.talkSeq || camId !== state.selectedCameraId;
  state.talkCameraId = camId;
  state.talkStartAttempted = false;
  state.talkAbortController = new AbortController();
  state.talkStarting = true;
  setTalkUI('starting');

  try {
    if (!navigator.mediaDevices || typeof navigator.mediaDevices.getUserMedia !== 'function') {
      throw new Error('this browser cannot use the microphone on this page');
    }
    const stream = await navigator.mediaDevices.getUserMedia({
      audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true }
    });
    if (cancelled()) {
      stream.getTracks().forEach(t => t.stop());
      return;
    }
    state.talkStream = stream;

    state.talkStartAttempted = true;
    const startRequest = apiRequest(`/api/v1/cameras/${encodeURIComponent(camId)}/talk/start`, 'POST');
    state.talkStartRequest = startRequest;
    let res;
    try { res = await startRequest; } finally {
      if (state.talkStartRequest === startRequest) state.talkStartRequest = null;
    }
    if (cancelled()) return;
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      throw new Error(err.error || `the gateway refused (HTTP ${res.status})`);
    }
    await res.json().catch(() => ({}));
    if (cancelled()) return;

    const AudioCtx = window.AudioContext || window.webkitAudioContext;
    const ctx = new AudioCtx();
    state.talkAudioCtx = ctx;
    if (ctx.state === 'suspended') await ctx.resume();
    if (cancelled()) return;
    const source = ctx.createMediaStreamSource(stream);
    const proc = ctx.createScriptProcessor(4096, 1, 1);
    state.talkProcessor = proc;
    const mute = ctx.createGain();
    mute.gain.value = 0; // keep the graph running without playing the mic back

    const ratio = ctx.sampleRate / 16000;
    let phase = 0; // position of the next output sample in the current buffer
    proc.onaudioprocess = (e) => {
      if (!state.isTalkActive || cancelled()) return;
      const input = e.inputBuffer.getChannelData(0);
      let energy = 0;
      for (let i = 0; i < input.length; i++) energy += input[i] * input[i];
      updateTalkLevel(Math.sqrt(energy / input.length));

      const out = [];
      while (phase < input.length) {
        // average the input window that maps onto this output sample (cheap low-pass)
        const from = Math.max(0, Math.floor(phase - ratio / 2));
        const to = Math.min(input.length, Math.max(from + 1, Math.floor(phase + ratio / 2)));
        let acc = 0;
        for (let j = from; j < to; j++) acc += input[j];
        const v = Math.max(-1, Math.min(1, acc / (to - from)));
        out.push(v < 0 ? v * 0x8000 : v * 0x7FFF);
        phase += ratio;
      }
      phase -= input.length;
      enqueueTalkChunk(camId, Int16Array.from(out).buffer, 'audio/l16;rate=16000;channels=1');
    };
    source.connect(proc);
    proc.connect(mute);
    mute.connect(ctx.destination);

    state.isTalkActive = true;
    setTalkUI('talking');
  } catch (err) {
    if (!cancelled()) {
      showToast(`Talk: ${err.message}`, 'error');
      await stopTalkback();
    }
  } finally {
    if (!cancelled()) state.talkStarting = false;
  }
}

async function stopTalkback() {
  if (state.talkStopping) return state.talkStopping;
  const camId = state.talkCameraId;
  const pendingStart = state.talkStartRequest;
  const stopRemote = state.isTalkActive || state.talkStartAttempted;
  state.talkSeq = (state.talkSeq || 0) + 1;
  state.isTalkActive = false;
  state.talkStarting = false;
  state.talkCameraId = null;
  state.talkStartAttempted = false;
  talkQueue.items = [];
  if (state.talkAbortController) state.talkAbortController.abort();
  state.talkAbortController = null;
  setTalkUI('off');

  if (state.talkProcessor) {
    try { state.talkProcessor.disconnect(); } catch (_) {}
    state.talkProcessor = null;
  }
  if (state.talkAudioCtx) {
    try { state.talkAudioCtx.close(); } catch (_) {}
    state.talkAudioCtx = null;
  }
  if (state.talkStream) {
    try { state.talkStream.getTracks().forEach(track => track.stop()); } catch (_) {}
    state.talkStream = null;
  }
  // Observe a start already sent to the gateway before stopping that camera.
  // A new Talk session cannot begin until this ordering is complete.
  const stopping = (async () => {
    if (pendingStart) await pendingStart.catch(() => {});
    if (stopRemote && camId) {
      try { await apiRequest(`/api/v1/cameras/${encodeURIComponent(camId)}/talk/stop`, 'POST'); }
      catch (err) { console.warn('[talk] stop error:', err); }
    }
  })();
  state.talkStopping = stopping;
  try { await stopping; } finally {
    if (state.talkStopping === stopping) state.talkStopping = null;
  }
}

// ============================================================================
// 8. Block cloud video: "Block all cameras" plus a switch per camera
// ============================================================================
//
// The rules live on the router the cameras connect to. "Connect router" asks
// for the router's password once (used for that connection, never saved);
// BombeCam installs its own key there, restricted to turning camera blocking
// on and off, so the switches take effect right away. See "How it works".

const PRIVACY_CHIPS = {
  blocked: ['privacy-chip-ok', 'Blocked on router'],
  not_blocked: ['privacy-chip-muted', 'Not blocked'],
  waiting_for_mac: ['privacy-chip-warn', 'Waiting for its network address'],
  pending: ['privacy-chip-busy', 'Updating router...'],
  not_applied: ['privacy-chip-warn', 'Not applied: connect router'],
};

async function refreshPrivacyStatus() {
  if (state.privacyBusy) return;
  try {
    const res = await apiRequest('/api/v1/privacy');
    if (!res.ok) {
      renderPrivacyBadge(null, true);
      return;
    }
    const data = await res.json();
    state.privacy = data;
    renderPrivacy(data);
  } catch (err) {
    renderPrivacyBadge(null, true);
  }
}

function renderPrivacyBadge(data, failed) {
  let text = 'Firewall: not blocked';
  let short = 'Not blocked';
  let cls = 'badge-unverified';
  if (failed) {
    text = 'Firewall: status unavailable';
    short = 'Status unavailable';
    cls = 'badge-error';
  } else if (!data || !data.camera_count) {
    text = 'Firewall: no cameras yet';
    short = 'No cameras';
    cls = 'badge-secondary';
  } else {
    // Count what the router actually blocks, not just what was chosen.
    const inForce = (data.cameras || []).filter(c => c.state === 'blocked').length;
    if (!data.in_sync) {
      text = 'Firewall: not applied yet';
      short = 'Not applied yet';
      cls = 'badge-warning';
    } else if (inForce === data.camera_count) {
      text = 'Firewall: all cameras blocked';
      short = 'All blocked';
      cls = 'badge-verified';
    } else if (inForce > 0) {
      text = `Firewall: ${inForce} of ${data.camera_count} blocked`;
      short = `${inForce} of ${data.camera_count} blocked`;
      cls = 'badge-verified';
    }
  }
  const header = document.getElementById('privacy-badge');
  if (header) {
    header.textContent = text;
    header.className = `badge badge-button ${cls}`;
  }
  const badge = document.getElementById('privacy-state-badge');
  if (badge) {
    badge.textContent = short;
    badge.className = `badge ${cls}`;
  }
}

function stripSSHPort(addr) {
  return String(addr || '').replace(/:22$/, '');
}

function renderPrivacy(data) {
  renderPrivacyBadge(data, false);
  const headline = document.getElementById('privacy-headline');
  if (headline && data.headline) headline.textContent = data.headline;
  const router = data.router || {};

  // Router row
  const status = document.getElementById('privacy-router-status');
  if (status) {
    if (router.connected) {
      const region = (data.region || 'us').toUpperCase();
      status.innerHTML = `Connected: <strong>${escapeHtml(stripSSHPort(router.address))}</strong>` +
        (router.firewall ? ` <span class="text-muted">(OpenWrt ${escapeHtml(router.firewall)}, ${escapeHtml(region)} region)</span>` : ` <span class="text-muted">(${escapeHtml(region)} region)</span>`);
    } else if (router.known) {
      status.innerHTML = `Not connected <span class="text-muted">(last used ${escapeHtml(stripSSHPort(router.address))})</span>`;
    } else {
      status.textContent = 'Not connected';
    }
  }
  const connectBtn = document.getElementById('btn-privacy-connect');
  if (connectBtn) {
    connectBtn.style.display = router.connected && !router.script_outdated ? 'none' : '';
    connectBtn.textContent = router.connected ? 'Update router' : (router.known ? 'Reconnect router' : 'Connect router');
    connectBtn.disabled = Boolean(state.privacyBusy);
  }
  const checkBtn = document.getElementById('btn-privacy-check');
  if (checkBtn) {
    checkBtn.style.display = router.connected ? '' : 'none';
    checkBtn.disabled = Boolean(state.privacyBusy);
  }
  const disconnectBtn = document.getElementById('btn-privacy-disconnect');
  if (disconnectBtn) {
    disconnectBtn.style.display = router.connected ? '' : 'none';
    disconnectBtn.disabled = Boolean(state.privacyBusy);
  }

  // One notice at a time, most important first.
  const alert = document.getElementById('privacy-alert');
  if (alert) {
    let kind = '';
    let html = '';
    const err = data.last_error;
    if (err && !data.in_sync) {
      kind = 'alert-danger';
      html = escapeHtml(err.message || 'The router could not be updated.');
      if (err.error === 'router_password_needed' || err.error === 'router_not_connected') {
        html += '<br><button type="button" class="btn btn-primary btn-sm" data-privacy-connect>Connect router</button>';
      }
    } else if (!data.in_sync && !router.connected) {
      kind = 'alert-info';
      html = 'Your choices are saved but not in force yet. Connect your router to apply them.' +
        '<br><button type="button" class="btn btn-primary btn-sm" data-privacy-connect>Connect router</button>';
    } else if (router.script_outdated) {
      kind = 'alert-info';
      html = `The router has an older BombeCam router script (${escapeHtml(router.script_version)}). ` +
        'Press Update router and enter the router password to install the new one.';
    } else if (data.not_ready_reason) {
      kind = 'alert-info';
      html = escapeHtml(data.not_ready_reason);
    }
    alert.style.display = kind ? '' : 'none';
    alert.className = `alert ${kind}`;
    alert.innerHTML = html;
    alert.querySelectorAll('[data-privacy-connect]').forEach(b => b.addEventListener('click', () => openRouterConnect()));
  }

  // Master switch
  const master = document.getElementById('privacy-master');
  if (master) {
    master.checked = data.master === 'all';
    master.indeterminate = data.master === 'some';
    master.disabled = !data.camera_count || Boolean(state.privacyBusy);
    master.setAttribute('aria-checked', data.master === 'all' ? 'true' : (data.master === 'some' ? 'mixed' : 'false'));
  }
  const hint = document.getElementById('privacy-master-hint');
  if (hint) {
    hint.textContent = data.block_new_cameras
      ? 'New cameras are blocked automatically while this is on.'
      : 'Blocks every camera, including cameras you add later.';
  }

  // Cameras
  const list = document.getElementById('privacy-camera-list');
  if (list) {
    list.innerHTML = '';
    const cams = data.cameras || [];
    if (cams.length === 0) {
      list.innerHTML = '<li class="text-muted">No cameras connected yet.</li>';
    }
    cams.forEach(cam => {
      const li = document.createElement('li');
      const busy = state.privacyBusy === cam.id || state.privacyBusy === 'all';
      const [chipCls, chipText] = busy ? PRIVACY_CHIPS.pending : (PRIVACY_CHIPS[cam.state] || PRIVACY_CHIPS.not_blocked);
      const title = cam.state === 'waiting_for_mac'
        ? ' title="BombeCam learns a camera\'s network address when it streams to this PC; open its live view once."' : '';
      li.innerHTML =
        `<div class="privacy-cam-text"><span class="privacy-cam-name">${escapeHtml(cam.name || cam.id)}</span>` +
        `<code class="privacy-cam-mac">${escapeHtml(cam.mac || 'network address not known yet')}</code></div>` +
        `<div class="privacy-cam-row-end"><span class="privacy-chip ${chipCls}"${title}>${escapeHtml(chipText)}</span></div>`;
      const sw = document.createElement('input');
      sw.type = 'checkbox';
      sw.className = 'privacy-switch';
      sw.setAttribute('role', 'switch');
      sw.setAttribute('aria-label', `Block cloud video for ${cam.name || cam.id}`);
      sw.checked = Boolean(cam.blocked);
      sw.disabled = Boolean(state.privacyBusy);
      sw.addEventListener('change', () => setCameraBlocked(cam.id, sw.checked));
      li.querySelector('.privacy-cam-row-end').appendChild(sw);
      list.appendChild(li);
    });
  }

  const manual = document.getElementById('privacy-manual-command');
  if (manual && data.manual_command) manual.textContent = data.manual_command;
  const setupLine = document.getElementById('how-stream-setup');
  if (setupLine) setupLine.style.display = data.block_stream_setup ? 'none' : '';

  const hostKey = document.getElementById('privacy-host-key');
  if (hostKey) {
    hostKey.textContent = router.host_key
      ? `Router identity remembered for ${stripSSHPort(router.address)}: ${router.host_key}`
      : 'BombeCam remembers the router\'s identity (SSH host key) the first time it connects, and refuses to send the password if it changes.';
  }
  const forget = document.getElementById('btn-privacy-forget-router');
  if (forget) forget.style.display = router.known ? '' : 'none';
}

function showPrivacyResult(kind, message, warnings, output) {
  const box = document.getElementById('privacy-result');
  if (box) {
    box.style.display = message ? '' : 'none';
    const cls = { ok: 'alert-success', busy: 'alert-info', info: 'alert-info' }[kind] || 'alert-danger';
    box.className = `privacy-result alert ${cls}`;
    let html = escapeHtml(message || '');
    if (warnings && warnings.length) {
      html += '<ul class="privacy-warnings">' + warnings.map(w => `<li>${escapeHtml(w)}</li>`).join('') + '</ul>';
    }
    box.innerHTML = html;
  }
  const out = document.getElementById('privacy-output');
  const outText = document.getElementById('privacy-output-text');
  if (out && outText) {
    out.style.display = output ? '' : 'none';
    outText.textContent = output || '';
  }
}

// sendPrivacyChange posts a camera or Block-all change; the gateway saves it
// and, with a connected router, applies it at once.
async function sendPrivacyChange(path, body, busyKey, doneText) {
  state.privacyBusy = busyKey;
  if (state.privacy) renderPrivacy(state.privacy);
  try {
    const res = await apiRequest(path, 'POST', body);
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      showPrivacyResult('error', data.message || `Change failed (HTTP ${res.status}).`);
      return;
    }
    const r = data.router || {};
    if (r.ok) {
      if (r.changed) {
        showPrivacyResult('ok', doneText, r.warnings, r.output);
      } else {
        showPrivacyResult('', '');
      }
    } else if (r.error === 'router_not_connected' || r.error === 'router_password_needed') {
      showPrivacyResult('info', r.message || 'Saved. Connect your router to apply it.');
      openRouterConnect(r.error === 'router_password_needed'
        ? 'The router no longer accepts BombeCam\'s key, probably because it was reset. Enter its password to connect it again.'
        : '');
    } else {
      showPrivacyResult('error', r.message || 'The router could not be updated.', r.warnings, r.output);
    }
    if (data.status) state.privacy = data.status;
  } catch (err) {
    showPrivacyResult('error', `Couldn't reach BombeCam: ${err.message}`);
  } finally {
    state.privacyBusy = '';
    if (state.privacy) renderPrivacy(state.privacy);
  }
}

// Turning blocking on clears the camera's open connections on the router, so
// none opened before the block can carry on past it. Every action that turns
// blocking on asks first: the switches, Block all, and Connect/Update router
// (which applies switches that are already on).
function clearConnectionsQuestion(whose) {
  return `Block cloud video? This will temporarily clear ${whose} open connections on the router.`;
}

// A cancelled switch is put back.
function confirmClearConnections(whose) {
  if (confirm(clearConnectionsQuestion(whose))) return true;
  if (state.privacy) renderPrivacy(state.privacy);
  return false;
}

// camerasToBlock are the cameras a router update would block now.
function camerasToBlock() {
  return ((state.privacy || {}).cameras || []).filter(c => c.blocked && c.mac);
}

function setCameraBlocked(id, blocked) {
  const cam = ((state.privacy || {}).cameras || []).find(c => c.id === id);
  const name = cam ? cam.name : 'the camera';
  if (blocked && !confirmClearConnections(cam ? `${name}'s` : "the camera's")) return Promise.resolve();
  return sendPrivacyChange('/api/v1/privacy/camera', { camera_id: id, blocked }, id,
    blocked ? `Cloud video blocked for ${name}.` : `${name} works normally again.`);
}

function setAllBlocked(blocked) {
  if (blocked && !confirmClearConnections("every camera's")) return Promise.resolve();
  return sendPrivacyChange('/api/v1/privacy/block-all', { blocked }, 'all',
    blocked ? 'Cloud video blocked for every camera.' : 'Every camera works normally again.');
}

function openRouterConnect(reason) {
  const data = state.privacy || {};
  const router = data.router || {};
  const desc = document.getElementById('privacy-modal-desc');
  if (desc) {
    desc.textContent = (reason ? reason + ' ' : '') +
      'BombeCam connects to your router over SSH once, installs its router script and a key that can only turn camera blocking on and off, then applies your choices. The password is used for this one connection and isn\'t saved.';
  }
  // The address to try: the router in use, else the one last typed here,
  // else BombeCam's suggestion (the router the cameras are on).
  const addr = document.getElementById('privacy-router-address');
  const hint = document.getElementById('privacy-router-address-hint');
  let typed = '';
  try { typed = localStorage.getItem('bombecam.routerAddress') || ''; } catch (_) {}
  let source = '';
  if (addr && !addr.value) {
    if (router.address) {
      addr.value = stripSSHPort(router.address);
      source = 'current';
    } else if (typed) {
      addr.value = typed;
      source = 'typed';
    } else {
      addr.value = data.suggested_router_address || '192.168.8.1';
      source = data.suggested_router_source || 'guess';
    }
  }
  if (hint) {
    const why = {
      current: 'The router BombeCam is set up with.',
      typed: 'The address you entered last time.',
      last: 'The router BombeCam last connected to.',
      cameras: 'Suggested: the router your cameras are on. Change it if your cameras connect to a different router.',
      default_gateway: 'Suggested: this PC\'s router. If your cameras are on a different router (for example a travel router), enter that one\'s address.',
      guess: 'A guess. Enter the address of the router your cameras connect to (GL.iNet routers use 192.168.8.1 unless you changed it).',
    };
    if (source) hint.textContent = why[source] || why.guess;
  }
  const regionEl = document.getElementById('privacy-region');
  if (regionEl) regionEl.value = data.region === 'eu' ? 'eu' : 'us';
  const user = document.getElementById('privacy-router-user');
  if (user && router.user) user.value = router.user;
  const pw = document.getElementById('privacy-router-password');
  if (pw) pw.value = '';
  openModal('modal-privacy-apply');
  setTimeout(() => pw && pw.focus(), 50);
}

async function handleRouterConnect(e) {
  if (e && e.preventDefault) e.preventDefault();
  const addrEl = document.getElementById('privacy-router-address');
  const pwEl = document.getElementById('privacy-router-password');
  const userEl = document.getElementById('privacy-router-user');
  const address = addrEl ? addrEl.value.trim() : '';
  if (!address) {
    showToast('Enter the router\'s address (for example 192.168.8.1).', 'warning');
    return;
  }
  try { localStorage.setItem('bombecam.routerAddress', stripSSHPort(address)); } catch (_) {}
  const toBlock = camerasToBlock();
  if (toBlock.length && !confirm(clearConnectionsQuestion(toBlock.length === 1
    ? `${toBlock[0].name || 'the camera'}'s` : "the blocked cameras'"))) return;
  const btn = document.getElementById('btn-privacy-confirm');
  if (btn) {
    btn.disabled = true;
    btn.textContent = 'Connecting...';
  }
  state.privacyBusy = 'router';
  showPrivacyResult('busy', 'Connecting to the router. This usually takes a few seconds (up to a minute if the router is slow to look up names).');
  try {
    const regionEl = document.getElementById('privacy-region');
    const res = await apiRequest('/api/v1/privacy/router/connect', 'POST', {
      router_address: address,
      router_user: userEl ? userEl.value.trim() : 'root',
      router_password: pwEl ? pwEl.value : '',
      region: regionEl ? regionEl.value : '',
    });
    const data = await res.json().catch(() => ({}));
    if (res.ok) {
      if (pwEl) pwEl.value = '';
      closeModal('modal-privacy-apply');
      showPrivacyResult('ok', `Router connected${data.firewall ? ` (OpenWrt ${data.firewall})` : ''}. Your choices are in force, and the switches now apply right away.`,
        data.warnings, data.output);
      showToast('Router connected.', 'success');
    } else if (['router_auth', 'invalid_router_address', 'invalid_router_user'].includes(data.error)) {
      // Keep the dialog open so the password or address can be fixed.
      showToast(data.message || 'The router rejected the login.', 'error');
      showPrivacyResult('error', data.message || 'The router rejected the login.');
      if (pwEl) pwEl.select();
    } else {
      if (pwEl) pwEl.value = '';
      closeModal('modal-privacy-apply');
      let msg = data.message || `Connecting failed (HTTP ${res.status}).`;
      if (data.error === 'router_unreachable') {
        msg += ' If this isn\'t a GL.iNet or OpenWrt router, see Other ways to block camera traffic.';
      }
      showPrivacyResult('error', msg, data.warnings, data.output);
      showToast('Connecting the router failed.', 'error');
    }
  } catch (err) {
    showPrivacyResult('error', `Couldn't reach BombeCam: ${err.message}`);
  } finally {
    state.privacyBusy = '';
    if (btn) {
      btn.disabled = false;
      btn.textContent = 'Connect';
    }
    await refreshPrivacyStatus();
  }
}

// handleRouterCheck asks the router for its own report: which cameras it
// blocks and whether any BombeCam rule is left in its firewall.
async function handleRouterCheck() {
  state.privacyBusy = 'router';
  showPrivacyResult('busy', 'Asking the router...');
  try {
    const res = await apiRequest('/api/v1/privacy/router/check', 'POST', {});
    const data = await res.json().catch(() => ({}));
    const r = data.result || {};
    const output = (data.router && data.router.output) || '';
    if (res.ok) {
      const n = r.cameras || '0';
      let msg = n === '0' ? 'The router blocks no camera' : `The router blocks ${n} camera(s)`;
      // While blocking, the router reports whether its rules are loaded;
      // with nothing blocked, whether any BombeCam rule is left over. Only a
      // script from before that check leaves both out.
      if (r.block === 'yes' && r.loaded !== undefined) msg += r.loaded === 'yes' ? ', and its rules are loaded in the firewall.' : ', but its rules are not loaded in the firewall. Restart the router, then Check router again.';
      else if (r.block === 'yes') msg += '.';
      else if (r.residue !== undefined) msg += r.residue === '0' ? ', and no BombeCam rule is left in its firewall.' : `, but ${r.residue} BombeCam rule(s) are left in its firewall.`;
      else msg += '. (Its script is too old to check the firewall itself: press Update router.)';
      showPrivacyResult('ok', msg, data.router && data.router.warnings, output);
    } else {
      showPrivacyResult('error', data.message || 'The router could not be checked.', null, output);
    }
  } catch (err) {
    showPrivacyResult('error', `Couldn't reach BombeCam: ${err.message}`);
  } finally {
    state.privacyBusy = '';
    await refreshPrivacyStatus();
  }
}

async function handleRouterDisconnect() {
  if (!confirm('Remove BombeCam from the router? Its rules and its key are deleted there, and every camera works normally again.')) return;
  state.privacyBusy = 'router';
  showPrivacyResult('busy', 'Removing BombeCam from the router...');
  try {
    const res = await apiRequest('/api/v1/privacy/router/disconnect', 'POST', {});
    const data = await res.json().catch(() => ({}));
    if (res.ok) {
      showPrivacyResult('ok', 'BombeCam was removed from the router. Every camera works normally.');
      showToast('Router disconnected.', 'info');
    } else {
      showPrivacyResult('error', (data.message || 'The router could not be updated.') +
        ' If the router was reset or replaced, use Forget this router under Connect router > Advanced.');
    }
  } catch (err) {
    showPrivacyResult('error', `Couldn't reach BombeCam: ${err.message}`);
  } finally {
    state.privacyBusy = '';
    await refreshPrivacyStatus();
  }
}

async function handlePrivacyForgetRouter() {
  if (!confirm('Forget this router\'s address, identity and BombeCam\'s key? Use this after resetting or replacing the router. Nothing on the router is changed, and your per-camera choices are kept for the next router.')) return;
  try {
    const res = await apiRequest('/api/v1/privacy/forget-router', 'POST');
    if (res.ok) {
      const addr = document.getElementById('privacy-router-address');
      if (addr) addr.value = '';
      showToast('Router forgotten.', 'info');
      await refreshPrivacyStatus();
    } else {
      showToast('Could not forget the router.', 'error');
    }
  } catch (err) {
    showToast(`Error: ${err.message}`, 'error');
  }
}

// Stop / start all cameras (the router is never touched).
function renderStopButton(stopped) {
  state.camerasStopped = Boolean(stopped);
  const btn = document.getElementById('btn-stop-cameras');
  if (!btn || btn.dataset.busy === '1') return;
  btn.textContent = stopped ? '▶ Start cameras' : '⛔ Stop cameras';
  btn.className = stopped ? 'btn btn-success btn-sm' : 'btn btn-secondary btn-sm';
}

async function handleStopGateway() {
  const btn = document.getElementById('btn-stop-cameras');
  const starting = Boolean(state.camerasStopped);
  if (btn) {
    btn.dataset.busy = '1';
    btn.disabled = true;
    btn.textContent = starting ? 'Starting...' : 'Stopping...';
  }
  try {
    if (!starting && state.isTalkActive) {
      await stopTalkback();
    }
    const res = await apiRequest(starting ? '/api/v1/gateway/start' : '/api/v1/gateway/stop', 'POST');
    if (res.ok) {
      if (!starting) teardownVideo();
      showToast(starting ? 'Cameras starting...' : 'Cameras stopped.', starting ? 'success' : 'warning');
      if (btn) btn.dataset.busy = '';
      renderStopButton(!starting);
      viewerTick();
    } else {
      showToast(starting ? 'Could not start the cameras.' : 'Could not stop the cameras.', 'error');
    }
  } catch (err) {
    showToast(`Error: ${err.message}`, 'error');
  } finally {
    if (btn) {
      btn.dataset.busy = '';
      btn.disabled = false;
      renderStopButton(state.camerasStopped);
    }
  }
}

// ============================================================================
// 9. NVR Integration Snippets & 1-Click Clipboard Copy
// ============================================================================

async function loadNVRSnippets() {
  const btn = document.getElementById('btn-refresh-nvr');
  if (btn) {
    btn.disabled = true;
    btn.textContent = 'Loading...';
  }

  try {
    const [frigateRes, haRes, setRes] = await Promise.all([
      apiRequest('/api/v1/integrations/frigate'),
      apiRequest('/api/v1/integrations/homeassistant'),
      apiRequest('/api/v1/integrations/settings')
    ]);

    if (frigateRes.ok) {
      const fData = await frigateRes.json();
      document.getElementById('snippet-frigate').textContent = fData.config_yaml || '# No Frigate configuration available';
    }
    if (haRes.ok) {
      const haData = await haRes.json();
      document.getElementById('snippet-ha').textContent = haData.setup_steps || 'No cameras yet.';
    }
    if (setRes.ok) {
      const s = await setRes.json();
      renderPorts(s);
      renderStreamList(s.cameras || []);
      const note = document.getElementById('nvr-address-note');
      if (note) {
        let text = `Addresses use ${s.advertised_address}, this PC's address on your network.`;
        if ((s.addresses || []).length > 1) text += ' This PC is on more than one network: pick the right one on the Frigate / Home Assistant page.';
        if (s.address_source === 'loopback') text = 'This PC has no network address, so other devices cannot open the streams yet.';
        if (s.network && s.network.public) text += ' Windows treats this network as Public, which blocks other devices: see the Frigate / Home Assistant page.';
        note.textContent = text;
      }
    }
  } catch (err) {
    console.warn('[nvr] loadNVRSnippets error:', err);
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = '🔄 Refresh';
    }
  }
}

// Ports on this PC (Streams & ports tab).
function renderPorts(s) {
  const p = s.ports || {};
  const set = (id, v) => { const el = document.getElementById(id); if (el && v) el.textContent = v; };
  set('port-tag-rtsp', p.rtsp);
  set('port-tag-hls', p.hls);
  set('port-tag-webrtc', p.webrtc);
  set('port-tag-webrtc-udp', p.webrtc_udp);
  set('port-tag-snapshot', p.snapshot);
  const udp = s.rtsp_udp_ports || [];
  if (udp.length === 2) set('port-tag-rtsp-udp', `${udp[0]}\u2013${udp[1]}`);
  const snap = document.getElementById('port-item-snapshot');
  if (snap) snap.hidden = !s.snapshots;
  set('port-tag-web', window.location.port || (window.location.protocol === 'https:' ? '443' : '80'));
}

// One row per camera with its RTSP and HLS addresses and copy buttons.
function renderStreamList(cameras) {
  const list = document.getElementById('stream-list');
  if (!list) return;
  if (!cameras.length) {
    list.innerHTML = '<div class="text-muted">No cameras yet. Press Add cameras to set one up.</div>';
    return;
  }
  const urlRow = (kind, url, what) => url ? `
      <div class="stream-url">
        <span class="stream-url-kind">${kind}</span>
        <code class="stream-url-value" title="${escapeHtml(url)}">${escapeHtml(url)}</code>
        <button class="btn btn-secondary btn-sm btn-copy" data-copy="${escapeHtml(url)}" title="Copy the ${what} address">📋 Copy</button>
      </div>` : '';
  list.innerHTML = cameras.map(cam => `
    <div class="stream-row">
      <div class="stream-row-head">
        <span class="status-dot ${cam.streaming ? 'on' : ''}"></span>
        <span class="stream-row-name">${escapeHtml(cam.name || cam.id)}</span>
        <span class="stream-row-state">${cam.streaming ? 'Streaming' : 'Not streaming right now'}</span>
      </div>
      ${urlRow('RTSP', cam.rtsp_url, 'RTSP')}
      ${urlRow('HLS', cam.hls_url, 'HLS')}
      ${cam.rtsp_url_by_id && cam.rtsp_url_by_id !== cam.rtsp_url ? `<div class="control-hint stream-row-alt">Also by camera ID: <code>${escapeHtml(cam.rtsp_url_by_id)}</code></div>` : ''}
    </div>`).join('');
}

// Shut down (Settings): BombeCam and its video server stop; the router's
// Firewall rules stay where they are.
// Settings: Start with Windows (Windows only) and, in Docker, no Shut down
// button (the container would just restart).
async function refreshGatewayInfo() {
  let info = null;
  try {
    const res = await apiRequest('/api/v1/gateway/info');
    if (res.ok) info = await res.json();
  } catch (_) {}
  if (!info) return;
  renderAutostart(info.autostart || {});
  const shutdownBtn = document.getElementById('btn-shutdown');
  const shutdownHint = document.getElementById('shutdown-hint');
  if (info.docker && shutdownBtn && shutdownHint) {
    shutdownBtn.style.display = 'none';
    shutdownHint.textContent = 'BombeCam runs in Docker here and Docker starts it again after it closes. To stop it, run docker compose stop in its folder.';
  }
}

function renderAutostart(a) {
  const row = document.getElementById('manage-autostart');
  if (!row) return;
  row.style.display = a.supported ? '' : 'none';
  if (!a.supported) return;
  const on = !!a.enabled && !!a.this_copy;
  document.getElementById('btn-autostart-on')?.classList.toggle('active', on);
  document.getElementById('btn-autostart-off')?.classList.toggle('active', !on);
  const hint = document.getElementById('autostart-hint');
  if (hint) {
    hint.textContent = a.enabled && !a.this_copy
      ? `Windows starts another copy of BombeCam at sign-in${a.other_command ? ` (${a.other_command})` : ''}. Press On to start this copy instead.`
      : 'Starts BombeCam in the background when you sign in, so streams and recordings keep going after a restart. It shows as an icon by the clock.';
  }
}

async function setAutostart(enabled) {
  try {
    const res = await apiRequest('/api/v1/gateway/autostart', 'POST', { enabled });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
    renderAutostart(data);
    showToast(enabled ? 'BombeCam will start when you sign in to Windows.' : 'BombeCam will no longer start with Windows.', 'success');
  } catch (err) {
    showToast(`Start with Windows: ${err.message}`, 'error');
  }
}

async function handleShutdown() {
  if (!confirm('Shut down BombeCam? Streams stop until you open BombeCam again. Firewall rules stay on your router and keep working.')) return;
  const btn = document.getElementById('btn-shutdown');
  if (btn) { btn.disabled = true; btn.textContent = 'Shutting down...'; }
  try {
    if (state.isTalkActive) await stopTalkback();
  } catch (_) {}
  let ok = false;
  try {
    const res = await apiRequest('/api/v1/gateway/shutdown', 'POST');
    ok = res.ok;
  } catch (_) {}
  if (!ok) {
    showToast('Could not shut down BombeCam.', 'error');
    if (btn) { btn.disabled = false; btn.textContent = '⏻ Shut down'; }
    return;
  }
  (state.timers || []).forEach(clearInterval);
  state.timers = [];
  teardownVideo();
  showShutdownScreen();
  closeTabIfAllowed();
}

// closeTabIfAllowed closes this tab after Shut down. Browsers only allow it
// for a tab that has shown just this page (as when BombeCam opened it); in
// any other tab nothing happens and the shutdown screen stays.
function closeTabIfAllowed() {
  setTimeout(() => {
    try { window.close(); } catch (_) {}
  }, 1200);
}

function showShutdownScreen(message) {
  // A different title, so the tray icon never reuses this tab later.
  document.title = 'BombeCam has shut down';
  document.body.innerHTML = `
    <main class="shutdown-screen">
      <div class="shutdown-card">
        <div class="shutdown-icon" aria-hidden="true">⏻</div>
        <h1>BombeCam has shut down</h1>
        <p>${escapeHtml(message || 'Streams have stopped. Firewall rules stay on your router and keep working.')}</p>
        <p class="text-muted">To start again, open <strong>bombecam.exe</strong> in the BombeCam folder. This tab closes by itself if your browser allows it; otherwise you can close it.</p>
      </div>
    </main>`;
}

async function copyToClipboard(text, btnElement) {
  if (!text) {
    showToast('Nothing to copy.', 'warning');
    return;
  }

  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
    } else {
      const ta = document.createElement('textarea');
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      document.execCommand('copy');
      document.body.removeChild(ta);
    }

    if (btnElement) {
      const orig = btnElement.textContent;
      btnElement.textContent = '✓ Copied!';
      btnElement.classList.add('btn-copied');
      setTimeout(() => {
        btnElement.textContent = orig;
        btnElement.classList.remove('btn-copied');
      }, 2000);
    }
    showToast('Copied to clipboard!', 'success');
  } catch (err) {
    showToast('Failed to copy to clipboard.', 'error');
  }
}

// ============================================================================
// 10. Modals, Toasts & DOM Event Listeners
// ============================================================================

function openModal(modalId) {
  const m = document.getElementById(modalId);
  if (m) m.classList.add('active');
}

function closeModal(modalId) {
  const m = document.getElementById(modalId);
  if (m) m.classList.remove('active');
}

function showToast(message, type = 'info') {
  const container = document.getElementById('toast-container');
  if (!container) return;

  const toast = document.createElement('div');
  toast.className = `toast toast-${type}`;
  toast.textContent = message;

  container.appendChild(toast);
  setTimeout(() => {
    toast.remove();
  }, 4000);
}

function sleep(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}

function escapeHtml(str) {
  if (!str) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#039;');
}

function setupEventListeners() {
  // Sign-in
  document.getElementById('form-login')?.addEventListener('submit', handleLoginGo);
  document.getElementById('btn-save-server-key')?.addEventListener('click', handleSaveServerKeyPanel);
  document.getElementById('input-server-key')?.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); handleSaveServerKeyPanel(); }
  });
  document.getElementById('btn-open-server-key')?.addEventListener('click', openServerKeyModal);
  document.getElementById('btn-submit-server-key')?.addEventListener('click', handleSubmitServerKeyModal);
  document.getElementById('btn-reset-server-key')?.addEventListener('click', handleResetServerKey);
  document.getElementById('btn-save-app-id')?.addEventListener('click', handleSaveAppIdPanel);
  document.getElementById('input-app-id')?.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); handleSaveAppIdPanel(); }
  });
  document.getElementById('btn-open-app-id')?.addEventListener('click', openAppIdModal);
  document.getElementById('btn-submit-app-id')?.addEventListener('click', handleSubmitAppIdModal);
  document.getElementById('btn-reset-app-id')?.addEventListener('click', handleResetAppId);
  document.getElementById('btn-login-go')?.addEventListener('click', (e) => {
    if (!document.getElementById('form-login')) {
      handleLoginGo(e);
    }
  });

  // Screen 2: Discovery Toolbar & Enrollment
  document.getElementById('chk-discovery-select-all')?.addEventListener('change', e => {
    const isChecked = e.target.checked;
    document.querySelectorAll('.chk-discovery-item').forEach(cb => {
      cb.checked = isChecked;
      const id = cb.getAttribute('data-id');
      if (isChecked) {
        state.selectedDiscoveryIds.add(id);
      } else {
        state.selectedDiscoveryIds.delete(id);
      }
    });
    updateDiscoveryActionState();
  });
  document.getElementById('btn-add-to-bombecam')?.addEventListener('click', handleAddToBombeCam);

  // Account forms & buttons
  document.getElementById('form-account-setup')?.addEventListener('submit', handleSetupSubmit);
  document.getElementById('btn-reset-profile')?.addEventListener('click', handleResetProfile);
  document.getElementById('btn-login-cancel')?.addEventListener('click', () => {
    checkOnboardingStatus();
  });
  document.getElementById('privacy-badge')?.addEventListener('click', () => switchTab('tab-safety'));
  document.getElementById('btn-sound')?.addEventListener('click', toggleSound);
  document.getElementById('volume-slider')?.addEventListener('input', e => setVolume(Number(e.target.value) / 100));
  try {
    const saved = parseFloat(localStorage.getItem('bombecam.volume'));
    if (!Number.isNaN(saved)) state.volume = Math.max(0, Math.min(1, saved));
  } catch (_) {}
  applySoundState();
  document.getElementById('btn-fullscreen')?.addEventListener('click', () => {
    const wrap = document.getElementById('video-wrapper');
    if (!wrap) return;
    if (document.fullscreenElement) document.exitFullscreen();
    else if (wrap.requestFullscreen) wrap.requestFullscreen();
  });

  // Discovery (legacy table)
  document.getElementById('btn-refresh-discovery')?.addEventListener('click', () => loadDiscoveredCameras(true));
  document.getElementById('btn-enroll-selected')?.addEventListener('click', handleEnrollSelected);
  document.getElementById('chk-select-all')?.addEventListener('change', e => {
    document.querySelectorAll('.chk-cam-select').forEach(cb => {
      cb.checked = e.target.checked;
    });
    updateSelectedDiscoveryCount();
  });

  // Camera & Stream controls
  document.getElementById('stream-camera-select')?.addEventListener('change', e => {
    selectActiveCamera(e.target.value);
  });
  document.getElementById('btn-overlay-retry')?.addEventListener('click', async e => {
    const camId = state.selectedCameraId;
    if (!camId) return;
    e.currentTarget.disabled = true;
    try {
      const res = await apiRequest(`/api/v1/cameras/${encodeURIComponent(camId)}/unlock`, 'POST');
      if (res.ok) setOverlay('Retrying the camera connection...');
      else showToast('Retry request failed.', 'error');
    } finally {
      e.currentTarget.disabled = false;
      viewerTick();
    }
  });
  document.getElementById('btn-copy-rtsp')?.addEventListener('click', e => {
    copyToClipboard(document.getElementById('stream-rtsp-url').value, e.currentTarget);
  });
  document.getElementById('btn-copy-hls')?.addEventListener('click', e => {
    copyToClipboard(document.getElementById('stream-hls-url').value, e.currentTarget);
  });

  // Digital Zoom
  document.getElementById('btn-zoom-in')?.addEventListener('click', () => applyZoom(0.25));
  document.getElementById('btn-zoom-out')?.addEventListener('click', () => applyZoom(-0.25));
  document.getElementById('btn-zoom-reset')?.addEventListener('click', () => applyZoom(0, true));

  // PTZ D-pad
  document.querySelectorAll('.ptz-dir, .ptz-stop').forEach(btn => {
    btn.addEventListener('click', () => {
      const dir = parseInt(btn.getAttribute('data-dir'), 10);
      sendPTZ(dir);
    });
  });

  // Auxiliary Switches
  document.getElementById('btn-ir-auto')?.addEventListener('click', () => sendAuxControl('ir', 'auto'));
  document.getElementById('btn-ir-off')?.addEventListener('click', () => sendAuxControl('ir', 'off'));
  Object.keys(ONOFF_KEYS).forEach(action => ['on', 'off'].forEach(v => {
    document.getElementById(`btn-${action}-${v}`)?.addEventListener('click', () => handleOnOffChoice(action, v));
  }));

  // Talkback Push-to-Talk
  // Talk: click to start, click again to stop; or hold while speaking and
  // release to stop.
  const talkBtn = document.getElementById('btn-talkback');
  if (talkBtn) {
    let pressedAt = 0;
    talkBtn.addEventListener('pointerdown', e => {
      e.preventDefault();
      if (state.isTalkActive || state.talkStarting) {
        pressedAt = 0;
        stopTalkback();
      } else {
        pressedAt = Date.now();
        startTalkback();
      }
    });
    talkBtn.addEventListener('pointerup', () => {
      if (pressedAt && Date.now() - pressedAt > 600) stopTalkback(); // held: walkie-talkie mode
      pressedAt = 0;
    });
  }

  // Block cloud video (Block all + per-camera switches, router connect) and Stop
  document.getElementById('privacy-master')?.addEventListener('change', (e) => setAllBlocked(e.target.checked));
  document.getElementById('btn-privacy-connect')?.addEventListener('click', () => openRouterConnect());
  document.getElementById('btn-privacy-disconnect')?.addEventListener('click', handleRouterDisconnect);
  document.getElementById('btn-privacy-check')?.addEventListener('click', handleRouterCheck);
  document.getElementById('form-privacy-apply')?.addEventListener('submit', handleRouterConnect);
  document.getElementById('btn-privacy-forget-router')?.addEventListener('click', handlePrivacyForgetRouter);
  document.getElementById('btn-stop-cameras')?.addEventListener('click', handleStopGateway);
  document.getElementById('btn-shutdown')?.addEventListener('click', handleShutdown);
  document.getElementById('btn-autostart-on')?.addEventListener('click', () => setAutostart(true));
  document.getElementById('btn-autostart-off')?.addEventListener('click', () => setAutostart(false));
  document.getElementById('stream-list')?.addEventListener('click', e => {
    const btn = e.target.closest('[data-copy]');
    if (btn) copyToClipboard(btn.getAttribute('data-copy'), btn);
  });

  // NVR Snippets
  document.getElementById('btn-refresh-nvr')?.addEventListener('click', loadNVRSnippets);
  document.getElementById('btn-copy-frigate')?.addEventListener('click', e => {
    copyToClipboard(document.getElementById('snippet-frigate').textContent, e.currentTarget);
  });
  document.getElementById('btn-copy-ha')?.addEventListener('click', e => {
    copyToClipboard(document.getElementById('snippet-ha').textContent, e.currentTarget);
  });

  // Modal Closers
  document.querySelectorAll('[data-close]').forEach(btn => {
    btn.addEventListener('click', () => {
      const modalId = btn.getAttribute('data-close');
      closeModal(modalId);
    });
  });
}

// Kickoff SPA
if (typeof document !== 'undefined' && document.addEventListener) {
  document.addEventListener('DOMContentLoaded', initApp);
}

// Expose ADTS, talkback, and auto-detect helpers for inspection and testing environments
if (typeof window !== 'undefined') {
  window.createADTSHeader = createADTSHeader;
  window.createFallbackADTSFrame = createFallbackADTSFrame;
  window.deliverTalkbackChunk = deliverTalkbackChunk;
  window.startTalkback = startTalkback;
  window.stopTalkback = stopTalkback;
  window.AAC_SILENCE_PAYLOAD = AAC_SILENCE_PAYLOAD;
  window.detectTimezone = detectTimezone;
  window.detectTimezoneOffset = detectTimezoneOffset;
  window.inferCountryCode = inferCountryCode;
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    createADTSHeader,
    createFallbackADTSFrame,
    deliverTalkbackChunk,
    startTalkback,
    stopTalkback,
    AAC_SILENCE_PAYLOAD,
    detectTimezone,
    detectTimezoneOffset,
    inferCountryCode,
  };
}

/* =====================================================================
 * Cameras, Osaio logins, Add cameras and Settings
 * BombeCam has no account of its own: it keeps a pool of Osaio logins,
 * stored encrypted on this computer, and uses them only to find and reach
 * cameras. Cameras from different logins sit side by side; a login can be
 * added, given a new password, or removed with its cameras.
 * ===================================================================== */

function setListStatus(id, msg, kind) {
  const el = document.getElementById(id);
  if (!el) return;
  el.textContent = msg || '';
  el.className = 'list-status' + (msg && kind ? ` is-${kind}` : '');
}

function plural(n, word) {
  return `${n} ${word}${n === 1 ? '' : 's'}`;
}

// After cameras or logins change: refresh every view that shows them, without
// sending the user back through the first-run screens.
async function afterCameraChange() {
  try { await loadEnrolledCameras(); } catch (_) {}
  try { await refreshOnboardingSnapshot(); } catch (_) {}
  await refreshAccounts();
  if (state.activeTab === 'tab-cameras') await renderCamerasTab();
  if (state.activeTab === 'tab-logins') renderLoginsTab();
  if (state.activeTab === 'tab-streams') loadNVRSnippets();
}

// ---------------------------------------------------------------------------
// Cameras tab: one row per camera, filterable, so any number of cameras fits.
// ---------------------------------------------------------------------------

const camerasView = { filter: '', rows: [], live: {} };

async function renderCamerasTab() {
  const list = document.getElementById('cameras-list');
  if (!list) return;
  try {
    const [profRes, streamsRes] = await Promise.all([
      apiRequest('/api/v1/onboarding/profile'),
      apiRequest('/api/v1/integrations/streams'),
    ]);
    const prof = profRes.ok ? await profRes.json() : {};
    const streams = streamsRes.ok ? await streamsRes.json() : {};
    if (profRes.ok) state.profile = prof;
    camerasView.rows = Object.values(prof.cameras || {})
      .sort((x, y) => String(x.name || x.uuid).localeCompare(String(y.name || y.uuid), undefined, { numeric: true }));
    camerasView.live = {};
    (streams.streams || []).forEach(c => { camerasView.live[c.uuid] = c; });
  } catch (err) {
    setListStatus('cameras-status', 'Could not read the camera list from BombeCam.', 'error');
  }
  drawCamerasList();
}

function drawCamerasList() {
  const list = document.getElementById('cameras-list');
  const count = document.getElementById('cameras-count');
  const toolbar = document.getElementById('cameras-toolbar');
  if (!list) return;
  const all = camerasView.rows;
  const q = camerasView.filter.trim().toLowerCase();
  const rows = q ? all.filter(c => [c.name, c.uuid, c.model, c.account_email, c.stream_name]
    .some(v => String(v || '').toLowerCase().includes(q))) : all;
  if (toolbar) toolbar.hidden = all.length < 5;
  if (count) count.textContent = q ? `${rows.length} of ${plural(all.length, 'camera')}` : plural(all.length, 'camera');

  list.innerHTML = '';
  if (!all.length) {
    list.innerHTML = '<div class="item-empty">No cameras yet. Press <strong>Add cameras</strong> to bring in the cameras an Osaio login can see.</div>';
    return;
  }
  if (!rows.length) {
    list.innerHTML = '<div class="item-empty">No camera matches that search.</div>';
    return;
  }
  const multiLogin = new Set(all.map(c => c.account_email || '')).size > 1;
  rows.forEach(cam => {
    const id = cam.uuid;
    const live = camerasView.live[id] || {};
    const row = document.createElement('div');
    row.className = 'item-row camera-row';
    const meta = [cam.model, multiLogin || !cam.account_email ? cam.account_email : '', live.ip || cam.ip_address]
      .filter(Boolean).map(escapeHtml).join(' · ');
    row.innerHTML =
      `<div class="item-main">` +
        `<div class="item-title">${escapeHtml(cam.name || id)}` +
          ` <span class="item-pill ${live.streaming ? 'is-ok' : ''}">${live.streaming ? 'Streaming' : 'Not streaming'}</span></div>` +
        `<div class="item-meta">${meta || escapeHtml(id)}</div>` +
      `</div>` +
      `<div class="item-field">` +
        `<label for="stream-name-${escapeHtml(id)}">Stream name</label>` +
        `<div class="inline-input">` +
          `<input type="text" class="form-control code-input" id="stream-name-${escapeHtml(id)}" value="${escapeHtml(cam.stream_name || '')}" spellcheck="false" maxlength="40" autocomplete="off" data-bwignore="true" data-1p-ignore="true" data-lpignore="true">` +
          `<button type="button" class="btn btn-secondary btn-sm" data-act="rename">Save</button>` +
        `</div>` +
      `</div>` +
      `<div class="item-actions">` +
        `<button type="button" class="btn btn-secondary btn-sm" data-act="view">View</button>` +
        `<button type="button" class="btn btn-danger btn-sm" data-act="remove">Remove</button>` +
      `</div>`;
    const input = row.querySelector('input');
    row.querySelector('[data-act="rename"]').addEventListener('click', () => renameStream(id, input.value.trim(), cam.stream_name || ''));
    input.addEventListener('keydown', e => {
      if (e.key === 'Enter') { e.preventDefault(); renameStream(id, input.value.trim(), cam.stream_name || ''); }
    });
    row.querySelector('[data-act="view"]').addEventListener('click', () => viewCamera(id));
    row.querySelector('[data-act="remove"]').addEventListener('click', () => removeCamera(id, cam.name || id));
    list.appendChild(row);
  });
}

async function viewCamera(id) {
  const select = document.getElementById('stream-camera-select');
  if (select) select.value = id;
  switchTab('tab-dashboard');
  await selectActiveCamera(id);
}

async function renameStream(id, name, oldName) {
  if (!name || name === oldName) return;
  if (oldName && !confirm(`Change the stream name from "${oldName}" to "${name}"? Frigate and Home Assistant setups that use rtsp://…/${oldName} need the new address (the camera-ID address keeps working).`)) return;
  try {
    const res = await apiRequest(`/api/v1/cameras/${encodeURIComponent(id)}/stream-name`, 'POST', { stream_name: name });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) { setListStatus('cameras-status', (data && (data.message || data.error)) || 'Could not rename.', 'error'); return; }
    setListStatus('cameras-status', `Stream name saved: ${data.rtsp_url || name}`, 'ok');
    await renderCamerasTab();
    try { await loadNVRSnippets(); } catch (_) {}
  } catch (err) {
    setListStatus('cameras-status', 'Could not rename (BombeCam unreachable).', 'error');
  }
}

async function removeCamera(id, name) {
  if (!confirm(`Remove ${name} from BombeCam? Its local stream stops and it is forgotten. You can add it again later.`)) return;
  setListStatus('cameras-status', `Removing ${name}...`);
  try {
    const res = await apiRequest(`/api/v1/cameras/${encodeURIComponent(id)}`, 'DELETE');
    const data = await res.json().catch(() => ({}));
    if (!res.ok) { setListStatus('cameras-status', (data && data.error) || 'Remove failed.', 'error'); return; }
    setListStatus('cameras-status', `${name} removed. ${data.firewall_note || ''}`.trim(), data.still_on_router ? 'warn' : 'ok');
    await afterCameraChange();
  } catch (err) {
    setListStatus('cameras-status', 'Remove failed (BombeCam unreachable).', 'error');
  }
}

// ---------------------------------------------------------------------------
// Osaio logins tab
// ---------------------------------------------------------------------------

const LOGIN_STATUS = {
  connected: ['Connected', 'is-ok'],
  connecting: ['Connecting...', ''],
  needs_password: ['Needs its password', 'is-error'],
  server_key: ['Server key needed', 'is-warn'],
  app_id: ['App ID needed', 'is-warn'],
  unreachable: ['Osaio unreachable', 'is-warn'],
  not_connected: ['Not connected', 'is-warn'],
};

function renderLoginsTab() {
  const list = document.getElementById('logins-list');
  if (!list) return;
  const accounts = state.accounts;
  if (!accounts) { list.innerHTML = '<div class="text-muted">Loading...</div>'; return; }
  list.innerHTML = '';
  if (!accounts.length) {
    list.innerHTML = '<div class="item-empty">No Osaio logins are stored. Press <strong>Add a login</strong>.</div>';
    return;
  }
  accounts.forEach(acct => {
    const [label, cls] = LOGIN_STATUS[acct.status] || ['Not connected', 'is-warn'];
    const needsPw = acct.status === 'needs_password' || acct.status === 'not_connected';
    const row = document.createElement('div');
    row.className = 'item-row login-row';
    const meta = [plural(acct.cameras, 'camera'), acct.country ? `country code +${acct.country}` : ''].filter(Boolean).join(' · ');
    row.innerHTML =
      `<div class="item-main">` +
        `<div class="item-title">${escapeHtml(acct.email)} <span class="item-pill ${cls}">${escapeHtml(label)}</span></div>` +
        `<div class="item-meta">${escapeHtml(meta)}</div>` +
        (acct.status !== 'connected' && acct.detail ? `<div class="item-detail">${escapeHtml(acct.detail)}</div>` : '') +
      `</div>` +
      `<div class="item-actions">` +
        (acct.status === 'connected' ? `<button type="button" class="btn btn-secondary btn-sm" data-act="cameras">Add its cameras</button>` : '') +
        `<button type="button" class="btn ${needsPw ? 'btn-primary' : 'btn-secondary'} btn-sm" data-act="password">Update password</button>` +
        `<button type="button" class="btn btn-danger btn-sm" data-act="remove">Remove</button>` +
      `</div>`;
    row.querySelector('[data-act="cameras"]')?.addEventListener('click', () => openAddCameras(acct.email));
    row.querySelector('[data-act="password"]').addEventListener('click', () => openLoginPassword(acct.email));
    row.querySelector('[data-act="remove"]').addEventListener('click', () => removeLogin(acct));
    list.appendChild(row);
  });
}

async function removeLogin(acct) {
  const last = (state.accounts || []).length <= 1;
  let msg = `Remove ${acct.email} from BombeCam?\n\nBombeCam forgets this login` +
    (acct.cameras ? ` and the ${plural(acct.cameras, 'camera')} added through it.` : '.') +
    ' Other logins and their cameras stay. The login itself is not changed at Osaio.';
  if (last) msg += '\n\nThis is the last login, so BombeCam goes back to its sign-in page. Firewall and Frigate / Home Assistant settings are kept.';
  if (!confirm(msg)) return;
  setListStatus('logins-status', `Removing ${acct.email}...`);
  try {
    const res = await apiRequest('/api/v1/accounts/remove', 'POST', { email: acct.email });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) { setListStatus('logins-status', data.message || 'Remove failed.', 'error'); return; }
    setListStatus('logins-status', `${acct.email} removed. ${data.firewall_note || ''}`.trim(), 'ok');
    if (!data.logins_left) {
      await checkOnboardingStatus();
      return;
    }
    await afterCameraChange();
  } catch (err) {
    setListStatus('logins-status', 'Remove failed (BombeCam unreachable).', 'error');
  }
}

function openLoginPassword(email) {
  const emailEl = document.getElementById('login-password-email');
  const pw = document.getElementById('login-password-input');
  const country = document.getElementById('login-password-country');
  if (emailEl) emailEl.value = email;
  if (pw) pw.value = '';
  if (country) { country.value = suggestedAccountCountry(email); delete country.dataset.countryEdited; }
  setListStatus('login-password-status', '');
  openModal('modal-login-password');
  setTimeout(() => pw && pw.focus(), 50);
}

// signInLogin adds a login to the pool or saves its new password. Osaio
// checks the password first, so a typo never disturbs a working login.
async function signInLogin(email, password, countryValue) {
  const country = normalizedCountryInput(countryValue);
  if (!country) return { ok: false, message: 'Enter the country calling code for this Osaio login.' };
  try {
    const res = await apiRequest('/api/v1/accounts/signin', 'POST', { email, password, country });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      if (data.error === 'server_key_missing') renderServerKeyPanel({ configured: false, editable: true, source: '' });
      if (data.error === 'app_id_missing') renderAppIdPanel({ configured: false, editable: true, source: '' });
      return { ok: false, message: data.message || mapSetupError(res.status, data) };
    }
    await refreshAccounts();
    return { ok: true, email: data.email || email, added: data.added };
  } catch (err) {
    return { ok: false, message: 'Could not reach BombeCam. Is it running?' };
  }
}

async function handleLoginPasswordSubmit(e) {
  if (e && e.preventDefault) e.preventDefault();
  const email = document.getElementById('login-password-email')?.value || '';
  const password = document.getElementById('login-password-input')?.value || '';
  if (!password) { setListStatus('login-password-status', 'Enter the password.', 'error'); return; }
  const btn = document.getElementById('btn-login-password-save');
  if (btn) { btn.disabled = true; btn.textContent = 'Checking...'; }
  setListStatus('login-password-status', 'Checking the password with Osaio...');
  const r = await signInLogin(email, password, document.getElementById('login-password-country')?.value);
  if (btn) { btn.disabled = false; btn.textContent = 'Check and save'; }
  if (!r.ok) { setListStatus('login-password-status', r.message, 'error'); return; }
  document.getElementById('login-password-input').value = '';
  closeModal('modal-login-password');
  showToast(`Password saved for ${email}.`, 'success');
  setListStatus('logins-status', `${email} is connected again. Its cameras reconnect by themselves.`, 'ok');
  await afterCameraChange();
}

// ---------------------------------------------------------------------------
// Add cameras: from a stored login, or a new one that joins the pool.
// ---------------------------------------------------------------------------

const NEW_LOGIN = '__new__';
const addCams = { email: '', results: [], selected: new Set() };

function addCamsNewLogin() {
  return (document.getElementById('add-cam-login')?.value || NEW_LOGIN) === NEW_LOGIN;
}

function updateAddCamsForm() {
  const fresh = addCamsNewLogin();
  const fields = document.getElementById('add-cam-new-login');
  if (fields) fields.hidden = !fresh;
  const btn = document.getElementById('btn-search-cameras');
  if (btn) btn.textContent = fresh ? '🔍 Sign in and search for cameras' : '🔍 Search for cameras';
}

// openAddCameras opens the dialog; preselect is a stored login's email,
// NEW_LOGIN for a new one, or empty for the first signed-in login.
async function openAddCameras(preselect) {
  if (!state.accounts) await refreshAccounts();
  const accounts = state.accounts || [];
  const select = document.getElementById('add-cam-login');
  if (select) {
    select.innerHTML = '';
    accounts.forEach(a => {
      const opt = document.createElement('option');
      opt.value = a.email;
      opt.textContent = a.email + (a.status === 'connected' ? '' : ` (${(LOGIN_STATUS[a.status] || ['not connected'])[0].toLowerCase()})`);
      select.appendChild(opt);
    });
    const opt = document.createElement('option');
    opt.value = NEW_LOGIN;
    opt.textContent = 'Another Osaio login...';
    select.appendChild(opt);
    const firstConnected = accounts.find(a => a.status === 'connected');
    select.value = (preselect && [...select.options].some(o => o.value === preselect)) ? preselect
      : (firstConnected ? firstConnected.email : NEW_LOGIN);
  }
  const group = document.getElementById('add-cam-login-group');
  if (group) group.hidden = accounts.length === 0;
  const emailEl = document.getElementById('add-cam-email');
  const passEl = document.getElementById('add-cam-password');
  if (emailEl) emailEl.value = '';
  if (passEl) passEl.value = '';
  const countryInput = document.getElementById('add-cam-country');
  if (countryInput) { countryInput.value = inferCountryCode(); delete countryInput.dataset.countryEdited; }
  addCams.email = '';
  addCams.results = [];
  addCams.selected.clear();
  document.getElementById('add-cameras-results').innerHTML = '';
  setListStatus('add-cameras-status', '');
  updateAddCamsForm();
  updateAddSelectedState();
  openModal('modal-add-cameras');
  // A stored, connected login lists its cameras straight away.
  const chosen = accounts.find(a => a.email === select?.value);
  if (chosen && chosen.status === 'connected') runCameraSearch();
}

function updateAddSelectedState() {
  const btn = document.getElementById('btn-add-selected');
  if (btn) btn.disabled = addCams.selected.size === 0;
}

async function runCameraSearch() {
  const btn = document.getElementById('btn-search-cameras');
  const label = btn ? btn.textContent : '';
  let email = document.getElementById('add-cam-login')?.value || NEW_LOGIN;
  if (btn) btn.disabled = true;
  try {
    if (email === NEW_LOGIN) {
      email = (document.getElementById('add-cam-email')?.value || '').trim();
      const password = document.getElementById('add-cam-password')?.value || '';
      if (!email || !password) { setListStatus('add-cameras-status', 'Enter the Osaio email and password.', 'error'); return; }
      if (btn) btn.textContent = 'Signing in...';
      setListStatus('add-cameras-status', 'Signing in to Osaio...');
      const r = await signInLogin(email, password, document.getElementById('add-cam-country')?.value);
      if (!r.ok) { setListStatus('add-cameras-status', r.message, 'error'); return; }
      email = r.email;
      // The login is in the pool now: show it as chosen.
      const select = document.getElementById('add-cam-login');
      if (select && ![...select.options].some(o => o.value === email)) {
        const opt = document.createElement('option');
        opt.value = email;
        opt.textContent = email;
        select.insertBefore(opt, select.lastElementChild);
      }
      if (select) select.value = email;
      document.getElementById('add-cam-login-group').hidden = false;
      document.getElementById('add-cam-password').value = '';
      updateAddCamsForm();
    }
    if (btn) btn.textContent = 'Searching...';
    setListStatus('add-cameras-status', `Looking for the cameras ${email} can see...`);
    const res = await apiRequest('/api/v1/accounts/cameras', 'POST', { email });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      setListStatus('add-cameras-status', data.error === 'not_signed_in'
        ? 'This login isn\'t signed in. Update its password on the Osaio logins tab, then try again.'
        : (data.message || 'Search failed. Try again.'), 'error');
      return;
    }
    addCams.email = data.account_email || email;
    addCams.results = data.cameras || [];
    addCams.selected.clear();
    renderSearchResults();
    const fresh = addCams.results.filter(c => !c.enrolled).length;
    setListStatus('add-cameras-status', !addCams.results.length ? `${addCams.email} can't see any cameras. Share cameras with it in the Osaio app, then search again.`
      : fresh ? `${addCams.email} can see ${plural(addCams.results.length, 'camera')}. Choose the ones to add.`
      : `Every camera ${addCams.email} can see is already added.`);
  } finally {
    if (btn) { btn.disabled = false; if (btn.textContent.endsWith('...')) btn.textContent = label; }
    updateAddCamsForm();
    if (state.activeTab === 'tab-logins') renderLoginsTab();
  }
}

function renderSearchResults() {
  const list = document.getElementById('add-cameras-results');
  if (!list) return;
  list.innerHTML = '';
  addCams.results.forEach(cam => {
    const row = document.createElement('label');
    row.className = 'discovery-item';
    const already = !!cam.enrolled;
    row.innerHTML =
      `<input type="checkbox" value="${escapeHtml(cam.id)}" ${already ? 'checked disabled' : ''}>` +
      `<span class="discovery-name">${cam.online ? '🟢' : '⚪'} ${escapeHtml(cam.name || cam.id)}</span>` +
      `<span class="discovery-model">${escapeHtml(cam.model || '')}</span>` +
      (already ? '<span class="badge badge-info">Added</span>' : '');
    const cb = row.querySelector('input');
    if (!already) {
      cb.addEventListener('change', () => {
        if (cb.checked) addCams.selected.add(cam.id); else addCams.selected.delete(cam.id);
        updateAddSelectedState();
      });
    }
    list.appendChild(row);
  });
  updateAddSelectedState();
}

async function addSelectedCameras() {
  if (addCams.selected.size === 0) return;
  const btn = document.getElementById('btn-add-selected');
  if (btn) { btn.disabled = true; btn.textContent = 'Adding...'; }
  try {
    const res = await apiRequest('/api/v1/cameras/add', 'POST', {
      email: addCams.email,
      camera_ids: Array.from(addCams.selected),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) { setListStatus('add-cameras-status', (data && data.error) || 'Could not add cameras.', 'error'); return; }
    const n = (data.added || []).length;
    addCams.results.forEach(c => { if (addCams.selected.has(c.id)) c.enrolled = true; });
    addCams.selected.clear();
    renderSearchResults();
    setListStatus('add-cameras-status', `Added ${plural(n, 'camera')}. They appear on the Cameras tab and in the viewer.`, 'ok');
    await afterCameraChange();
  } catch (err) {
    setListStatus('add-cameras-status', 'Could not add cameras (BombeCam unreachable).', 'error');
  } finally {
    if (btn) btn.textContent = 'Add selected cameras';
    updateAddSelectedState();
  }
}

// ---------------------------------------------------------------------------
// Settings: server key, app ID, Stop cameras, Start with Windows, Shut down, Delete all
// ---------------------------------------------------------------------------

function openSettings() {
  openModal('modal-manage');
  const st = document.getElementById('manage-status');
  if (st) st.textContent = '';
  refreshGatewayInfo();
}

async function deleteAllStoredInfo() {
  if (!confirm('Delete all stored information? Every Osaio login, camera and setting is removed from this computer, and if a router is connected in the Firewall tab, BombeCam first removes its rules and key from it. Your administrator sign-in stays.')) return;
  const statusEl = document.getElementById('manage-status');
  if (statusEl) statusEl.textContent = 'Deleting stored information...';
  let ok = false;
  try {
    const res = await apiRequest('/api/v1/profile/forget', 'POST');
    ok = res.ok;
    const d = await res.json().catch(() => ({}));
    if (ok) {
      if (d && d.router_updated === false) alert(d.router_message || 'Your router still has BombeCam\'s rules.');
    } else if (statusEl) {
      statusEl.textContent = 'Delete failed: ' + ((d && (d.message || d.error)) || ('HTTP ' + res.status)) + '. Nothing was changed.';
    }
  } catch (err) {
    if (statusEl) statusEl.textContent = 'Delete failed: could not reach BombeCam. Nothing was changed.';
  }
  if (!ok) return; // do not mask a failure by reloading
  state.csrfToken = '';
  state.enrolledCameras = [];
  state.discoveredCameras = [];
  state.onboardingStatus = null;
  location.reload();
}

function setupPoolListeners() {
  document.getElementById('btn-open-add-cameras')?.addEventListener('click', () => openAddCameras());
  document.getElementById('btn-cameras-add')?.addEventListener('click', () => openAddCameras());
  document.getElementById('btn-logins-add')?.addEventListener('click', () => openAddCameras(NEW_LOGIN));
  document.getElementById('btn-open-manage')?.addEventListener('click', openSettings);
  document.getElementById('btn-search-cameras')?.addEventListener('click', runCameraSearch);
  document.getElementById('btn-add-selected')?.addEventListener('click', addSelectedCameras);
  document.getElementById('btn-delete-all')?.addEventListener('click', deleteAllStoredInfo);
  document.getElementById('add-cam-login')?.addEventListener('change', () => {
    addCams.results = [];
    addCams.selected.clear();
    document.getElementById('add-cameras-results').innerHTML = '';
    setListStatus('add-cameras-status', '');
    updateAddCamsForm();
    updateAddSelectedState();
    const a = (state.accounts || []).find(x => x.email === document.getElementById('add-cam-login').value);
    if (a && a.status === 'connected') runCameraSearch();
  });
  document.getElementById('add-cam-password')?.addEventListener('keydown', e => {
    if (e.key === 'Enter') { e.preventDefault(); runCameraSearch(); }
  });
  document.getElementById('form-login-password')?.addEventListener('submit', handleLoginPasswordSubmit);
  document.getElementById('cameras-filter')?.addEventListener('input', e => {
    camerasView.filter = e.target.value;
    drawCamerasList();
  });
  document.getElementById('session-badge')?.addEventListener('click', e => {
    if (document.getElementById('view-workspace')?.style.display === 'none') return;
    switchTab(e.currentTarget.dataset.target || 'tab-cameras');
  });
}

if (typeof document !== 'undefined' && document.addEventListener) {
  document.addEventListener('DOMContentLoaded', setupPoolListeners);
}

/* =====================================================================
 * Administrator: BombeCam's own sign-in (never an Osaio login)
 * Created on this PC at first start. Other devices always sign in; this PC
 * too if the administrator chose so. Optional two-step sign-in with an
 * authenticator app, plus one-time recovery codes.
 * ===================================================================== */

const adminState = { status: null, signedOutShown: false };

async function refreshAdminStatus() {
  try {
    const res = await fetch('/api/v1/admin/status', { credentials: 'same-origin', headers: { Accept: 'application/json' } });
    if (res.ok) adminState.status = await res.json();
  } catch (_) {}
  renderAdminSummary();
  return adminState.status;
}

function showAdminScreen(id) {
  document.body.classList.add('admin-gate');
  const ws = document.getElementById('view-workspace');
  const nav = document.getElementById('main-nav-tabs');
  const ob = document.getElementById('view-onboarding');
  if (ws) ws.style.display = 'none';
  if (nav) nav.style.display = 'none';
  if (ob) ob.style.display = 'flex';
  showOnboardingScreen(id);
}

// adminGate shows the administrator screen the page needs first, and
// reports whether the rest of the page may load.
async function adminGate() {
  const st = await refreshAdminStatus();
  if (!st) return true;
  if (!st.admin_exists) {
    showAdminScreen(st.can_create ? 'screen-admin-create' : 'screen-admin-elsewhere');
    const unreadable = document.getElementById('admin-unreadable');
    if (unreadable) unreadable.hidden = !st.profile_unreadable;
    if (st.can_create) setTimeout(() => document.getElementById('admin-create-password')?.focus(), 50);
    return false;
  }
  if (st.sign_in_required && !st.signed_in) {
    showAdminSignIn();
    return false;
  }
  document.body.classList.remove('admin-gate');
  return true;
}

function showAdminSignIn(message) {
  showAdminScreen('screen-admin-signin');
  const name = document.getElementById('admin-signin-name');
  let remembered = '';
  try { remembered = localStorage.getItem('bombecam.adminName') || ''; } catch (_) {}
  if (name && !name.value) name.value = remembered || 'admin';
  setListStatus('admin-signin-status', message || '', message ? 'warn' : '');
  // Forgot the password: tips first; Delete all only on the PC running
  // BombeCam, where it is always available.
  const thisPC = !!(adminState.status && adminState.status.this_pc);
  const reset = document.getElementById('admin-forgot-reset');
  if (reset) reset.hidden = true;
  const still = document.getElementById('btn-admin-forgot-still');
  if (still) still.hidden = false;
  const here = document.getElementById('admin-forgot-thispc');
  const elsewhere = document.getElementById('admin-forgot-elsewhere');
  if (here) here.hidden = !thisPC;
  const hint = (adminState.status && adminState.status.password_hint) || '';
  const hintItem = document.getElementById('admin-forgot-hint');
  if (hintItem) hintItem.hidden = !hint;
  const hintText = document.getElementById('admin-forgot-hint-text');
  if (hintText) hintText.textContent = hint;
  if (elsewhere) elsewhere.hidden = thisPC;
  setTimeout(() => document.getElementById('admin-signin-password')?.focus(), 50);
}

// handleStartOver: after a forgotten password, delete all saved data on this
// PC (administrator included) and shut BombeCam down.
async function handleStartOver() {
  if (!confirm('Shut down BombeCam and delete all of its saved data on this PC? The administrator, Osaio logins, cameras and settings are removed. This can\'t be undone. Start BombeCam again afterwards to create a new administrator.')) return;
  setListStatus('admin-start-over-status', 'Deleting saved data...');
  try {
    const res = await apiRequest('/api/v1/admin/start-over', 'POST');
    const d = await res.json().catch(() => ({}));
    if (!res.ok) {
      setListStatus('admin-start-over-status', (d.message || 'That didn\'t work') + '. Nothing was changed.', 'error');
      return;
    }
    if (d.router_updated === false) alert(d.router_message || 'Your router still has BombeCam\'s rules.');
    showShutdownScreen('BombeCam deleted its saved data and shut down. Start it again (bombecam.exe) to create a new administrator.');
  } catch (err) {
    setListStatus('admin-start-over-status', 'Could not reach BombeCam. Nothing was changed.', 'error');
  }
}

// handleForgotDeleteAll deletes saved data that can't be opened (lost key,
// damaged file), from the first-start screen.
async function handleForgotDeleteAll(statusId) {
  if (!confirm('Delete BombeCam\'s saved information on this PC? It can\'t be opened, so it can\'t be recovered either.')) return;
  setListStatus(statusId, 'Deleting stored information...');
  try {
    const res = await apiRequest('/api/v1/profile/forget', 'POST');
    const d = await res.json().catch(() => ({}));
    if (!res.ok) {
      setListStatus(statusId, 'Delete failed: ' + (d.message || d.error || ('HTTP ' + res.status)) + '. Nothing was changed.', 'error');
      return;
    }
    if (d.router_updated === false) alert(d.router_message || 'Your router still has BombeCam\'s rules.');
    location.reload();
  } catch (err) {
    setListStatus(statusId, 'Delete failed: could not reach BombeCam. Nothing was changed.', 'error');
  }
}

function onSignedOut() {
  if (adminState.signedOutShown) return;
  adminState.signedOutShown = true;
  (state.timers || []).forEach(clearInterval);
  state.timers = [];
  try { teardownVideo(); } catch (_) {}
  document.querySelectorAll('.modal.active').forEach(m => m.classList.remove('active'));
  showAdminSignIn('You were signed out. Sign in again to continue.');
}

// storePassword offers the password to the browser's password manager when
// it supports that (Chrome, Edge); others offer to save it from the form.
async function storePassword(name, password) {
  try {
    if (window.PasswordCredential && navigator.credentials && navigator.credentials.store) {
      await navigator.credentials.store(new window.PasswordCredential({ id: name, password, name: 'BombeCam' }));
    }
  } catch (_) {}
}

async function handleAdminCreate(e) {
  e.preventDefault();
  const name = (document.getElementById('admin-create-name').value || '').trim() || 'admin';
  const password = document.getElementById('admin-create-password').value;
  const confirmPw = document.getElementById('admin-create-confirm').value;
  const min = (adminState.status && adminState.status.min_password_chars) || 8;
  if (password.length < min) { setListStatus('admin-create-status', `Use at least ${min} characters.`, 'error'); return; }
  if (password !== confirmPw) { setListStatus('admin-create-status', 'The two passwords are different.', 'error'); return; }
  const btn = document.getElementById('btn-admin-create');
  if (btn) btn.disabled = true;
  const res = await apiRequest('/api/v1/admin/setup', 'POST', {
    name, password, require_local_sign_in: document.getElementById('admin-create-local').checked,
    hint: document.getElementById('admin-create-hint').value,
  });
  const data = await res.json().catch(() => ({}));
  if (btn) btn.disabled = false;
  if (!res.ok) { setListStatus('admin-create-status', data.message || 'Could not create the administrator.', 'error'); return; }
  try { localStorage.setItem('bombecam.adminName', name); } catch (_) {}
  await storePassword(name, password);
  location.reload();
}

async function handleAdminSignIn(e) {
  e.preventDefault();
  const name = (document.getElementById('admin-signin-name').value || '').trim();
  const password = document.getElementById('admin-signin-password').value;
  const code = (document.getElementById('admin-signin-code').value || '').trim();
  const remember = document.getElementById('admin-signin-remember').checked;
  const btn = document.getElementById('btn-admin-signin');
  if (btn) btn.disabled = true;
  const res = await apiRequest('/api/v1/admin/signin', 'POST', { name, password, code });
  const data = await res.json().catch(() => ({}));
  if (btn) btn.disabled = false;
  if (res.ok) {
    if (remember) {
      try { localStorage.setItem('bombecam.adminName', name); } catch (_) {}
      await storePassword(name, password);
    }
    if (data.used_recovery_code) {
      alert(`You used a recovery code. ${data.recovery_codes_left} left. Make new ones in Settings > Sign-in settings if you are running low.`);
    }
    location.reload();
    return;
  }
  if (data.error === 'code_required') {
    document.getElementById('admin-signin-code-group').hidden = false;
    setListStatus('admin-signin-status', data.message, 'warn');
    document.getElementById('admin-signin-code').focus();
    return;
  }
  setListStatus('admin-signin-status', data.message || 'Sign-in failed.', 'error');
  if (data.error === 'wrong_code') {
    const c = document.getElementById('admin-signin-code');
    c.value = '';
    c.focus();
  }
}

async function handleAdminSignOut() {
  await apiRequest('/api/v1/admin/signout', 'POST');
  location.reload();
}

function renderAdminSummary() {
  const st = adminState.status || {};
  const hint = document.getElementById('manage-admin-hint');
  const out = document.getElementById('btn-admin-signout');
  if (hint) {
    hint.textContent = !st.admin_exists ? 'Not set up.'
      : `${st.name || 'Administrator'} · two-step sign-in ${st.two_step ? 'on' : 'off'} · ${st.require_local_sign_in ? 'this PC asks for the sign-in' : 'this PC opens without sign-in'}`;
  }
  if (out) out.hidden = !st.signed_in;
}

function adminCsrf(data) {
  if (data && data.csrf_token) state.csrfToken = data.csrf_token;
}

async function openAdminModal() {
  closeModal('modal-manage');
  openModal('modal-admin');
  await refreshAdminStatus();
  const st = adminState.status || {};
  document.getElementById('admin-summary').textContent = `Signed in as ${st.name || 'administrator'}. Phones, tablets and other computers always have to sign in.`;
  document.getElementById('admin-pw-name').value = st.name || '';
  document.getElementById('admin-local-require').checked = !!st.require_local_sign_in;
  ['admin-hint-text', 'admin-hint-password', 'admin-pw-current', 'admin-pw-new', 'admin-pw-confirm', 'admin-local-password', 'admin-2fa-start-password', 'admin-2fa-code', 'admin-2fa-manage-password', 'admin-2fa-manage-code']
    .forEach(id => { const el = document.getElementById(id); if (el) el.value = ''; });
  document.getElementById('admin-hint-text').value = st.password_hint || '';
  setListStatus('admin-status', '');
  renderTwoStep(st);
}

function renderTwoStep(st) {
  document.getElementById('admin-2fa-state').textContent = st.two_step
    ? `On. ${st.recovery_codes_left} recovery code${st.recovery_codes_left === 1 ? '' : 's'} left.`
    : 'Off. Turn it on to also ask for a code from an authenticator app.';
  document.getElementById('form-admin-2fa-start').hidden = !!st.two_step;
  document.getElementById('form-admin-2fa-manage').hidden = !st.two_step;
  document.getElementById('form-admin-2fa-confirm').hidden = true;
  document.getElementById('admin-2fa-codes').hidden = true;
}

function showRecoveryCodes(codes) {
  document.getElementById('admin-2fa-codes-list').textContent = (codes || []).join('\n');
  document.getElementById('admin-2fa-codes').hidden = false;
}

async function adminPost(path, body, okText) {
  const res = await apiRequest(path, 'POST', body);
  const data = await res.json().catch(() => ({}));
  adminCsrf(data);
  if (!res.ok) {
    setListStatus('admin-status', data.message || 'That didn\'t work.', 'error');
    return null;
  }
  if (okText) setListStatus('admin-status', okText, 'ok');
  return data;
}

function setupAdminListeners() {
  document.getElementById('form-admin-create')?.addEventListener('submit', handleAdminCreate);
  document.getElementById('form-admin-signin')?.addEventListener('submit', handleAdminSignIn);
  document.getElementById('btn-admin-start-over')?.addEventListener('click', handleStartOver);
  document.getElementById('btn-admin-forgot-still')?.addEventListener('click', e => {
    document.getElementById('admin-forgot-reset').hidden = false;
    e.currentTarget.hidden = true;
  });
  document.getElementById('btn-admin-unreadable-delete')?.addEventListener('click', () => handleForgotDeleteAll('admin-unreadable-status'));
  document.getElementById('btn-open-admin')?.addEventListener('click', openAdminModal);
  document.getElementById('btn-admin-signout')?.addEventListener('click', handleAdminSignOut);

  document.getElementById('form-admin-password')?.addEventListener('submit', async e => {
    e.preventDefault();
    const pw = document.getElementById('admin-pw-new').value;
    if (pw !== document.getElementById('admin-pw-confirm').value) {
      setListStatus('admin-status', 'The two new passwords are different.', 'error');
      return;
    }
    const data = await adminPost('/api/v1/admin/password',
      { password: document.getElementById('admin-pw-current').value, new_password: pw },
      'Password changed. Other browsers were signed out.');
    if (data) {
      await storePassword((adminState.status && adminState.status.name) || 'admin', pw);
      e.target.reset();
    }
  });
  document.getElementById('form-admin-hint')?.addEventListener('submit', async e => {
    e.preventDefault();
    const hint = document.getElementById('admin-hint-text').value;
    const data = await adminPost('/api/v1/admin/hint',
      { password: document.getElementById('admin-hint-password').value, hint },
      hint.trim() ? 'Hint saved.' : 'Hint removed.');
    if (data) {
      document.getElementById('admin-hint-password').value = '';
      refreshAdminStatus();
    }
  });
  document.getElementById('form-admin-local')?.addEventListener('submit', async e => {
    e.preventDefault();
    const on = document.getElementById('admin-local-require').checked;
    const data = await adminPost('/api/v1/admin/local-sign-in',
      { password: document.getElementById('admin-local-password').value, require_local_sign_in: on },
      on ? 'This PC now asks for the sign-in too.' : 'This PC now opens BombeCam without sign-in.');
    if (data) {
      document.getElementById('admin-local-password').value = '';
      refreshAdminStatus();
    }
  });
  document.getElementById('form-admin-2fa-start')?.addEventListener('submit', async e => {
    e.preventDefault();
    const data = await adminPost('/api/v1/admin/two-step/start', { password: document.getElementById('admin-2fa-start-password').value });
    if (!data) return;
    document.getElementById('admin-2fa-qr').src = data.qr || '';
    document.getElementById('admin-2fa-secret').textContent = (data.secret || '').replace(/(.{4})/g, '$1 ').trim();
    document.getElementById('form-admin-2fa-start').hidden = true;
    document.getElementById('form-admin-2fa-confirm').hidden = false;
    document.getElementById('admin-2fa-code').focus();
  });
  document.getElementById('form-admin-2fa-confirm')?.addEventListener('submit', async e => {
    e.preventDefault();
    const data = await adminPost('/api/v1/admin/two-step/confirm', { code: document.getElementById('admin-2fa-code').value }, 'Two-step sign-in is on.');
    if (!data) return;
    await refreshAdminStatus();
    renderTwoStep(adminState.status || {});
    showRecoveryCodes(data.recovery_codes);
  });
  document.getElementById('btn-admin-2fa-recovery')?.addEventListener('click', async () => {
    const data = await adminPost('/api/v1/admin/recovery-codes', { password: document.getElementById('admin-2fa-manage-password').value }, 'New recovery codes made; the old ones no longer work.');
    if (!data) return;
    await refreshAdminStatus();
    renderTwoStep(adminState.status || {});
    showRecoveryCodes(data.recovery_codes);
  });
  document.getElementById('btn-admin-2fa-disable')?.addEventListener('click', async () => {
    if (!confirm('Turn off two-step sign-in? Signing in will only need the password.')) return;
    const data = await adminPost('/api/v1/admin/two-step/disable', {
      password: document.getElementById('admin-2fa-manage-password').value,
      code: document.getElementById('admin-2fa-manage-code').value,
    }, 'Two-step sign-in is off.');
    if (!data) return;
    await refreshAdminStatus();
    renderTwoStep(adminState.status || {});
  });
  document.getElementById('btn-admin-codes-copy')?.addEventListener('click', e => {
    copyToClipboard(document.getElementById('admin-2fa-codes-list').textContent, e.currentTarget);
  });
  document.getElementById('btn-admin-codes-save')?.addEventListener('click', () => {
    const text = 'BombeCam recovery codes. Each works once.\n\n' + document.getElementById('admin-2fa-codes-list').textContent + '\n';
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([text], { type: 'text/plain' }));
    a.download = 'bombecam-recovery-codes.txt';
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  });
}

if (typeof document !== 'undefined' && document.addEventListener) {
  document.addEventListener('DOMContentLoaded', setupAdminListeners);
}

