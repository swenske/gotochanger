// gotochanger management UI. Vanilla JS, no build step, no dependencies.
// Authentication is cookie/session based (set by /api/v1/auth/*); the
// browser sends the session cookie automatically on same-origin requests.

const state = {
  role: null,
  username: null,
  status: null,
  mailboxQueues: {},
  storageQueues: {},
  logicalLibraries: [],
  showLibraryColors: false,
  hideUnassigned: false,
  tapeSetFamily: {},          // tape_set name -> barcode family label, e.g. "LTO (L8)"
  backupLastRun: null,        // RFC3339 string from GET /api/v1/backup/schedule (Admin-only - see loadBackupLastRun), null if unknown/not Admin
  pendingDriveOps: new Set(),      // drive indices with a same-tab in-flight load/unload
  pendingRobotOps: 0,              // counter, not bool - guards overlapping arm-invoking actions
  pendingElementOps: new Set(),     // "slot:<addr>"/"ioslot:<addr>"/"drive:<index>" keys with a same-tab in-flight Move/Load/Unload
  doorPhases: {},             // "magazine:<id>"/"mailbox:<id>" -> "opening"|"closing"|"scanning", pushed live via SSE
  busyElements: new Set(),   // "slot:<addr>"/"ioslot:<addr>"/"drive:<index>" keys the SERVER reports as mid-Move/Load/Unload, pushed live via SSE "busy" messages - unlike pendingElementOps, survives a page refresh (see applyCardProcessingOverlay)
  armState: null,             // { busy, position } - pushed live via SSE "arm" messages (see connectStream); falls back to status.arm_state until the first one arrives
  robotActivity: [],          // recent live-only arm-narration steps ({time, message}), newest first, pushed live via SSE "arm" messages - never derived from /api/v1/events (see connectStream)
};

// Cap on how many entries state.robotActivity keeps (only the latest
// ROBOT_ACTIVITY_VISIBLE of those are actually rendered) - a rolling
// client-side buffer mirroring the server's own capped ArmSteps ring
// buffer (internal/library/library.go's maxArmSteps), not derived from
// /api/v1/events - the arm's step-by-step narration is live-only, never
// persisted (see PhaseNotifier's doc comment), exactly like door phases.
const ROBOT_ACTIVITY_MAX = 50;
const ROBOT_ACTIVITY_VISIBLE = 30;

const panelOrderKey = "gotochanger.dashboard.panelOrder";
const eventsCollapsedKey = "gotochanger.dashboard.eventsCollapsed";
const collapsedPanelsKey = "gotochanger.dashboard.collapsedPanels";
const showLibraryColorsKey = "gotochanger.dashboard.showLibraryColors";
const hideUnassignedKey = "gotochanger.dashboard.hideUnassigned";
let dashboardUIInitialized = false;

async function api(path, opts = {}) {
  const headers = Object.assign({ "Content-Type": "application/json" }, opts.headers || {});
  const res = await fetch(path, Object.assign({ credentials: "same-origin" }, opts, { headers }));
  if (res.status === 204) return null;
  const body = await res.json().catch(() => null);
  if (!res.ok) {
    if (res.status === 401) boot(); // session expired/missing: fall back to the login screen
    const msg = (body && body.error) || res.statusText;
    throw new Error(msg);
  }
  return body;
}

// ==================== Icons ====================
// Small inline SVG glyphs for dynamically-rendered dashboard content (static
// chrome - nav/panel headers/admin subnav - has its own copies inlined
// directly in index.html). Kept in sync by hand since there's no build step
// to share them - the same accepted convention this file already uses for
// roboticFaultKinds below, which must match library.RoboticFaultKinds.
const ICONS = {
  drive: '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="6" width="18" height="12" rx="1.5"/><path d="M3 12h18"/><circle cx="7" cy="9" r="0.8" fill="currentColor" stroke="none"/><circle cx="7" cy="15" r="0.8" fill="currentColor" stroke="none"/></svg>',
  robot: '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="5" cy="19" r="2"/><path d="M5 17V11h6l4-4h4"/><circle cx="19" cy="7" r="1.6"/><path d="M11 11l2 2"/></svg>',
  warning: '<svg class="icon icon-sm" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3l10 18H2L12 3z"/><path d="M12 10v4M12 17.5h.01"/></svg>',
  cleaning: '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><rect x="6" y="3" width="12" height="18" rx="2"/><path d="M9 8h6M9 12h6M9 16h4"/></svg>',
};

// ==================== Boot / auth screens ====================

// One of these is picked at random each time the auth screen (bootstrap/
// login/change-password) is shown, for the decorative header image behind
// the auth card - see showAuthScreen(). Files live in static/img/, served
// at /assets/img/... via RegisterWebUI's file server (internal/api/web.go).
const AUTH_HEADER_BACKGROUNDS = [
  "/assets/img/auth-header1.gif",
  "/assets/img/auth-header2.webp",
  "/assets/img/auth-header3.jpg",
  "/assets/img/auth-header4.gif",
];

function pickAuthHeaderBg() {
  const img = document.getElementById("authHeaderBg");
  if (!img) return;
  const pick = AUTH_HEADER_BACKGROUNDS[Math.floor(Math.random() * AUTH_HEADER_BACKGROUNDS.length)];
  img.src = pick;
}

// Shows the daemon's build version (from authStateResponse.version) at the
// bottom of the login form and in the dashboard header. Both elements are
// updated from the one /api/v1/auth/state fetch in boot(), so they're set
// correctly before the user even sees either screen.
function setVersionDisplay(version) {
  const text = version ? `v${version}` : "";
  for (const id of ["loginVersion", "appVersion"]) {
    const el = document.getElementById(id);
    if (el) el.textContent = text;
  }
}

// Client-side because gotochangerd itself often has no outbound internet
// (it commonly runs on a firewalled backup host); the admin's browser
// checking GitHub directly needs no new daemon code, endpoint, or cache.
// GitHub's releases API allows unauthenticated CORS requests.
function isNewerVersion(latest, current) {
  const a = latest.split(".").map(Number);
  const b = current.split(".").map(Number);
  for (let i = 0; i < Math.max(a.length, b.length); i++) {
    const x = a[i] || 0, y = b[i] || 0;
    if (x !== y) return x > y;
  }
  return false;
}

async function checkForUpdate(currentVersion) {
  const badge = document.getElementById("updateBadge");
  if (!badge || !currentVersion || currentVersion === "dev") return;
  try {
    const res = await fetch("https://api.github.com/repos/swenske/gotochanger/releases/latest", {
      signal: AbortSignal.timeout(5000),
    });
    if (!res.ok) return;
    const data = await res.json();
    const latest = (data.tag_name || "").replace(/^v/, "");
    if (latest && isNewerVersion(latest, currentVersion)) {
      badge.title = `v${latest} available`;
      badge.hidden = false;
    }
  } catch (e) {
    // Offline, GitHub unreachable, or rate-limited — not critical, skip silently.
  }
}

async function boot() {
  disconnectStream();
  stopFallbackPolling();
  let s;
  try {
    s = await fetch("/api/v1/auth/state", { credentials: "same-origin" }).then((r) => r.json());
  } catch (e) {
    s = { bootstrap_required: false, authenticated: false };
  }
  setVersionDisplay(s.version);
  checkForUpdate(s.version);
  if (s.bootstrap_required) {
    showAuthScreen("bootstrap");
  } else if (!s.authenticated) {
    showAuthScreen("login");
  } else if (s.must_change_password) {
    showAuthScreen("changePassword");
  } else {
    await enterAppOrWizard(s);
  }
}

// Shared by boot() and the bootstrap/login handlers: whoever just became
// authenticated lands on the dashboard only if the setup wizard has been
// completed, otherwise on the wizard. Previously bootstrap/login called
// showApp() directly without this check, so the wizard was unreachable
// except via a raw page reload while already authenticated.
//
// GET /api/v1/wizard is Admin-only (server.go), since it exposes
// in-progress topology setup state - only an Admin can ever complete the
// wizard, and an Operator/Viewer account only gets created once an Admin
// has already done so. So only gate admins on wizard completion here;
// every other role goes straight to the dashboard. Skipping this check
// for non-admins previously meant every non-admin login 403'd on this
// call and surfaced as a bogus "role does not have admin access" error
// on the login screen - non-admin accounts could never sign in at all.
async function enterAppOrWizard(s) {
  if (s.role !== "admin") {
    showApp(s);
    return;
  }
  const wizardState = await api("/api/v1/wizard");
  if (wizardState.completed) {
    showApp(s);
  } else {
    showWizardScreen(wizardState);
  }
}

function showAuthScreen(which) {
  document.getElementById("app").hidden = true;
  document.getElementById("authScreen").hidden = false;
  document.getElementById("wizardScreen").hidden = true;
  pickAuthHeaderBg();
  for (const id of ["bootstrapForm", "loginForm", "changePasswordForm"]) {
    document.getElementById(id).hidden = true;
  }
  const map = { bootstrap: "bootstrapForm", login: "loginForm", changePassword: "changePasswordForm" };
  document.getElementById(map[which]).hidden = false;
}

function showWizardScreen(state) {
  document.getElementById("app").hidden = true;
  document.getElementById("authScreen").hidden = true;
  document.getElementById("wizardScreen").hidden = false;
  renderWizardStep(state);
}

function showApp(s) {
  state.role = s.role;
  state.username = s.username;
  document.getElementById("authScreen").hidden = true;
  document.getElementById("wizardScreen").hidden = true;
  document.getElementById("app").hidden = false;
  document.getElementById("whoami").textContent = `${s.username} (${s.role})`;
  document.getElementById("adminNavBtn").hidden = s.role !== "admin";
  initDashboardUI();
  switchView("dashboard");
  loadTapeSetFamilyMap();
  loadBackupLastRun();
  refresh();
  connectStream();
}

document.getElementById("bootstrapForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const password = document.getElementById("bootstrapPassword").value;
  const confirm = document.getElementById("bootstrapConfirm").value;
  try {
    const s = await api("/api/v1/auth/bootstrap", { method: "POST", body: JSON.stringify({ password, confirm_password: confirm }) });
    await enterAppOrWizard(s);
  } catch (err) {
    document.getElementById("bootstrapError").textContent = err.message;
  }
});

document.getElementById("loginForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const username = document.getElementById("loginUsername").value;
  const password = document.getElementById("loginPassword").value;
  try {
    const s = await api("/api/v1/auth/login", { method: "POST", body: JSON.stringify({ username, password }) });
    if (s.must_change_password) {
      showAuthScreen("changePassword");
    } else {
      await enterAppOrWizard(s);
    }
  } catch (err) {
    document.getElementById("loginError").textContent = err.message;
  }
});

const wizardSteps = [
  { id: "mode", title: "Operational Mode" },
  { id: "drives", title: "Drives" },
  { id: "magazines", title: "Magazines" },
  { id: "mailboxes", title: "Mailboxes" },
  { id: "offsite", title: "Offsite Location" },
  { id: "tape-sets", title: "Tape Sets" },
  { id: "logical-libs", title: "Logical Libraries" },
  { id: "latency", title: "Latency Simulation" },
  { id: "telemetry", title: "Anonymous Statistics" },
];

// Generates the next unused "<prefix>N" name given a list of existing
// names, so removing an entry from the middle and adding a new one can't
// collide with a name that's still in use.
function nextSequentialName(existingNames, prefix) {
  const nums = existingNames
    .map((n) => parseInt(String(n || "").slice(prefix.length), 10))
    .filter((n) => !isNaN(n));
  const next = nums.length ? Math.max(...nums) + 1 : 1;
  return `${prefix}${next}`;
}

// ==================== Logical library color helpers (shared by Admin > Logical Libraries and wizard step 7) ====================

const LOGICAL_LIBRARY_COLORS = ["#4285F4", "#EA4335", "#FBBC05", "#34A853", "#9C27B0", "#00ACC1", "#FF7043", "#5C6BC0"];

function nextLogicalLibraryColor(existingCount) {
  return LOGICAL_LIBRARY_COLORS[existingCount % LOGICAL_LIBRARY_COLORS.length];
}

// ==================== Tape set helpers (shared by Admin > Tape Sets and wizard step 6) ====================

async function loadTapeTypeOptions() {
  const tts = await api("/api/v1/tape-types");
  return (tts || []).map((tt) => ({ value: tt.name, label: `${tt.name} (${tt.capacity})` }));
}

function defaultTapeSetFolder(name) {
  return `/var/lib/gotochanger/tapesets/${name}`;
}

// Mirrors config.ValidateTapeSet's checks plus the "at least one tape"
// requirement, so both Admin > Tape Sets and the wizard give identical
// error text instead of only finding out from the server.
function validateTapeSetInput({ name, tape_type, storage_folder, tape_count }) {
  if (!name) return "Name is required.";
  if (!tape_type) return "Tape type is required.";
  if (!storage_folder || storage_folder[0] !== "/") return "Storage folder must be an absolute path.";
  if (!(parseInt(tape_count, 10) >= 1)) return "Number of tapes must be at least 1.";
  return null;
}

async function renderWizardStep(state) {
  const step = state.current_step || 1;
  const options = await api("/api/v1/wizard/options");
  renderWizardStepUI(step, state, options);
}

// Re-renders the current step from the in-memory `state`/`options` already
// held by the caller, without a round trip to the wizard API. Used both for
// the initial render and to reflect local edits (add/remove/change) made to
// list-shaped step data (magazines/tape sets/logical libraries) before the
// user clicks Next.
function renderWizardStepUI(step, state, options) {
  document.getElementById("wizardSteps").innerHTML = wizardSteps
    .map((s, i) => `
      <div class="wizard-step ${i + 1 === step ? "active" : i + 1 < step ? "completed" : ""}">
        <div class="wizard-step-number">${i + 1}</div>
        <div class="wizard-step-title">${s.title}</div>
      </div>
    `)
    .join("");

  document.getElementById("wizardContent").innerHTML = `
    <form id="wizardForm" class="wizard-form">
      ${renderWizardStepContent(step, state, options)}
      <div class="wizard-actions">
        ${step > 1 ? `<button type="button" id="wizardPrev" class="btn">Previous</button>` : ""}
        <button type="submit" id="wizardNext" class="btn primary">${step === wizardSteps.length ? "Finish" : "Next"}</button>
      </div>
    </form>
  `;

  document.getElementById("wizardForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await handleWizardStepSubmit(step, state, options);
    } catch (err) {
      console.error("Error submitting wizard step:", err);
      alert("Error: " + err.message);
    }
  });

  if (step > 1) {
    document.getElementById("wizardPrev").addEventListener("click", async () => {
      try {
        // Carry the already-known state fields along, not just the target
        // step number: the backend re-validates the destination step's
        // required fields on every transition, forward or backward, so a
        // bare {step} body always fails validation for any step that has
        // required fields. Strip completed/current_step first: the server
        // decodes WizardRequest with unknown fields disallowed, and those
        // two are WizardState-only fields, not part of WizardRequest.
        const { completed, current_step, ...requestFields } = state;
        const body = Object.assign({}, requestFields, { step: step - 1 });
        const newState = await api("/api/v1/wizard", { method: "POST", body: JSON.stringify(body) });
        renderWizardStep(newState);
      } catch (err) {
        console.error("Error going to previous wizard step:", err);
        alert("Error: " + err.message);
      }
    });
  }

  attachWizardStepEditors(step, state, options, () => renderWizardStepUI(step, state, options));
}

// Resolves which logical-library row (if any) currently claims a given
// drive/magazine/mailbox in the wizard's in-progress state.logical_libraries,
// keyed by row INDEX (not name - a row's name may be blank/duplicate while
// mid-edit, and index is the only stable identifier at this stage). Since
// nothing is persisted server-side yet, the only possible conflict is
// another row within this same in-progress wizard step - the wizard's bulk
// save (SaveLogicalLibraries) has no exclusivity check of its own, unlike
// Admin's AddLogicalLibrary/UpdateLogicalLibrary, so this is a
// client-side-only guard. Used by step 7's assignment board to decide which
// card an item's chip belongs in.
function wizardLibraryOwners(state) {
  const driveOwner = {};
  const magazineOwner = {};
  const mailboxOwner = {};
  (state.logical_libraries || []).forEach((lib, i) => {
    for (const di of lib.drives || []) driveOwner[di] = i;
    for (const id of lib.magazines || []) magazineOwner[id] = i;
    for (const id of lib.mailboxes || []) mailboxOwner[id] = i;
  });
  return { driveOwner, magazineOwner, mailboxOwner };
}

// kernelModeAvailable/kernelModeHint back step 1's kernel-mode radio
// gating, from GET /api/v1/wizard/options' kernel_mode field (see
// api.KernelModeStatus) - a live check, not a fixed catalog entry, since
// availability depends on whether the gotochanger-kernel package/kernel
// module are actually present on this host right now.
function kernelModeAvailable(options) {
  return !!(options && options.kernel_mode && options.kernel_mode.available);
}

function kernelModeHint(options) {
  const km = (options && options.kernel_mode) || {};
  if (km.in_container) return "Kernel mode isn't available inside a Docker container - it needs real host kernel/TCMU access. Run gotochangerd outside Docker to use this mode.";
  if (km.missing_package) return "Install the gotochanger-kernel package to enable this mode.";
  if (km.missing_kernel_module) return "The target_core_user kernel module isn't loaded yet - it loads automatically at boot once gotochanger-kernel is installed, or run: sudo modprobe target_core_user";
  return "Kernel mode isn't available on this host yet.";
}

// Refreshing can't help while running in a container - availability there
// is fixed by the deployment, not by anything an in-place recheck could
// pick up - so the button is only offered for the two host-side cases.
function kernelModeShowRefresh(options) {
  const km = (options && options.kernel_mode) || {};
  return !kernelModeAvailable(options) && !km.in_container;
}

function renderWizardStepContent(step, state, options) {
  switch (step) {
    case 1:
      return `
        <h2>Operational Mode</h2>
        <p>Name this virtual tape library and choose its operational mode.</p>
        <div class="form-group">
          <label>VTL Name:</label>
          <input type="text" name="vtlName" value="${state.vtl_name || ""}" placeholder="VTL0" required>
        </div>
        <div class="form-group">
          <label>
            <input type="radio" name="operationalMode" value="changer" ${state.operational_mode === "changer" || !state.operational_mode ? "checked" : ""} required>
            Changer Command Script
          </label>
        </div>
        <div class="form-group">
          <label>
            <input type="radio" name="operationalMode" value="kernel" ${state.operational_mode === "kernel" ? "checked" : ""} ${kernelModeAvailable(options) ? "" : "disabled"}>
            Kernel SCSI devices via TCMU/LIO
          </label>
          ${kernelModeAvailable(options) ? "" : `<p class="hint">${kernelModeHint(options)}</p>`}
          ${kernelModeShowRefresh(options) ? `<button type="button" id="wizardKernelModeRefresh" class="btn">Refresh</button>` : ""}
        </div>
        <h3>Restore from an existing backup</h3>
        <p>Already have a backup of a previously configured virtual tape library? Restore it instead of configuring one from scratch. This replaces the entire database and restarts the service.</p>
        <div class="form-group">
          <input type="file" id="wizardRestoreFile" accept=".db">
          <button type="button" id="wizardRestoreBtn" class="btn">Restore backup</button>
        </div>
        <p class="auth-error" id="wizardRestoreError"></p>
        <p class="hint" id="wizardRestoreStatus"></p>
      `;
    case 2:
      return `
        <h2>Drives</h2>
        <p>Configure the drives for the virtual tape library.</p>
        <div id="driveTypes">
          ${(state.drives || [])
            .map(
              (d, i) => `
                <div class="form-group wizard-row" data-index="${i}">
                  <label>Drive ${i + 1} Type:</label>
                  <select name="driveType" data-index="${i}">
                    ${options.drive_types
                      .map(
                        (dt) => `
                          <option value="${dt.name}" ${dt.name === d.name ? "selected" : ""}>${dt.name} (${dt.description})</option>
                        `
                      )
                      .join("")}
                  </select>
                  <button type="button" class="btn" data-remove-drive="${i}">Remove</button>
                </div>
              `
            )
            .join("")}
        </div>
        <button type="button" id="addDrive" class="btn">Add Drive</button>
      `;
    case 3:
      return `
        <h2>Magazines</h2>
        <p>Configure the magazines for the virtual tape library.</p>
        <div id="magazines">
          ${(state.magazines || [])
            .map(
              (mag) => `
                <div class="form-group wizard-row">
                  <label>${mag.id} Slots:</label>
                  <select name="magazineSlots" data-id="${mag.id}">
                    ${[5, 10, 15, 20]
                      .map(
                        (slots) => `
                          <option value="${slots}" ${mag.slots === slots ? "selected" : ""}>${slots}</option>
                        `
                      )
                      .join("")}
                  </select>
                  <button type="button" class="btn" data-remove-magazine="${mag.id}">Remove</button>
                </div>
              `
            )
            .join("")}
        </div>
        <button type="button" id="addMagazine" class="btn">Add Magazine</button>
      `;
    case 4:
      return `
        <h2>Mailboxes</h2>
        <p>Configure the mailboxes (I/O door element groups) for the virtual tape library. Optional - skip this step if you don't need import/export I/O slots.</p>
        <div id="mailboxes">
          ${(state.mailboxes || [])
            .map(
              (mb) => `
                <div class="form-group wizard-row">
                  <label>${mb.id} Slots:</label>
                  <select name="mailboxSlots" data-id="${mb.id}">
                    ${[1, 2, 3, 4, 5]
                      .map(
                        (slots) => `
                          <option value="${slots}" ${mb.slots === slots ? "selected" : ""}>${slots}</option>
                        `
                      )
                      .join("")}
                  </select>
                  <button type="button" class="btn" data-remove-mailbox="${mb.id}">Remove</button>
                </div>
              `
            )
            .join("")}
        </div>
        <button type="button" id="addMailbox" class="btn">Add Mailbox</button>
      `;
    case 5:
      return `
        <h2>Offsite Location</h2>
        <p>Optionally create a virtual offsite location.</p>
        <div class="form-group">
          <label>
            <input type="checkbox" name="offsiteLocation" ${state.offsite_location ? "checked" : ""}>
            Enable Offsite Location
          </label>
        </div>
      `;
    case 6:
      return `
        <h2>Tape Sets</h2>
        <p>Group cartridges into tape sets by tape type, each stored in its own folder on disk. Cartridges are auto-generated with barcodes matching the tape type's format (e.g. LTO-8 &rarr; 000001L8, 000002L8, ...).</p>
        <div id="tapeSets">
          ${(state.tape_sets || [])
            .map(
              (ts, i) => `
                <div class="form-group wizard-row" data-index="${i}">
                  <label>Name:</label>
                  <input type="text" name="tapeSetName" data-index="${i}" value="${ts.name}" required>
                  <label>Tape Type:</label>
                  <select name="tapeSetType" data-index="${i}">
                    ${(options.tape_types || [])
                      .map(
                        (tt) => `
                          <option value="${tt.name}" ${tt.name === ts.tape_type ? "selected" : ""}>${tt.name} (${tt.capacity})</option>
                        `
                      )
                      .join("")}
                  </select>
                  <label>Storage Folder:</label>
                  <div class="field-with-button">
                    <input type="text" name="tapeSetFolder" data-index="${i}" value="${ts.storage_folder || ""}" placeholder="${defaultTapeSetFolder(ts.name)}" required>
                    <button type="button" class="btn" data-browse-tapeset="${i}">Browse&hellip;</button>
                  </div>
                  <label>Number of tapes:</label>
                  <input type="number" name="tapeSetCount" data-index="${i}" min="1" value="${ts.tape_count || 1}">
                  <button type="button" class="btn" data-remove-tapeset="${i}">Remove</button>
                </div>
              `
            )
            .join("")}
        </div>
        <button type="button" id="addTapeSet" class="btn">Add Tape Set</button>
      `;
    case 7: {
      // owner === undefined means "unassigned, lives in the Available pool";
      // otherwise it's the owning row's index (see wizardLibraryOwners).
      const owners = wizardLibraryOwners(state);
      const chip = (kind, id, label, owner) =>
        `<span class="chip" data-chip-kind="${kind}" data-chip-id="${id}" data-chip-owner="${owner === undefined ? "pool" : owner}">${label}</span>`;
      const zone = (kind, owner, items, ownerMap, idOf, labelOf) => {
        const chips = items
          .map((item, idx) => ({ item, idx }))
          .filter(({ item, idx }) => ownerMap[idOf(item, idx)] === owner)
          .map(({ item, idx }) => chip(kind, idOf(item, idx), labelOf(item, idx), owner))
          .join("");
        return `<div class="board-dropzone" data-zone-kind="${kind}" data-zone-owner="${owner === undefined ? "pool" : owner}">${chips}</div>`;
      };
      const boardSections = (owner) => `
        <div class="board-section-label">Drives</div>
        ${zone("drive", owner, state.drives || [], owners.driveOwner, (d, di) => di, (d, di) => `Drive ${di} (${d.name})`)}
        <div class="board-section-label">Magazines</div>
        ${zone("magazine", owner, state.magazines || [], owners.magazineOwner, (m) => m.id, (m) => m.id)}
        <div class="board-section-label">Mailboxes</div>
        ${zone("mailbox", owner, state.mailboxes || [], owners.mailboxOwner, (m) => m.id, (m) => m.id)}
      `;
      return `
        <h2>Logical Libraries</h2>
        <p>Create logical libraries to partition the virtual tape library. Drag a drive, magazine, or mailbox from Available into a library to assign it, or back into Available to unassign it - an item already assigned to a library can only be dropped back into Available, not directly into another library.</p>
        <div class="board">
          <div class="board-card board-pool">
            <h3>Available</h3>
            ${boardSections(undefined)}
          </div>
          <div id="logicalLibs">
            ${(state.logical_libraries || [])
              .map(
                (lib, i) => `
                <div class="form-group wizard-row board-card lib-bordered" data-index="${i}" style="border-left-color: ${lib.color || "#4285F4"}">
                  <label>Logical Library ${i + 1} Name:</label>
                  <input type="text" name="logicalLibName" data-index="${i}" value="${lib.name}" required>
                  <label>Color:</label>
                  <input type="color" name="logicalLibColor" data-index="${i}" value="${lib.color || "#4285F4"}">
                  ${boardSections(i)}
                  <button type="button" class="btn" data-remove-lib="${i}">Remove</button>
                </div>
              `
              )
              .join("")}
          </div>
          <button type="button" id="addLogicalLib" class="btn">Add Logical Library</button>
        </div>
      `;
    }
    case 8:
      return `
        <h2>Latency Simulation</h2>
        <p>Simulate real tape library timing (drive load/unload, robotic arm movement, barcode scans, and more) instead of instant operations.</p>
        <div class="form-group">
          <label><input type="checkbox" name="latencyEnabled" ${state.latency_enabled ? "checked" : ""}> Enable tape library latency simulation</label>
          <p class="hint">You can fine-tune the exact delays afterwards from Admin &gt; Latency.</p>
        </div>
      `;
    case 9:
      return `
        <h2>Anonymous Statistics</h2>
        <p>Help gotochanger development by sending a small, anonymous report once when the daemon starts. This is entirely optional and can be changed at any time from Admin &gt; Settings.</p>
        <div class="form-group">
          <label><input type="checkbox" name="telemetryEnabled" ${state.telemetry_enabled ? "checked" : ""}> Send anonymous usage statistics</label>
          <p class="hint">Sent to <code>${options.telemetry_endpoint || ""}</code>, once per daemon startup (plus once now, immediately, if you enable it here).</p>
        </div>
        <div class="form-group raw-config">
          <p><strong>Exactly what would be sent</strong> (this is a live preview, generated from your actual configuration):</p>
          <pre>${JSON.stringify(options.telemetry_preview || {}, null, 2)}</pre>
          <p class="hint">Never sent: the VTL name, magazine/mailbox/drive/tape-type names, volume barcodes or file paths, usernames, or any IP address/hostname. Only anonymous counts, feature toggles, and a one-way, non-reversible instance ID.</p>
        </div>
      `;
    default:
      return "";
  }
}

// Wires up the interactive bits of steps 2/3/4/6/7 that plain form fields
// can't cover: adding/removing list entries and keeping edits to existing
// entries synced back into `state`. handleWizardStepSubmit forwards
// state.drives/magazines/mailboxes/tape_sets/logical_libraries directly (not
// via FormData) for these steps, so without this wiring those fields can
// never be populated by the user.
function attachWizardStepEditors(step, state, options, refresh) {
  if (step === 1) {
    document.getElementById("wizardRestoreBtn").addEventListener("click", async () => {
      const file = document.getElementById("wizardRestoreFile").files[0];
      const errorEl = document.getElementById("wizardRestoreError");
      if (!file) {
        errorEl.textContent = "Choose a backup file first.";
        return;
      }
      if (!confirm("This replaces the entire database and restarts the service. Continue?")) return;
      await submitRestore(file, document.getElementById("wizardRestoreStatus"), errorEl);
    });
    const kernelModeRefreshBtn = document.getElementById("wizardKernelModeRefresh");
    if (kernelModeRefreshBtn) {
      kernelModeRefreshBtn.addEventListener("click", () => {
        // vtl_name only lands in `state` on form submit, so a re-render from
        // `state` alone would silently drop an unsaved, already-typed name.
        const vtlNameInput = document.querySelector('input[name="vtlName"]');
        if (vtlNameInput) state.vtl_name = vtlNameInput.value;
        renderWizardStep(state);
      });
    }
  }
  if (step === 2) {
    document.getElementById("addDrive").addEventListener("click", () => {
      if (!options.drive_types || !options.drive_types.length) {
        alert("No drive types are configured; cannot add a drive.");
        return;
      }
      state.drives = state.drives || [];
      state.drives.push(Object.assign({}, options.drive_types[0]));
      refresh();
    });
    document.querySelectorAll('select[name="driveType"]').forEach((sel) => {
      sel.addEventListener("change", (e) => {
        const dt = options.drive_types.find((d) => d.name === e.target.value);
        if (dt) state.drives[parseInt(e.target.dataset.index, 10)] = Object.assign({}, dt);
      });
    });
    document.querySelectorAll("[data-remove-drive]").forEach((btn) => {
      btn.addEventListener("click", () => {
        state.drives.splice(parseInt(btn.dataset.removeDrive, 10), 1);
        refresh();
      });
    });
  } else if (step === 3) {
    document.getElementById("addMagazine").addEventListener("click", () => {
      state.magazines = state.magazines || [];
      const id = nextSequentialName(state.magazines.map((m) => m.id), "Magazine");
      state.magazines.push({ id, slots: 5 });
      refresh();
    });
    document.querySelectorAll('select[name="magazineSlots"]').forEach((sel) => {
      sel.addEventListener("change", (e) => {
        const mag = state.magazines.find((m) => m.id === e.target.dataset.id);
        if (mag) mag.slots = parseInt(e.target.value, 10);
      });
    });
    document.querySelectorAll("[data-remove-magazine]").forEach((btn) => {
      btn.addEventListener("click", () => {
        state.magazines = state.magazines.filter((m) => m.id !== btn.dataset.removeMagazine);
        refresh();
      });
    });
  } else if (step === 4) {
    document.getElementById("addMailbox").addEventListener("click", () => {
      state.mailboxes = state.mailboxes || [];
      const id = nextSequentialName(state.mailboxes.map((m) => m.id), "Mailbox");
      state.mailboxes.push({ id, slots: 1 });
      refresh();
    });
    document.querySelectorAll('select[name="mailboxSlots"]').forEach((sel) => {
      sel.addEventListener("change", (e) => {
        const mb = state.mailboxes.find((m) => m.id === e.target.dataset.id);
        if (mb) mb.slots = parseInt(e.target.value, 10);
      });
    });
    document.querySelectorAll("[data-remove-mailbox]").forEach((btn) => {
      btn.addEventListener("click", () => {
        state.mailboxes = state.mailboxes.filter((m) => m.id !== btn.dataset.removeMailbox);
        refresh();
      });
    });
  } else if (step === 6) {
    document.getElementById("addTapeSet").addEventListener("click", () => {
      if (!options.tape_types || !options.tape_types.length) {
        alert("No tape types are configured; cannot create a tape set.");
        return;
      }
      state.tape_sets = state.tape_sets || [];
      const name = nextSequentialName(state.tape_sets.map((t) => t.name), "TapeSet");
      state.tape_sets.push({ name, tape_type: options.tape_types[0].name, storage_folder: "", tape_count: 1 });
      refresh();
    });
    document.querySelectorAll('input[name="tapeSetName"]').forEach((el) => {
      el.addEventListener("input", (e) => {
        state.tape_sets[parseInt(e.target.dataset.index, 10)].name = e.target.value;
      });
    });
    document.querySelectorAll('select[name="tapeSetType"]').forEach((sel) => {
      sel.addEventListener("change", (e) => {
        state.tape_sets[parseInt(e.target.dataset.index, 10)].tape_type = e.target.value;
      });
    });
    document.querySelectorAll('input[name="tapeSetFolder"]').forEach((el) => {
      el.addEventListener("input", (e) => {
        state.tape_sets[parseInt(e.target.dataset.index, 10)].storage_folder = e.target.value;
      });
    });
    document.querySelectorAll("[data-browse-tapeset]").forEach((btn) => {
      btn.addEventListener("click", async () => {
        const idx = parseInt(btn.dataset.browseTapeset, 10);
        const input = document.querySelector(`input[name="tapeSetFolder"][data-index="${idx}"]`);
        const chosen = await openFolderPicker(input.value || undefined);
        if (chosen) {
          input.value = chosen;
          state.tape_sets[idx].storage_folder = chosen;
        }
      });
    });
    document.querySelectorAll('input[name="tapeSetCount"]').forEach((el) => {
      el.addEventListener("input", (e) => {
        state.tape_sets[parseInt(e.target.dataset.index, 10)].tape_count = parseInt(e.target.value, 10) || 0;
      });
    });
    document.querySelectorAll("[data-remove-tapeset]").forEach((btn) => {
      btn.addEventListener("click", () => {
        state.tape_sets.splice(parseInt(btn.dataset.removeTapeset, 10), 1);
        refresh();
      });
    });
  } else if (step === 7) {
    document.getElementById("addLogicalLib").addEventListener("click", () => {
      state.logical_libraries = state.logical_libraries || [];
      const name = nextSequentialName(state.logical_libraries.map((l) => l.name), "Library");
      const color = nextLogicalLibraryColor(state.logical_libraries.length);
      state.logical_libraries.push({ name, drives: [], magazines: [], mailboxes: [], color });
      refresh();
    });
    document.querySelectorAll('input[name="logicalLibName"]').forEach((el) => {
      el.addEventListener("input", (e) => {
        state.logical_libraries[parseInt(e.target.dataset.index, 10)].name = e.target.value;
      });
    });
    document.querySelectorAll('input[name="logicalLibColor"]').forEach((el) => {
      el.addEventListener("input", (e) => {
        state.logical_libraries[parseInt(e.target.dataset.index, 10)].color = e.target.value;
      });
    });
    // kind -> the state.logical_libraries[i] field it belongs to, and how
    // to parse a chip's data-chip-id back into that field's value type
    // (drive ids are numeric indices; magazine/mailbox ids are strings).
    const chipFields = {
      drive: ["drives", (v) => parseInt(v, 10)],
      magazine: ["magazines", (v) => v],
      mailbox: ["mailboxes", (v) => v],
    };
    resetDropZones();
    document.querySelectorAll("[data-chip-kind]").forEach((el) => {
      const owner = el.dataset.chipOwner === "pool" ? undefined : parseInt(el.dataset.chipOwner, 10);
      const [, parse] = chipFields[el.dataset.chipKind];
      wireDraggable(el, { kind: el.dataset.chipKind, id: parse(el.dataset.chipId), currentLibrary: owner });
    });
    document.querySelectorAll("[data-zone-kind]").forEach((el) => {
      const zoneOwner = el.dataset.zoneOwner; // "pool" or a row-index string
      const [field] = chipFields[el.dataset.zoneKind];
      wireDropZone(el, {
        // Only the pool accepts an already-assigned item (unassign); a
        // library zone only accepts a pool-resident item (assign) - moving
        // between two libraries directly is not a valid single drop, by
        // design (see the step-7 hint text above).
        accepts: (p) => p.kind === el.dataset.zoneKind && (zoneOwner === "pool" ? true : p.currentLibrary === undefined),
        onDrop: (p) => {
          if (zoneOwner === "pool") {
            const lib = state.logical_libraries[p.currentLibrary];
            lib[field] = lib[field].filter((v) => v !== p.id);
          } else {
            const lib = state.logical_libraries[parseInt(zoneOwner, 10)];
            if (!lib[field].includes(p.id)) lib[field].push(p.id);
          }
          // Re-render so the item's chip moves to its new zone and every
          // other zone's valid/invalid state is recomputed fresh.
          refresh();
        },
      });
    });
    document.querySelectorAll("[data-remove-lib]").forEach((btn) => {
      btn.addEventListener("click", () => {
        state.logical_libraries.splice(parseInt(btn.dataset.removeLib, 10), 1);
        refresh();
      });
    });
  }
}

async function handleWizardStepSubmit(step, state, options) {
  const form = document.getElementById("wizardForm");
  const formData = new FormData(form);
  const data = { step };

  switch (step) {
    case 1:
      data.vtl_name = formData.get("vtlName") || "";
      data.operational_mode = formData.get("operationalMode") || "changer";
      break;
    case 2:
      data.drives = state.drives || [];
      break;
    case 3:
      data.magazines = state.magazines || [];
      break;
    case 4:
      data.mailboxes = state.mailboxes || [];
      break;
    case 5:
      data.offsite_location = formData.has("offsiteLocation");
      break;
    case 6:
      for (const ts of state.tape_sets || []) {
        const err = validateTapeSetInput({ name: ts.name, tape_type: ts.tape_type, storage_folder: ts.storage_folder, tape_count: ts.tape_count });
        if (err) throw new Error(`Tape set ${ts.name || "(unnamed)"}: ${err}`);
      }
      data.tape_sets = state.tape_sets || [];
      break;
    case 7:
      data.logical_libraries = state.logical_libraries || [];
      break;
    case 8:
      data.latency_enabled = formData.has("latencyEnabled");
      break;
    case 9:
      data.telemetry_enabled = formData.has("telemetryEnabled");
      break;
  }

  const newState = await api("/api/v1/wizard", { method: "POST", body: JSON.stringify(data) });
  if (newState.completed) {
    await api("/api/v1/wizard/complete", { method: "POST" });
    const s = await api("/api/v1/auth/state");
    showApp(s);
  } else {
    renderWizardStep(newState);
  }
}

document.getElementById("changePasswordForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const current_password = document.getElementById("cpCurrent").value;
  const new_password = document.getElementById("cpNew").value;
  try {
    await api("/api/v1/auth/change-password", { method: "POST", body: JSON.stringify({ current_password, new_password }) });
    boot();
  } catch (err) {
    document.getElementById("cpError").textContent = err.message;
  }
});

document.getElementById("logoutBtn").addEventListener("click", async () => {
  await api("/api/v1/auth/logout", { method: "POST" }).catch(() => {});
  boot();
});

// ==================== Navigation ====================

document.getElementById("mainNav").addEventListener("click", (e) => {
  const btn = e.target.closest(".nav-btn[data-view]");
  if (!btn) return;
  switchView(btn.dataset.view);
});

// Every admin subsection is Admin-only, "backup" included. It used to be
// the one Operator+ exception, for manual backup download - that exception
// is gone now that a backup file contains the users/tokens tables too (see
// the routing comment on /api/v1/backup/download), so the whole Admin view
// is hidden from non-admins and enforced server-side by RBAC regardless.
function switchView(view) {
  for (const btn of document.querySelectorAll("#mainNav .nav-btn[data-view]")) {
    btn.classList.toggle("active", btn.dataset.view === view);
  }
  document.getElementById("view-dashboard").hidden = view !== "dashboard";
  document.getElementById("view-admin").hidden = view !== "admin";
  if (view === "admin") {
    for (const btn of document.querySelectorAll(".subnav-btn")) btn.hidden = false;
    selectAdminSection("users");
  }
}

function selectAdminSection(section) {
  for (const b of document.querySelectorAll(".subnav-btn")) b.classList.toggle("active", b.dataset.admin === section);
  for (const id of ["admin-users", "admin-tokens", "admin-drive-types", "admin-tape-types", "admin-tape-sets", "admin-drives", "admin-magazines", "admin-mailboxes", "admin-logical-libraries", "admin-latency", "admin-cleaning-tapes", "admin-settings", "admin-prometheus", "admin-telemetry", "admin-security", "admin-backup", "admin-reset"]) {
    document.getElementById(id).hidden = id !== "admin-" + section;
  }
  loadAdmin(section);
}

document.querySelector(".sub-nav").addEventListener("click", (e) => {
  const btn = e.target.closest(".subnav-btn");
  if (!btn) return;
  selectAdminSection(btn.dataset.admin);
});

function loadAdmin(section) {
  if (section === "users") loadUsers();
  else if (section === "tokens") loadTokens();
  else if (section === "settings") loadSettings();
  else if (section === "latency") loadLatencySettings();
  else if (section === "prometheus") loadPrometheusSettings();
  else if (section === "telemetry") loadTelemetrySettings();
  else if (section === "cleaning-tapes") loadCleaningAdmin();
  else if (section === "security") loadSecuritySettings();
  else if (section === "backup") loadBackup();
  else if (section === "reset") loadReset();
  else loadAdminExtra(section);
}

function fmtBytes(n) {
  if (!n) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return n.toFixed(1) + " " + units[i];
}

// ==================== folder picker ====================
// A second, standalone <dialog>, independent of the generic one below, so
// it can open on top of it (e.g. from a "Browse..." button inside an
// already-open create/edit dialog) - each <dialog> element manages its own
// place in the browser's top-layer stack.
const folderPickerDlg = document.getElementById("folderPickerDialog");
const folderPickerPathEl = document.getElementById("folderPickerPath");
const folderPickerListEl = document.getElementById("folderPickerList");

async function renderFolderPicker(path) {
  const resp = await api("/api/v1/fs/browse?path=" + encodeURIComponent(path));
  folderPickerPathEl.textContent = resp.path;
  folderPickerListEl.innerHTML = "";
  folderPickerDlg.dataset.currentPath = resp.path;
  if (resp.parent) {
    const li = document.createElement("li");
    li.textContent = ".. (up)";
    li.addEventListener("click", () => renderFolderPicker(resp.parent));
    folderPickerListEl.appendChild(li);
  }
  for (const e of (resp.entries || []).filter((e) => e.is_dir)) {
    const li = document.createElement("li");
    li.textContent = e.name;
    li.addEventListener("click", () => renderFolderPicker(resp.path.replace(/\/$/, "") + "/" + e.name));
    folderPickerListEl.appendChild(li);
  }
}

// openFolderPicker shows the folder-browsing dialog starting at startPath
// (default /var/lib/gotochanger) and resolves to the chosen absolute path,
// or null if cancelled.
function openFolderPicker(startPath) {
  return new Promise((resolve) => {
    renderFolderPicker(startPath || "/var/lib/gotochanger").catch((err) => alert("Error: " + err.message));
    folderPickerDlg.showModal();
    const selectBtn = document.getElementById("folderPickerSelectBtn");
    const cancelBtn = document.getElementById("folderPickerCancelBtn");
    const onSelect = () => { cleanup(); resolve(folderPickerDlg.dataset.currentPath); };
    const onCancel = () => { cleanup(); resolve(null); };
    function cleanup() {
      folderPickerDlg.close();
      selectBtn.removeEventListener("click", onSelect);
      cancelBtn.removeEventListener("click", onCancel);
    }
    selectBtn.addEventListener("click", onSelect);
    cancelBtn.addEventListener("click", onCancel);
  });
}

// ==================== PIN keypad ====================
// Its own bespoke dialog, not the generic openDialog mechanism below - same
// precedent as the folder-picker/Bareos-config-viewer dialogs above, since
// a numeric keypad needs its own layout/interaction, not a form field.
//
// openPINPrompt shows the keypad and calls attemptFn(digits) once 4 digits
// are entered. attemptFn must return (or resolve to) {ok:true} on success
// or {ok:false, message} on failure. On failure the dialog stays open,
// clears the entered digits, and shows message inline - the operator can
// retry immediately without losing dashboard state or navigating away, per
// the requirement that a wrong PIN keeps the user on the same screen.
// Resolves true if attemptFn ultimately succeeded, false if the operator
// cancelled.
function openPINPrompt(title, attemptFn) {
  return new Promise((resolve) => {
    const pinDlg = document.getElementById("pinDialog");
    const titleEl = document.getElementById("pinDialogTitle");
    const display = document.getElementById("pinDialogDisplay");
    const errorEl = document.getElementById("pinDialogError");
    const keypad = document.getElementById("pinDialogKeypad");
    const okBtn = document.getElementById("pinDialogOk");

    titleEl.textContent = title;
    let digits = "";
    let submitting = false;
    let resolved = false;

    function render() {
      display.textContent = "● ".repeat(digits.length) + "○ ".repeat(Math.max(0, 4 - digits.length));
      okBtn.disabled = submitting || digits.length !== 4;
    }

    function finish(ok) {
      if (resolved) return;
      resolved = true;
      keypad.removeEventListener("click", onKeypadClick);
      okBtn.removeEventListener("click", submit);
      pinDlg.removeEventListener("close", onNativeClose);
      pinDlg.close();
      resolve(ok);
    }

    function onNativeClose() {
      finish(false);
    }

    async function submit() {
      if (submitting || digits.length !== 4) return;
      submitting = true;
      render();
      let result;
      try {
        result = await attemptFn(digits);
      } catch (e) {
        result = { ok: false, message: e.message };
      }
      submitting = false;
      if (result && result.ok) {
        finish(true);
        return;
      }
      digits = "";
      render();
      errorEl.textContent = (result && result.message) || "Incorrect PIN";
    }

    function onKeypadClick(e) {
      const keyBtn = e.target.closest("button");
      if (!keyBtn) return;
      if (keyBtn.dataset.digit !== undefined) {
        if (submitting || digits.length >= 4) return;
        digits += keyBtn.dataset.digit;
        errorEl.textContent = "";
        render();
        if (digits.length === 4) submit();
      } else if (keyBtn.dataset.action === "backspace") {
        if (submitting) return;
        digits = digits.slice(0, -1);
        errorEl.textContent = "";
        render();
      } else if (keyBtn.dataset.action === "cancel") {
        finish(false);
      }
    }

    keypad.addEventListener("click", onKeypadClick);
    okBtn.addEventListener("click", submit);
    pinDlg.addEventListener("close", onNativeClose);

    errorEl.textContent = "";
    render();
    pinDlg.showModal();
  });
}

// ==================== dialog helper ====================
const dlg = document.getElementById("dialog");
const dlgTitle = document.getElementById("dialogTitle");
const dlgBody = document.getElementById("dialogBody");

// Builds one pane (a .board-card) of an "assignmentboard" dialog field -
// see openDialog below. `kinds` is the field's own kind descriptor array
// ({kind, field, label, items, idOf, labelOf, owner}); `membership` is the
// field's shared, mutable {kind -> Set(id)} staged state; `isRightPane`
// selects which half of each kind's catalog this pane shows (items IN
// membership vs. items NOT in it); `onChange` re-renders both panes after
// a drop. Mirrors the shape of the old per-page assignment board
// (renderLogicalLibraryBoard, since removed) but scoped to a single
// "Available | this library" pair instead of one card per library, and
// staged locally instead of persisting per drop.
function buildAssignmentBoardPane(title, kinds, membership, isRightPane, onChange) {
  const card = document.createElement("div");
  card.className = "board-card";
  const h3 = document.createElement("h3");
  h3.textContent = title;
  card.appendChild(h3);
  for (const k of kinds) {
    const label = document.createElement("div");
    label.className = "board-section-label";
    label.textContent = k.label;
    card.appendChild(label);
    const zone = document.createElement("div");
    zone.className = "board-dropzone";
    for (const item of k.items) {
      const id = k.idOf(item);
      const inHere = membership[k.kind].has(id);
      if (isRightPane !== inHere) continue;
      const owner = !isRightPane ? k.owner[id] : undefined;
      const chip = document.createElement("span");
      if (owner) {
        // Assigned to a *different* library - shown for context (matches
        // the old checkbox era's "in use by X" label) but not draggable;
        // reassigning it has to go through that other library's own Edit.
        chip.className = "chip chip-disabled";
        chip.innerHTML = `${k.labelOf(item)} <span class="hint">— in use by ${owner.name}</span>`;
      } else {
        chip.className = "chip";
        chip.innerHTML = k.labelOf(item);
        wireDraggable(chip, { kind: k.kind, id });
      }
      zone.appendChild(chip);
    }
    wireDropZone(zone, {
      accepts: (p) => p.kind === k.kind,
      onDrop: (p) => {
        if (isRightPane) membership[k.kind].add(p.id);
        else membership[k.kind].delete(p.id);
        onChange();
      },
    });
    card.appendChild(zone);
  }
  return card;
}

// fields: {name, label, type, options, value, placeholder, disabled}.
// type "checkboxgroup" resolves to an array of the checked option values;
// type "folderpicker" is a text input (still directly typeable) plus a
// "Browse..." button opening the folder-picker dialog above; type
// "assignmentboard" (see buildAssignmentBoardPane above) resolves to
// {[kind.field]: id[]} collected from its right-hand pane; every other
// type resolves to a plain string, same as before.
function openDialog(title, fields) {
  return new Promise((resolve) => {
    dlgTitle.textContent = title;
    dlgBody.innerHTML = "";
    const inputs = {};
    const checkboxGroups = {};
    const boardFields = {};
    for (const f of fields) {
      if (f.type !== "assignmentboard") {
        const label = document.createElement("label");
        label.textContent = f.label;
        dlgBody.appendChild(label);
      }
      if (f.type === "assignmentboard") {
        // Two-pane drag-and-drop membership editor: "Available" (items not
        // currently in f.kinds[].initial, minus anything already claimed by
        // a *different* logical library) on the left, f.rightLabel (this
        // library) on the right. Purely local/staged - nothing is
        // persisted until the dialog's own OK button resolves this
        // Promise; the caller does one PUT/POST with the returned
        // {drives, magazines, mailboxes}, same as every other field here.
        const wrap = document.createElement("div");
        wrap.className = "board board-modal";
        dlgBody.appendChild(wrap);
        const membership = {};
        for (const k of f.kinds) membership[k.kind] = new Set(k.initial || []);
        const render = () => {
          wrap.innerHTML = "";
          resetDropZones();
          wrap.appendChild(buildAssignmentBoardPane("Available", f.kinds, membership, false, render));
          wrap.appendChild(buildAssignmentBoardPane(f.rightLabel || "Assigned", f.kinds, membership, true, render));
        };
        render();
        boardFields[f.name] = () => {
          const result = {};
          for (const k of f.kinds) result[k.field] = [...membership[k.kind]];
          return result;
        };
        continue;
      }
      if (f.type === "checkboxgroup") {
        const group = document.createElement("div");
        group.className = "checkbox-group";
        const boxes = [];
        for (const opt of f.options) {
          const cbLabel = document.createElement("label");
          const cb = document.createElement("input");
          cb.type = "checkbox";
          cb.value = opt.value;
          cb.checked = !!opt.checked;
          cb.disabled = !!opt.disabled;
          cbLabel.classList.toggle("disabled", !!opt.disabled);
          cbLabel.appendChild(cb);
          cbLabel.appendChild(document.createTextNode(" " + opt.label));
          group.appendChild(cbLabel);
          boxes.push(cb);
        }
        dlgBody.appendChild(group);
        checkboxGroups[f.name] = boxes;
        continue;
      }
      if (f.type === "folderpicker") {
        const wrap = document.createElement("div");
        wrap.className = "field-with-button";
        const input = document.createElement("input");
        input.type = "text";
        input.name = f.name;
        if (f.value !== undefined) input.value = f.value;
        if (f.placeholder) input.placeholder = f.placeholder;
        const browseBtn = document.createElement("button");
        browseBtn.type = "button";
        browseBtn.textContent = "Browse…";
        browseBtn.addEventListener("click", async () => {
          const chosen = await openFolderPicker(input.value || undefined);
          if (chosen) input.value = chosen;
        });
        wrap.appendChild(input);
        wrap.appendChild(browseBtn);
        dlgBody.appendChild(wrap);
        inputs[f.name] = input;
        continue;
      }
      let input;
      if (f.type === "select") {
        input = document.createElement("select");
        for (const opt of f.options) {
          const o = document.createElement("option");
          o.value = opt.value; o.textContent = opt.label;
          o.disabled = !!opt.disabled;
          if (opt.value === f.value) o.selected = true;
          input.appendChild(o);
        }
      } else {
        input = document.createElement("input");
        input.type = f.type || "text";
        if (f.value !== undefined) input.value = f.value;
        if (f.placeholder) input.placeholder = f.placeholder;
        if (f.disabled) input.disabled = true;
      }
      input.name = f.name;
      dlgBody.appendChild(input);
      inputs[f.name] = input;
    }
    dlg.showModal();
    const onClose = () => {
      dlg.removeEventListener("close", onClose);
      if (dlg.returnValue !== "ok") { resolve(null); return; }
      const values = {};
      for (const k in inputs) values[k] = inputs[k].value;
      for (const k in checkboxGroups) values[k] = checkboxGroups[k].filter((cb) => cb.checked).map((cb) => cb.value);
      for (const k in boardFields) values[k] = boardFields[k]();
      resolve(values);
    };
    dlg.addEventListener("close", onClose);
  });
}

function mkBtn(label, onClick) {
  const b = document.createElement("button");
  b.textContent = label;
  b.addEventListener("click", async () => {
    try { await onClick(); } catch (e) { alert(e.message); }
  });
  return b;
}

// ==================== Drag and drop (assignment) ====================
// Generic, payload-agnostic drag/drop plumbing shared by the wizard's
// Logical Libraries step, Admin > Logical Libraries, and (for magazine
// bulk load/unload) the dashboard's outside-tapes/storage-slot cards. Only
// this mechanical layer is shared - DOM building and persistence stay
// separate per call site, matching this codebase's existing convention
// that a wizard step and its Admin equivalent are independent
// implementations sharing only small helpers (openDialog/mkBtn/
// queueAction/api()), not whole renderers.
//
// Known, deliberately accepted limitation: native HTML5 drag-and-drop has
// no keyboard equivalent. The checkbox lists this replaces were Tab+Space
// navigable; dragging a chip/card is not. Accepted for v1 per an explicit
// product decision (see CLAUDE.md) rather than building a parallel
// keyboard-accessible picker - revisit if that trade-off is ever revisited.
//
// The in-flight payload is kept in a plain module-level variable, not
// dataTransfer - dataTransfer.getData() is unreliable to read during
// dragenter/dragover across browsers, and since a drag never leaves this
// page there's no need to round-trip through it at all.
let dragPayload = null;
let dropZones = []; // {el, accepts(payload)} registered since the last resetDropZones()

// Call once per top-level render pass (not per item) before wiring any
// draggable/drop-zone elements, so stale zone references from a previous
// render don't accumulate.
function resetDropZones() {
  dropZones = [];
}

function wireDraggable(el, payload) {
  el.draggable = true;
  el.addEventListener("dragstart", (e) => {
    dragPayload = payload;
    el.classList.add("dragging");
    e.dataTransfer.effectAllowed = "move";
    // Some browsers (notably older Firefox) require *some* data to be set
    // during dragstart for the drag gesture to start at all - the value
    // itself is never read back.
    e.dataTransfer.setData("text/plain", "");
    for (const zone of dropZones) {
      const ok = zone.accepts(payload);
      zone.el.classList.toggle("drop-valid", ok);
      zone.el.classList.toggle("drop-invalid", !ok);
    }
  });
  el.addEventListener("dragend", () => {
    el.classList.remove("dragging");
    dragPayload = null;
    for (const zone of dropZones) zone.el.classList.remove("drop-valid", "drop-invalid", "drag-over");
  });
}

function wireDropZone(el, { accepts, onDrop }) {
  dropZones.push({ el, accepts });
  el.addEventListener("dragenter", (e) => {
    if (!dragPayload || !accepts(dragPayload)) return;
    e.preventDefault();
    el.classList.add("drag-over");
  });
  el.addEventListener("dragover", (e) => {
    // Required on dragover too (not just dragenter), or drop never fires.
    if (!dragPayload || !accepts(dragPayload)) return;
    e.preventDefault();
  });
  el.addEventListener("dragleave", (e) => {
    // dragleave fires when entering any descendant too, not just when
    // truly leaving el - ignore those to avoid flicker as the pointer
    // crosses child chip boundaries.
    if (e.relatedTarget && el.contains(e.relatedTarget)) return;
    el.classList.remove("drag-over");
  });
  el.addEventListener("drop", (e) => {
    e.preventDefault();
    el.classList.remove("drag-over");
    if (dragPayload && accepts(dragPayload)) onDrop(dragPayload);
  });
}

// ==================== Barcode / cartridge rendering ====================
// Code 39 (USS-39): every gotochanger barcode (internal/barcode) is
// uppercase-alphanumeric only, which is exactly the Code 39 charset. Each
// character (digits, A-Z, plus the non-data start/stop char '*') encodes as
// 9 alternating bar/space elements (5 bars + 4 spaces, starting and ending
// with a bar), of which exactly 3 are "wide" and 6 are "narrow" - hence
// "3 of 9". One extra narrow space is inserted between characters (not part
// of the 9-element pattern itself). A full symbol is '*' + DATA + '*'.
// Table below: 1 = wide element, 0 = narrow element, read bar/space/bar/...
const CODE39_PATTERNS = {
  "0": "000110100", "1": "100100001", "2": "001100001", "3": "101100000",
  "4": "000110001", "5": "100110000", "6": "001110000", "7": "000100101",
  "8": "100100100", "9": "001100100",
  A: "100001001", B: "001001001", C: "101001000", D: "000011001",
  E: "100011000", F: "001011000", G: "000001101", H: "100001100",
  I: "001001100", J: "000011100", K: "100000011", L: "001000011",
  M: "101000010", N: "000010011", O: "100010010", P: "001010010",
  Q: "000000111", R: "100000110", S: "001000110", T: "000010110",
  U: "110000001", V: "011000001", W: "111000000", X: "010010001",
  Y: "110010000", Z: "011010000", "*": "010010100",
};

// Renders `barcode` as a Code 39 SVG, human-readable text baked in under
// the bars (like a real printed tape-cartridge label - text and bars are
// one image, so there's no separate text element to keep in sync). Any
// character outside CODE39_PATTERNS (shouldn't happen - internal/barcode
// only ever produces uppercase alphanumerics) falls back to the "0"
// pattern rather than throwing, so a rendering hiccup never breaks the
// whole card.
//
// The rendered width is always TARGET_WIDTH regardless of barcode length
// (6-8 chars for the physical families, up to 32 for "generic") - unit
// width is solved backwards from the content length so a long barcode
// gets thinner bars instead of overflowing its dashboard card. Real Code
// 39 quiet zones (>=10x the narrow bar width on each side) are compressed
// here since this is a decorative cartridge-label rendering, not a
// scannable one.
const BARCODE_TARGET_WIDTH = 148;
function renderBarcodeSVG(barcode) {
  const code = String(barcode || "").toUpperCase();
  const symbol = "*" + code + "*";
  const chars = symbol.split("");
  const wideRatio = 2.4;
  const quietZoneUnits = 3;
  const narrowUnitsPerChar = 6 + 3 * wideRatio; // 6 narrow + 3 wide elements per Code 39 character
  const totalNarrowUnits = chars.length * narrowUnitsPerChar + (chars.length - 1) + 2 * quietZoneUnits;
  const unit = BARCODE_TARGET_WIDTH / totalNarrowUnits;
  const barHeight = 12;
  const textHeight = 11;
  const totalHeight = barHeight + textHeight + 2;

  let x = unit * quietZoneUnits;
  const rects = [];
  chars.forEach((ch, ci) => {
    const pattern = CODE39_PATTERNS[ch] || CODE39_PATTERNS["0"];
    for (let i = 0; i < 9; i++) {
      const wide = pattern[i] === "1";
      const w = wide ? unit * wideRatio : unit;
      if (i % 2 === 0) rects.push(`<rect x="${x.toFixed(2)}" y="0" width="${w.toFixed(2)}" height="${barHeight}" fill="#111"/>`);
      x += w;
    }
    if (ci < chars.length - 1) x += unit; // inter-character gap
  });
  const barsEndX = x;
  const totalWidth = barsEndX + unit * quietZoneUnits;
  const textStartX = unit * quietZoneUnits;
  const textSpanWidth = barsEndX - textStartX;

  return `<svg viewBox="0 0 ${totalWidth.toFixed(2)} ${totalHeight}" width="${totalWidth.toFixed(2)}" height="${totalHeight}" role="img" aria-label="barcode ${code}">` +
    `${rects.join("")}` +
    `<text x="${textStartX.toFixed(2)}" y="${totalHeight - 1}" textLength="${textSpanWidth.toFixed(2)}" lengthAdjust="spacingAndGlyphs" font-family="monospace" font-size="${textHeight}" fill="#111">${code}</text>` +
    `</svg>`;
}

// writeProtectSwitchHTML renders the write-protect state as a small
// vertical slide switch styled after a real tape's physical write-protect
// tab (red thumb, slid down when protected) instead of a text button -
// vertical rather than horizontal so it takes minimal width next to the
// barcode (see .wp-switch in style.css). Always rendered
// (even when not editable) so the card's write-protect state stays visible
// everywhere, matching how a real tab is visible whether or not you can
// currently reach it. `editable` controls only whether it's clickable
// (native `disabled`, so a click can never fire when it shouldn't) - the
// actual permission rule (physically accessible location + operator role)
// is decided by each caller and mirrored server-side by
// Library.SetVolumeWriteProtect/findAccessibleVolumeForWriteProtectLocked.
function writeProtectSwitchHTML(writeProtected, editable) {
  const label = writeProtected ? "Write-protected" : "Writable";
  const hint = editable ? ` - click to ${writeProtected ? "remove write-protect" : "write-protect"}` : "";
  return `<button type="button" class="wp-switch${writeProtected ? " locked" : ""}" ${editable ? "" : "disabled"} title="${label}${hint}" aria-pressed="${writeProtected ? "true" : "false"}" aria-label="Write-protect toggle"><span class="wp-switch-track"><span class="wp-switch-thumb"></span></span></button>`;
}

// wireWriteProtectSwitch attaches the click handler to a switch rendered by
// cartridgeLabelHTML inside card, if one is present and not disabled (a
// disabled/absent switch is a silent no-op - native `disabled` already
// prevents the click from ever firing, this just skips attaching a
// listener at all). Mirrors mkBtn's try/catch -> alert(e.message) pattern.
function wireWriteProtectSwitch(card, barcode, writeProtected) {
  const el = card.querySelector(".wp-switch");
  if (!el || el.disabled) return;
  el.addEventListener("click", async () => {
    try {
      await api(`/api/v1/volumes/${encodeURIComponent(barcode)}/write-protect`, { method: "POST", body: JSON.stringify({ write_protected: !writeProtected }) });
      refresh();
    } catch (e) {
      alert(e.message);
    }
  });
}

// Returns an HTML string (not a DOM node) wrapping a barcode SVG (bars +
// baked-in text, see above) plus an optional tape-type/family badge and
// write-protect switch - a plain string so it drops straight into the
// existing `card.innerHTML = ...` template-literal pattern used everywhere
// below, with no need to restructure any render function to
// appendChild-style. writeProtected is `undefined` for cleaning cartridges
// (no write-protect concept for those - see writeProtectSwitchHTML), which
// omits the switch entirely rather than rendering it disabled.
function cartridgeLabelHTML(barcode, tapeSetName, writeProtected, editable) {
  const family = tapeSetName ? state.tapeSetFamily[tapeSetName] : null;
  const badge = family ? `<div class="cart-meta"><span class="cart-badge">${family}</span></div>` : "";
  const wp = writeProtected !== undefined ? writeProtectSwitchHTML(writeProtected, editable) : "";
  return `<div class="cart-shell"><div class="cart-barcode-row"><div class="cart-barcode">${renderBarcodeSVG(barcode)}</div>${wp}</div>${badge}</div>`;
}

// applyCleaningTooltip sets a live "cleaning cycles left: N" tooltip
// (data-tooltip, not title - see style.css's [data-tooltip] rule) on any
// card representing a cleaning cartridge, wherever it's currently shown
// (a magazine slot, a drive, or the Admin > Cleaning Tapes pool) -
// max_uses comes from the already-Viewer-readable Status
// (cleaning_max_uses), so this works without an Admin-only settings
// fetch.
function applyCleaningTooltip(card, vol, maxUses) {
  if (!vol || !vol.cleaning) return;
  const cyclesLeft = Math.max(0, (maxUses || 0) - (vol.cleaning_usage_count || 0));
  card.dataset.tooltip = `cleaning cycles left: ${cyclesLeft}`;
}

// tape_set name -> barcode family label (e.g. "LTO (L8)"), used only for
// cartridgeLabelHTML's optional badge. Best-effort: Volume JSON carries
// `barcode`/`tape_set` but not the tape type/family directly, so this joins
// tape-sets to tape-types once (not on every 4s dashboard poll) via the same
// formatBarcodeFormat helper Admin > Tape Types already uses.
async function loadTapeSetFamilyMap() {
  try {
    const [sets, types] = await Promise.all([api("/api/v1/tape-sets"), api("/api/v1/tape-types")]);
    const byTypeName = {};
    for (const tt of types || []) byTypeName[tt.name] = formatBarcodeFormat(tt);
    const map = {};
    for (const ts of sets || []) map[ts.name] = byTypeName[ts.tape_type] || ts.tape_type;
    state.tapeSetFamily = map;
  } catch (e) {
    // Non-fatal: cartridge labels simply render without a family badge.
  }
}

// Last backup run time for the Library Status panel's Tape Management
// stats. GET /api/v1/backup/schedule is Admin-only (a backup snapshot
// carries every admin's password hash - see server.go's route comment),
// so this 403s for Viewer/Operator and is caught here exactly like
// loadTapeSetFamilyMap above degrades for non-Admins: state.backupLastRun
// stays null and renderLibraryStats simply omits the "Last backup" tile.
async function loadBackupLastRun() {
  try {
    const info = await api("/api/v1/backup/schedule");
    state.backupLastRun = (info && info.last_run) || null;
  } catch (e) {
    state.backupLastRun = null;
  }
}

// ==================== Dashboard ====================

async function refresh() {
  if (document.getElementById("view-dashboard").hidden) return;
  try {
    const status = await api("/api/v1/status");
    state.status = status;
    state.logicalLibraries = (state.showLibraryColors || state.hideUnassigned)
      ? (await api("/api/v1/logical-libraries")) || []
      : [];
    renderDashboard();
    const events = await api("/api/v1/events");
    renderEvents(events || []);
  } catch (e) {
    // errors already surfaced (e.g. redirected to login on 401)
  }
}

function renderDashboard() {
  const status = state.status;
  if (!status) return;
  const maps = buildLibraryMaps(state.logicalLibraries);
  // One resetDropZones() per top-level render pass, not per sub-render
  // call below - renderOutside/renderSlots each register their own tape
  // drag/drop zones (magazine bulk load/unload) into the same shared
  // dropZones list.
  resetDropZones();
  renderOutside(status.outside_volumes || []);
  document.getElementById("offsite").hidden = !status.offsite_enabled;
  if (status.offsite_enabled) renderOffsite(status.offsite_volumes || []);
  renderRobotStatus(status.robotic_fault || { active: false }, status.arm_state);
  renderLibraryStats(status);
  renderDrives(status.drives || [], maps, status);
  renderIOSlots(status.ioslots || [], status, maps);
  renderSlots(status.slots || [], status, maps);
}

// Resolves which logical library (if any) each drive/slot/io-slot belongs
// to, for the dashboard's "show library colors" / "hide unassigned"
// switches. Built fresh from /api/v1/logical-libraries on every refresh
// rather than cached, since exclusivity means an element can move between
// libraries (or become unassigned) at any time.
function buildLibraryMaps(logicalLibraries) {
  const driveColor = {};
  const slotColor = {};
  const ioslotColor = {};
  for (const lib of logicalLibraries || []) {
    for (const d of lib.drives || []) driveColor[d.index] = lib.color;
    for (const s of lib.slots || []) slotColor[s.address] = lib.color;
    for (const io of lib.io_slots || []) ioslotColor[io.address] = lib.color;
  }
  return { driveColor, slotColor, ioslotColor };
}

// Resolves which logical library (if any) owns each drive/magazine/mailbox,
// for the Admin > Logical Libraries Add/Edit dialogs - lets those dialogs
// disable and label elements already claimed by a *different* logical
// library, instead of only discovering the conflict via a 409 at save
// time. excludeName omits that library's own assignments (used by the
// Edit dialog, so a library's own elements never show as "in use").
// Derived from the same /api/v1/logical-libraries shape buildLibraryMaps
// already consumes; magazine/mailbox ownership is derived from each
// library's slots/io_slots since LogicalLibrary itself has no direct
// magazines/mailboxes list.
function computeLogicalLibraryOwners(logicalLibraries, excludeName) {
  const driveOwner = {};
  const magazineOwner = {};
  const mailboxOwner = {};
  for (const lib of logicalLibraries || []) {
    if (lib.name === excludeName) continue;
    const owner = { name: lib.name, color: lib.color };
    for (const d of lib.drives || []) driveOwner[d.index] = owner;
    for (const s of lib.slots || []) if (s.magazine_id) magazineOwner[s.magazine_id] = owner;
    for (const io of lib.io_slots || []) if (io.mailbox_id) mailboxOwner[io.mailbox_id] = owner;
  }
  return { driveOwner, magazineOwner, mailboxOwner };
}

function applyLibraryBorder(card, color) {
  if (color) {
    card.style.borderColor = color;
    card.classList.add("lib-bordered");
  }
}

function queueAction(queue, action, address, barcode = "") {
  const next = queue.filter((q) => q.address !== address);
  if (action) next.push({ action, address, barcode });
  return next;
}

function outsideOptions() {
  const vols = (state.status && state.status.outside_volumes) || [];
  return vols.slice().sort((a, b) => a.barcode.localeCompare(b.barcode)).map((v) => ({ value: v.barcode, label: v.barcode }));
}

function driveOptions() {
  const drives = (state.status && state.status.drives) || [];
  return drives.filter((d) => !d.volume && !d.fault).sort((a, b) => a.index - b.index).map((d) => ({ value: `drive:${d.index}`, label: `Drive ${d.index}` }));
}

function emptySlotOptions() {
  const slots = (state.status && state.status.slots) || [];
  return slots.filter((s) => !s.volume).sort((a, b) => a.address - b.address).map((s) => ({ value: `slot:${s.address}`, label: `Slot ${s.label || s.address}` }));
}

function emptyIOSlotOptions() {
  const ioslots = (state.status && state.status.ioslots) || [];
  return ioslots.filter((io) => !io.volume).sort((a, b) => a.address - b.address).map((io) => ({ value: `ioslot:${io.address}`, label: `I/O Slot ${io.label || io.address}` }));
}

function nonDriveMoveDestinationOptions() {
  return [...emptySlotOptions(), ...emptyIOSlotOptions()];
}

function unloadDestinationOptions() {
  const slots = (state.status && state.status.slots) || [];
  const ioslots = (state.status && state.status.ioslots) || [];
  const options = [];
  for (const s of slots.slice().sort((a, b) => a.address - b.address)) {
    const full = !!s.volume;
    const lbl = s.label || s.address;
    options.push({
      value: `slot:${s.address}`,
      label: full ? `Slot ${lbl} (full)` : `Slot ${lbl}`,
      disabled: full,
    });
  }
  for (const io of ioslots.slice().sort((a, b) => a.address - b.address)) {
    const full = !!io.volume;
    const lbl = io.label || io.address;
    options.push({
      value: `ioslot:${io.address}`,
      label: full ? `I/O Slot ${lbl} (full)` : `I/O Slot ${lbl}`,
      disabled: full,
    });
  }
  return options;
}

function parseMoveTarget(raw) {
  const parts = String(raw || "").split(":");
  return { kind: parts[0], address: Number(parts[1]) };
}

function savePanelOrder() {
  const container = document.getElementById("view-dashboard");
  const order = Array.from(container.querySelectorAll("section[data-panel-id]"))
    .map((el) => el.dataset.panelId)
    .filter(Boolean);
  localStorage.setItem(panelOrderKey, JSON.stringify(order));
}

function restorePanelOrder() {
  const container = document.getElementById("view-dashboard");
  const raw = localStorage.getItem(panelOrderKey);
  if (!raw) return;
  let order;
  try {
    order = JSON.parse(raw);
  } catch {
    return;
  }
  if (!Array.isArray(order)) return;
  for (const id of order) {
    const panel = container.querySelector(`section[data-panel-id="${id}"]`);
    if (panel) container.appendChild(panel);
  }
}

function setPanelCollapsed(panelEl, collapsed) {
  const btn = panelEl.querySelector(".panel-collapse-btn");
  panelEl.classList.toggle("collapsed", collapsed);
  if (btn) {
    btn.setAttribute("aria-expanded", String(!collapsed));
    btn.title = collapsed ? "Expand panel" : "Collapse panel";
    btn.textContent = collapsed ? "▲" : "▼";
  }
}

function updateCollapseAllBtn() {
  const panels = Array.from(document.querySelectorAll("#view-dashboard section[data-panel-id]"));
  const anyExpanded = panels.some((p) => !p.classList.contains("collapsed"));
  document.getElementById("collapseAllBtn").textContent = anyExpanded ? "Collapse all" : "Expand all";
}

function saveCollapsedPanels() {
  const collapsed = Array.from(document.querySelectorAll("#view-dashboard section[data-panel-id].collapsed"))
    .map((el) => el.dataset.panelId);
  localStorage.setItem(collapsedPanelsKey, JSON.stringify(collapsed));
  updateCollapseAllBtn();
}

function restoreCollapsedPanels() {
  const raw = localStorage.getItem(collapsedPanelsKey);
  let collapsed = [];
  if (raw) {
    try {
      const parsed = JSON.parse(raw);
      if (Array.isArray(parsed)) collapsed = parsed;
    } catch {
      collapsed = [];
    }
  }
  for (const panel of document.querySelectorAll("#view-dashboard section[data-panel-id]")) {
    setPanelCollapsed(panel, collapsed.includes(panel.dataset.panelId));
  }
  updateCollapseAllBtn();
}

function setEventsCollapsed(collapsed) {
  const panel = document.getElementById("events");
  const btn = document.getElementById("eventsToggleBtn");
  const icon = document.getElementById("eventsToggleIcon");
  panel.classList.toggle("collapsed", collapsed);
  btn.setAttribute("aria-expanded", String(!collapsed));
  btn.title = collapsed ? "Show activity log" : "Hide activity log";
  icon.textContent = collapsed ? "▲" : "▼";
  localStorage.setItem(eventsCollapsedKey, collapsed ? "1" : "0");
}

function initDashboardUI() {
  if (dashboardUIInitialized) return;
  dashboardUIInitialized = true;

  restorePanelOrder();
  restoreCollapsedPanels();

  const container = document.getElementById("view-dashboard");

  container.addEventListener("click", (e) => {
    const btn = e.target.closest(".panel-collapse-btn");
    if (!btn) return;
    const panel = btn.closest("section[data-panel-id]");
    if (!panel) return;
    setPanelCollapsed(panel, !panel.classList.contains("collapsed"));
    saveCollapsedPanels();
  });

  document.getElementById("collapseAllBtn").addEventListener("click", () => {
    const panels = Array.from(container.querySelectorAll("section[data-panel-id]"));
    const anyExpanded = panels.some((p) => !p.classList.contains("collapsed"));
    for (const panel of panels) setPanelCollapsed(panel, anyExpanded);
    saveCollapsedPanels();
  });

  let dragPanel = null;
  container.addEventListener("dragstart", (e) => {
    const panel = e.target.closest("section[data-panel-id]");
    if (!panel) return;
    dragPanel = panel;
    panel.classList.add("dragging");
    e.dataTransfer.effectAllowed = "move";
  });

  container.addEventListener("dragover", (e) => {
    if (!dragPanel) return;
    const target = e.target.closest("section[data-panel-id]");
    if (!target || target === dragPanel) return;
    e.preventDefault();
    const rect = target.getBoundingClientRect();
    const before = e.clientY < rect.top + rect.height / 2;
    if (before) {
      container.insertBefore(dragPanel, target);
    } else {
      container.insertBefore(dragPanel, target.nextSibling);
    }
  });

  container.addEventListener("dragend", () => {
    if (!dragPanel) return;
    dragPanel.classList.remove("dragging");
    dragPanel = null;
    savePanelOrder();
  });

  const initialCollapsed = localStorage.getItem(eventsCollapsedKey) === "1";
  setEventsCollapsed(initialCollapsed);
  document.getElementById("eventsToggleBtn").addEventListener("click", () => {
    const panel = document.getElementById("events");
    setEventsCollapsed(!panel.classList.contains("collapsed"));
  });

  const showColorsToggle = document.getElementById("showLibraryColorsToggle");
  const hideUnassignedToggle = document.getElementById("hideUnassignedToggle");
  state.showLibraryColors = localStorage.getItem(showLibraryColorsKey) === "1";
  state.hideUnassigned = localStorage.getItem(hideUnassignedKey) === "1";
  showColorsToggle.checked = state.showLibraryColors;
  hideUnassignedToggle.checked = state.hideUnassigned;
  showColorsToggle.addEventListener("change", (e) => {
    state.showLibraryColors = e.target.checked;
    localStorage.setItem(showLibraryColorsKey, state.showLibraryColors ? "1" : "0");
    refresh();
  });
  hideUnassignedToggle.addEventListener("change", (e) => {
    state.hideUnassigned = e.target.checked;
    localStorage.setItem(hideUnassignedKey, state.hideUnassigned ? "1" : "0");
    refresh();
  });
}

function renderOutside(vols) {
  const grid = document.getElementById("outsideGrid");
  grid.innerHTML = "";
  const canOperate = state.role === "admin" || state.role === "operator";
  for (const v of vols.slice().sort((a, b) => a.barcode.localeCompare(b.barcode))) {
    const card = document.createElement("div");
    card.className = "card";
    const cleaningNote = v.cleaning ? ` <span class="cart-badge">cleaning: ${v.cleaning_state || "available"}</span>` : "";
    card.innerHTML = `${cartridgeLabelHTML(v.barcode, v.tape_set, v.cleaning ? undefined : v.write_protected, canOperate)}<div>${fmtBytes(v.written_bytes)} / ${fmtBytes(v.capacity_bytes)}${v.full ? ' <span class="full">FULL</span>' : ""}${cleaningNote}</div>`;
    wireWriteProtectSwitch(card, v.barcode, v.write_protected);
    if (canOperate) {
      // Bulk-unload path: an open magazine's occupied-slot cards
      // (renderSlots) are draggable as kind "loadedTape" - dropping one
      // here queues a pickup, same as the existing per-slot "Pickup"/
      // "Pickup all" buttons.
      wireDraggable(card, { kind: "tape", barcode: v.barcode });
      const actions = document.createElement("div");
      actions.className = "actions";
      actions.appendChild(mkBtn("Delete", async () => {
        if (!confirm(`Delete outside tape ${v.barcode}? This removes the backing file.`)) return;
        await api(`/api/v1/outside/${encodeURIComponent(v.barcode)}`, { method: "DELETE" });
        refresh();
      }));
      card.appendChild(actions);
    }
    grid.appendChild(card);
  }
  if (canOperate) {
    wireDropZone(grid, {
      accepts: (p) => p.kind === "loadedTape",
      onDrop: (p) => {
        state.storageQueues[p.magId] = queueAction(state.storageQueues[p.magId] || [], "pickup", p.address);
        refresh();
      },
    });
  }
  setPanelSummary("outsideSummary", `tapes: ${vols.length}`);
}

// Fault kinds for the "Raise robotic fault" dialog - values must match
// library.RoboticFaultKinds (internal/library/types.go) exactly.
const roboticFaultKinds = [
  { value: "blocked_arm", label: "Blocked robotic arm" },
  { value: "mispositioned_cartridge", label: "Mispositioned cartridge" },
  { value: "pickup_failure", label: "Cartridge pickup failure" },
  { value: "drop_failure", label: "Cartridge drop / transport failure" },
  { value: "movement_jam", label: "Robot movement jam" },
  { value: "other", label: "Other mechanical fault" },
];

// armPositionLabel turns a server ArmPosition ({kind, address}) into the
// short label shown under the arm's Idle/Moving… status line - this is
// what "the UI must reflect the parked state clearly" means concretely.
function armPositionLabel(pos) {
  if (!pos || !pos.kind) return "Unknown";
  switch (pos.kind) {
    case "parked": return "Parked";
    case "slot": return `Slot ${pos.label || pos.address}`;
    case "ioslot": return `I/O slot ${pos.label || pos.address}`;
    case "drive": return `Drive ${pos.address}`;
    default: return "Unknown";
  }
}

function renderRobotStatus(fault, armStateFallback) {
  const grid = document.getElementById("robotStatus");
  grid.innerHTML = "";
  const canOperate = state.role === "admin" || state.role === "operator";
  const card = document.createElement("div");
  card.className = "card" + (fault.active ? "" : " empty");
  // state.armState (pushed live via SSE "arm" messages) is server truth
  // that reflects ANY caller - a real Bareos job via gotochanger-changer
  // included, not just this browser tab's own clicks - falling back to
  // the last GET /api/v1/status response until the first SSE message
  // arrives. pendingRobotOps stays OR-combined purely for same-tab,
  // zero-round-trip click feedback (see withDriveOp) - it no longer has
  // to carry the whole signal on its own.
  const armState = state.armState || armStateFallback || { busy: false, position: {} };
  const busy = armState.busy || state.pendingRobotOps > 0;
  const ledState = fault.active ? "fault" : (busy ? "busy" : "idle");
  let html = `<div class="addr"><span class="led led-${ledState}"></span>${ICONS.robot}Robotic arm</div>`;
  if (fault.active) {
    const kind = roboticFaultKinds.find((k) => k.value === fault.kind);
    html += `<div class="fault">${ICONS.warning}FAULT: ${kind ? kind.label : fault.kind}</div>`;
    if (fault.message) html += `<div>${fault.message}</div>`;
  } else {
    html += `<div>Status: ${busy ? "Moving…" : "Idle"}</div>`;
  }
  // Current position is always shown, not just while idle - it's server
  // truth (state.armState, pushed live) and always kept up to date, so
  // there's no "stale while moving" concern the earlier idle-only,
  // parenthetical-hint design was guarding against.
  html += `<div>Current position: ${armPositionLabel(armState.position)}</div>`;
  card.innerHTML = html;
  if (canOperate) {
    const actions = document.createElement("div");
    actions.className = "actions";
    if (fault.active) {
      actions.appendChild(mkBtn("Clear fault", async () => {
        await api("/api/v1/robotics/fault", { method: "POST", body: JSON.stringify({ active: false }) });
        refresh();
      }));
    } else {
      actions.appendChild(mkBtn("Raise fault", async () => {
        const v = await openDialog("Raise robotic fault", [
          { name: "kind", label: "Fault kind", type: "select", options: roboticFaultKinds, value: roboticFaultKinds[0].value },
          { name: "message", label: "Message (optional)" },
        ]);
        if (!v) return;
        await api("/api/v1/robotics/fault", { method: "POST", body: JSON.stringify({ active: true, kind: v.kind, message: v.message || "" }) });
        refresh();
      }));
    }
    card.appendChild(actions);
  }
  grid.appendChild(card);
}

// ==================== Library Status: aggregate statistics ====================

// Building blocks for the "Library Statistics" tiles in the right half of
// the Library Status panel (see renderLibraryStats below). valueClass is
// one of "ok"/"warn"/"err" (style.css), the same --ok/--warn/--err colors
// used everywhere else on the dashboard (LEDs, .vol/.full/.fault on
// cards) - kept as its own tiny modifier here rather than reusing those
// tape/volume-specific class names on unrelated tiles like "Drive faults".
function statCardHTML(label, value, valueClass, tooltip) {
  const tt = tooltip ? ` data-tooltip="${tooltip}"` : "";
  return `<div class="card stat-card"${tt}><div class="stat-label">${label}</div><div class="stat-value${valueClass ? " " + valueClass : ""}">${value}</div></div>`;
}

function statSectionHTML(title, cardsHTML) {
  return `<div class="stat-section"><div class="stat-section-label">${title}</div><div class="stat-cards">${cardsHTML}</div></div>`;
}

function statPct(numerator, denominator) {
  if (!denominator) return "—";
  return `${Math.round((numerator / denominator) * 100)}%`;
}

// computeSlotCounts/computeDriveCounts centralize the occupied/free and
// active/idle/free/fault classification shared by renderLibraryStats'
// stat cards below and each panel's collapsed-header summary (see
// setPanelSummary calls in renderOutside/renderOffsite/renderDrives/
// renderIOSlots/renderSlots) - one definition of "empty"/"occupied"/
// "active"/"idle", not a second one invented per call site. Slot and
// IOSlot share the same `.volume` field shape, so one helper covers both
// the Storage Slots and I/O Slots panels.
function computeSlotCounts(slots) {
  const occupied = slots.filter((s) => s.volume).length;
  return { total: slots.length, occupied, free: slots.length - occupied };
}

function computeDriveCounts(drives) {
  let active = 0, idle = 0, free = 0, fault = 0;
  for (const d of drives) {
    if (d.fault) { fault++; continue; }
    if (!d.volume) { free++; continue; }
    if (d.activity === "reading" || d.activity === "writing") active++;
    else idle++;
  }
  return { total: drives.length, active, idle, free, fault };
}

// setPanelSummary fills a panel's collapsed-header summary span (see
// index.html's #outsideSummary/#offsiteSummary/#librarySummary/
// #drivesSummary/#ioslotsSummary/#slotsSummary) - CSS
// (.panel.collapsed .panel-summary) is what actually shows/hides it, so
// this just needs to keep the text current on every render pass, whether
// or not the panel happens to be collapsed right now.
function setPanelSummary(id, text) {
  const el = document.getElementById(id);
  if (el) el.textContent = text;
}

// renderLibraryStats fills #libraryStats, added alongside the robotic arm
// status/activity feed when the panel was renamed from "Robotic Arm" to
// "Library Status". Every figure here is derived client-side from the
// same /api/v1/status snapshot already polled every 4s for the rest of
// the dashboard (see refresh/renderDashboard), so it updates live for
// free - and reuses the exact occupied/free/active field names and rules
// renderDrives/renderSlots already use (drive.fault/volume/activity,
// slot.volume/magazine_id, ioslot.volume/mailbox_id) rather than a second
// notion of "in use". Two Tape Management tiles (configured tape sets,
// last backup) come from Admin-only endpoints fetched once elsewhere
// (loadTapeSetFamilyMap/loadBackupLastRun) and are simply omitted when
// that data isn't available (non-Admin roles), matching how those loaders
// already degrade for lower roles rather than surfacing a 403.
function renderLibraryStats(status) {
  const el = document.getElementById("libraryStats");
  if (!el) return;

  const slots = status.slots || [];
  const ioslots = status.ioslots || [];
  const drives = status.drives || [];

  const slotCounts = computeSlotCounts(slots);
  const slotsHTML =
    statCardHTML("Total slots", slotCounts.total) +
    statCardHTML("Free", slotCounts.free, "ok") +
    statCardHTML("Occupied", slotCounts.occupied) +
    statCardHTML("Occupancy", statPct(slotCounts.occupied, slotCounts.total));

  const driveCounts = computeDriveCounts(drives);
  const drivesHTML =
    statCardHTML("Total drives", driveCounts.total) +
    statCardHTML("Free", driveCounts.free, "ok") +
    statCardHTML("Active", driveCounts.active, driveCounts.active ? "warn" : undefined) +
    statCardHTML("Idle", driveCounts.idle) +
    statCardHTML("Faults", driveCounts.fault, driveCounts.fault ? "err" : "ok") +
    statCardHTML("Utilization", statPct(driveCounts.active, driveCounts.total));

  const inLibraryVolumes = [
    ...slots.map((s) => s.volume).filter(Boolean),
    ...ioslots.map((s) => s.volume).filter(Boolean),
    ...drives.map((d) => d.volume).filter(Boolean),
  ];
  const magazineCount = new Set(slots.map((s) => s.magazine_id).filter(Boolean)).size;
  const mailboxCount = new Set(ioslots.map((s) => s.mailbox_id).filter(Boolean)).size;
  const tapeSetsInUse = new Set(inLibraryVolumes.map((v) => v.tape_set).filter(Boolean)).size;
  let writtenBytes = 0, capacityBytes = 0;
  for (const v of inLibraryVolumes) {
    if (v.capacity_bytes > 0) {
      writtenBytes += v.written_bytes || 0;
      capacityBytes += v.capacity_bytes;
    }
  }
  let tapeHTML =
    statCardHTML("Volumes in library", inLibraryVolumes.length) +
    statCardHTML("Magazines", magazineCount) +
    statCardHTML("Mailboxes", mailboxCount) +
    statCardHTML("Tape sets in use", tapeSetsInUse) +
    statCardHTML("Capacity used", statPct(writtenBytes, capacityBytes));
  const configuredTapeSets = Object.keys(state.tapeSetFamily || {}).length;
  if (configuredTapeSets > 0) tapeHTML += statCardHTML("Configured tape sets", configuredTapeSets);
  if (state.backupLastRun) tapeHTML += statCardHTML("Last backup", new Date(state.backupLastRun).toLocaleString());

  el.innerHTML =
    statSectionHTML("Storage Slots", slotsHTML) +
    statSectionHTML("Drives", drivesHTML) +
    statSectionHTML("Tape Management", tapeHTML);

  const armState = state.armState || status.arm_state || { position: {} };
  setPanelSummary(
    "librarySummary",
    `arm position: ${armPositionLabel(armState.position)}, total slots: ${slotCounts.total}, total volumes: ${inLibraryVolumes.length}`
  );
}

// ==================== Drive front-panel LED simulation ====================
// Mimics a real tape drive's front-panel light: green blinking = active
// operation, red = writing, amber = fault/warning, dim green = ready/idle,
// dark = empty. gotochangerd exposes no "busy" flag on a Drive (Load/Unload/
// Move just block for the whole simulated latency), so "busy" here means
// "this browser tab has an in-flight request touching this drive" - there is
// no way to see another client's in-flight operation, only our own. Priority
// (highest wins): fault > writing > busy > idle > off.

// Marks driveIndexes as busy for the duration of fn() (used around the
// load/unload/move fetches below); pendingRobotOps is a counter (not a bool)
// since arm-invoking dialogs could in principle overlap. gotochangerd
// exposes no per-drive "busy" flag in Status() - only the single global
// ArmState (see internal/library/library.go) - so there is no way to learn
// "this drive has a Move/Load/Unload in flight" from a status fetch alone,
// whether it's this tab's own pending POST or another client's. Painting
// the busy LED from this tab's own optimistic, already-cached state
// immediately, rather than waiting for its POST's own response, is what
// gives instant feedback for the operation this tab itself triggered.
async function withDriveOp(driveIndexes, fn) {
  for (const i of driveIndexes) state.pendingDriveOps.add(i);
  state.pendingRobotOps++;
  renderDashboard();
  try {
    return await fn();
  } finally {
    for (const i of driveIndexes) state.pendingDriveOps.delete(i);
    state.pendingRobotOps--;
    renderDashboard();
  }
}

// withCleaningOp mirrors withDriveOp but deliberately never touches
// pendingRobotOps: cleaning is modeled as a drive-internal process, not a
// robotic transport operation (see Library.Load's doc comment - it
// releases the server's lock for the cleaning-duration sleep
// specifically, for exactly this reason), so the Robotic Arm dashboard
// panel must not show "Moving…" for the (potentially long) request this
// wraps. It only marks pendingDriveOps, for immediate click-to-request-
// sent "busy" feedback - deliberately *not* an early "cleaning" signal:
// the tape isn't physically in the drive yet at click time, and the spec
// requires the drive not show "cleaning" until it actually is (see
// driveLedState, which only trusts server-confirmed d.volume.cleaning).
// With events now pushed live over SSE (see connectStream), that server
// confirmation arrives within milliseconds of the real commit, so no
// responsiveness is lost by not guessing here.
async function withCleaningOp(driveIndexes, fn) {
  for (const i of driveIndexes) state.pendingDriveOps.add(i);
  renderDashboard();
  try {
    return await fn();
  } finally {
    for (const i of driveIndexes) state.pendingDriveOps.delete(i);
    renderDashboard();
  }
}

// withElementOp marks a specific slot/I-O-slot/drive card (elementKey, e.g.
// "slot:5"/"ioslot:3"/"drive:0") as having a same-tab in-flight
// Move/Load/Unload the moment the destination is confirmed - before fn()
// even fires the request - so applyCardProcessingOverlay can gray the card
// out immediately, the same "commit at confirm time" pattern withCleaningOp
// uses for the drive LED.
async function withElementOp(elementKey, fn) {
  state.pendingElementOps.add(elementKey);
  renderDashboard();
  try {
    return await fn();
  } finally {
    state.pendingElementOps.delete(elementKey);
    renderDashboard();
  }
}

// isDriveCleaning is the single server-truth check for "this drive is
// mid cleaning-cycle" - deliberately requires cleaning_state === in_use,
// not just volume.cleaning, since a cleaning cartridge can also sit idle
// in a slot (available/expired). A cleaning cartridge is committed to the
// drive (Load) and cleaning_state flips to in_use before the cleaning-
// duration sleep, not after - see internal/library/library.go's Load -
// so this correctly reflects "busy cleaning" even for a ticker-triggered
// AutoCleanSweep cycle with no local in-flight request, and correctly
// does *not* fire during the mechanical load latency before the tape is
// actually seated (see withCleaningOp's doc comment for why the drive
// must not appear to be "cleaning" before that).
function isDriveCleaning(d) {
  return !!(d.volume && d.volume.cleaning && d.volume.cleaning_state === "in_use");
}

// Drive.activity ("reading"/"writing"/"", see internal/library/types.go)
// is set server-side by a per-drive filesystem watcher on the loaded
// volume's real backing file (inotify on Linux, see
// internal/library/activity_linux.go) - authoritative and near-real-time,
// unlike the old client-side written_bytes-diff-across-polls heuristic
// this replaced.
function driveLedState(d) {
  if (d.fault) return "fault";
  if (isDriveCleaning(d)) return "cleaning";
  if (d.activity === "writing") return "writing";
  if (d.activity === "reading") return "reading";
  if (state.pendingDriveOps.has(d.index)) return "busy";
  if (d.volume) return "idle";
  return "off";
}

function driveLedTitle(ledState) {
  switch (ledState) {
    case "fault": return "Fault";
    case "cleaning": return "Cleaning";
    case "writing": return "Writing";
    case "reading": return "Reading";
    case "busy": return "Active operation";
    case "idle": return "Ready";
    default: return "Empty";
  }
}

function renderDrives(drives, maps, status) {
  const grid = document.getElementById("drivesGrid");
  grid.innerHTML = "";
  const canOperate = state.role === "admin" || state.role === "operator";
  // See the identical comment in renderIOSlots/renderSlots: merges the
  // last-fetched status snapshot with SSE-pushed live state, since either
  // alone could momentarily miss an in-progress operation (a stale
  // snapshot right after a refresh, or a "busy" push that arrived before
  // this render's status did).
  const busyElements = new Set([...(status.busy_elements || []), ...state.busyElements]);
  for (const d of drives) {
    const color = maps.driveColor[d.index];
    // An unassigned drive stays visible if it currently holds a tape.
    if (state.hideUnassigned && !color && !d.volume) continue;
    const ledState = driveLedState(d);
    const card = document.createElement("div");
    card.className = "card" + (d.volume ? "" : " empty");
    if (d.volume && d.volume.cleaning) {
      applyCleaningTooltip(card, d.volume, status && status.cleaning_max_uses);
    } else if (status && status.cleaning_enabled) {
      // data-tooltip (CSS-rendered, see style.css), not the title
      // attribute: title's on-screen popup freezes at whatever text it
      // had when first shown and does not refresh while the cursor
      // stays hovering, even across this element being recreated by the
      // next 4s poll - which would make a live countdown like this one
      // appear stuck.
      const remaining = Math.max(0, (status.cleaning_mount_threshold || 0) - (d.mounts_since_cleaning || 0));
      card.dataset.tooltip = `mounts until cleaning: ${remaining}`;
    }
    if (state.showLibraryColors) applyLibraryBorder(card, color);
    let html = `<div class="addr"><span class="led led-${ledState}" title="${driveLedTitle(ledState)}"></span>${ICONS.drive}Drive ${d.index}</div>`;
    if (d.fault) html += `<div class="fault">${ICONS.warning}FAULT</div>`;
    if (d.volume) {
      // editable=false unconditionally: a mounted tape's write-protect tab
      // is never reachable (it's sealed inside the drive) - see
      // Library.findAccessibleVolumeForWriteProtectLocked.
      html += cartridgeLabelHTML(d.volume.barcode, d.volume.tape_set, d.volume.cleaning ? undefined : d.volume.write_protected, false);
      html += `<div>${fmtBytes(d.volume.written_bytes)} / ${fmtBytes(d.volume.capacity_bytes)}${d.volume.full ? ' <span class="full">FULL</span>' : ""}</div>`;
    } else {
      html += `<div>empty</div>`;
    }
    card.innerHTML = html;
    if (d.volume) wireWriteProtectSwitch(card, d.volume.barcode, d.volume.write_protected);
    const actions = document.createElement("div");
    actions.className = "actions";
    if (d.volume && canOperate) {
      actions.appendChild(mkBtn("Unload to...", async () => {
        const options = unloadDestinationOptions();
        if (!options.some((o) => !o.disabled)) {
          alert("No unload destination available (all slots are full).");
          return;
        }
        const v = await openDialog("Unload drive " + d.index, [{ name: "to", label: "Destination", type: "select", options }]);
        if (!v) return;
        const to = parseMoveTarget(v.to);
        await withElementOp(`drive:${d.index}`, () =>
          withDriveOp([d.index], () => api("/api/v1/unload", { method: "POST", body: JSON.stringify({ drive: d.index, to_kind: to.kind, to_address: to.address }) }))
        );
        refresh();
      }));
    }
    if (canOperate) {
      actions.appendChild(mkBtn(d.fault ? "Clear fault" : "Raise fault", async () => {
        await api(`/api/v1/drives/${d.index}/fault`, { method: "POST", body: JSON.stringify({ fault: !d.fault }) });
        refresh();
      }));
    }
    card.appendChild(actions);
    applyCardProcessingOverlay(card, `drive:${d.index}`, "Unloading…", busyElements.has(`drive:${d.index}`));
    applyCleaningOverlay(card, d);
    grid.appendChild(card);
  }
  const driveCounts = computeDriveCounts(drives);
  const loaded = driveCounts.idle + driveCounts.active;
  // Faults get their own always-visible tile in the expanded Library
  // Status stat cards - not one of the 4 named buckets here - but a
  // faulted drive shouldn't become invisible just because this panel is
  // collapsed, so it's appended only when there's actually one to show.
  const faultPart = driveCounts.fault ? ` fault: ${driveCounts.fault}` : "";
  setPanelSummary(
    "drivesSummary",
    `drives: ${driveCounts.total} idle: ${driveCounts.idle} active: ${driveCounts.active} loaded: ${loaded} empty: ${driveCounts.free}${faultPart}`
  );
}

// phaseLabel maps a door-phase key (see Status.doors.phases,
// library.Library.DoorPhases) to the status text shown over a grayed-out
// magazine/mailbox panel while that phase is active.
function phaseLabel(phase) {
  return { opening: "Opening…", closing: "Closing…", scanning: "Scanning…" }[phase] || "";
}

// applyGroupLockOverlay grays out groupEl and overlays a spinner + phase
// label whenever phase is non-empty, reflecting a real in-progress
// open/close/scan on the server (not a client-side timer guess) pushed live
// via the /api/v1/stream SSE connection.
function applyGroupLockOverlay(groupEl, phase) {
  if (!phase) return;
  groupEl.classList.add("magazine-locked");
  const overlay = document.createElement("div");
  overlay.className = "magazine-lock-overlay";
  const spinner = document.createElement("span");
  spinner.className = "magazine-lock-spinner";
  spinner.setAttribute("aria-hidden", "true");
  overlay.appendChild(spinner);
  const label = document.createElement("span");
  label.textContent = phaseLabel(phase);
  overlay.appendChild(label);
  groupEl.appendChild(overlay);
}

// applyCardProcessingOverlay grays out a single slot/I-O-slot/drive card
// and overlays a spinner + text while either this tab has a same-tab
// in-flight Move/Load/Unload for it (state.pendingElementOps, set by
// withElementOp - shows the specific text passed in, default
// "Processing…", since this tab knows exactly which action it requested)
// or the server itself reports it busy (serverBusy, from
// status.busy_elements/state.busyElements - see the callers below; shown
// as a generic "Busy…" since a refreshed or different tab has no way to
// know which specific operation is running). The server-truth path is
// what keeps the card grayed out across a page refresh, unlike
// pendingElementOps alone, which is lost on reload - mirrors
// applyGroupLockOverlay but scoped to one card instead of a whole
// magazine/mailbox group.
function applyCardProcessingOverlay(card, key, text = "Processing…", serverBusy = false) {
  const pending = state.pendingElementOps.has(key);
  if (!pending && !serverBusy) return;
  card.classList.add("card-processing");
  const overlay = document.createElement("div");
  overlay.className = "card-processing-overlay";
  const spinner = document.createElement("span");
  spinner.className = "card-processing-spinner";
  spinner.setAttribute("aria-hidden", "true");
  overlay.appendChild(spinner);
  const label = document.createElement("span");
  label.textContent = pending ? text : "Busy…";
  overlay.appendChild(label);
  card.appendChild(overlay);
}

// applyCleaningOverlay mirrors applyCardProcessingOverlay's visual
// treatment (grayed-out card + spinner + label) but is driven by
// server-truth cleaning state (isDriveCleaning), not a same-tab pending
// op - it must stay visible for the drive's whole cleaning cycle
// (which can be minutes, per config.CleaningSettings.Duration) and for
// every client watching, not just the tab that started it, and it must
// not appear before the tape is actually loaded (see isDriveCleaning).
function applyCleaningOverlay(card, d) {
  if (!isDriveCleaning(d)) return;
  card.classList.add("card-processing");
  const overlay = document.createElement("div");
  overlay.className = "card-processing-overlay";
  const spinner = document.createElement("span");
  spinner.className = "card-processing-spinner";
  spinner.setAttribute("aria-hidden", "true");
  overlay.appendChild(spinner);
  const label = document.createElement("span");
  label.textContent = "Cleaning…";
  overlay.appendChild(label);
  card.appendChild(overlay);
}

function renderIOSlots(ioslots, status, maps) {
  const container = document.getElementById("ioGrid");
  container.innerHTML = "";
  const canOperate = state.role === "admin" || state.role === "operator";
  const openMailboxes = (status.doors && status.doors.open_mailboxes) || [];
  // state.doorPhases (pushed live via SSE) takes precedence over
  // status.doors.phases: status is only ever a point-in-time snapshot from
  // whenever that fetch happened to land, so SSE's push-as-it-happens
  // phase is always at least as fresh - see connectStream()'s "phase"
  // listener.
  const phases = Object.assign({}, (status.doors && status.doors.phases) || {}, state.doorPhases);
  // Merges the last-fetched status snapshot's busy_elements with the
  // SSE-pushed live set for the same reason phases merges above - either
  // source alone could momentarily miss an in-progress Move/Load/Unload.
  const busyElements = new Set([...(status.busy_elements || []), ...state.busyElements]);

  const groups = [];
  const groupByID = {};
  for (const io of ioslots) {
    const mbId = io.mailbox_id || "";
    if (!groupByID[mbId]) {
      groupByID[mbId] = { id: mbId, ioslots: [] };
      groups.push(groupByID[mbId]);
    }
    groupByID[mbId].ioslots.push(io);
  }

  for (const group of groups) {
    const mbId = group.id;
    // A group is hidden only if NONE of its slots are assigned to a logical
    // library AND none currently hold a tape - a mailbox with even one
    // occupied slot must stay visible regardless of assignment.
    const assigned = group.ioslots.some((io) => maps.ioslotColor[io.address]);
    const hasVolume = group.ioslots.some((io) => io.volume);
    if (state.hideUnassigned && !assigned && !hasVolume) continue;
    const ioOpen = openMailboxes.includes(mbId);
    if (!state.mailboxQueues[mbId]) state.mailboxQueues[mbId] = [];
    const queue = state.mailboxQueues[mbId];

    const groupEl = document.createElement("div");
    groupEl.className = "magazine-group";

    const head = document.createElement("div");
    head.className = "panel-head magazine-group-head";
    const label = document.createElement("h3");
    label.textContent = mbId ? `Mailbox ${mbId}` : "Unassigned";
    head.appendChild(label);

    if (canOperate) {
      const headActions = document.createElement("div");
      headActions.className = "panel-head-actions";
      const pill = document.createElement("span");
      pill.className = "pill " + (ioOpen ? "ok" : "err");
      pill.textContent = ioOpen ? `open (${queue.length} queued)` : "closed";
      headActions.appendChild(pill);
      headActions.appendChild(mkBtn(ioOpen ? "Close mail slot" : "Open mail slot", async () => {
        if (!ioOpen) {
          const openWithPIN = (pin) => withDriveOp([], () => api(`/api/v1/doors/io/${encodeURIComponent(mbId)}/open`, { method: "POST", body: JSON.stringify({ pin }) }));
          if (status.mailbox_pin_required && status.mailbox_pin_required[mbId]) {
            const ok = await openPINPrompt(`Enter PIN for mailbox ${mbId}`, async (pin) => {
              try {
                await openWithPIN(pin);
                return { ok: true };
              } catch (e) {
                return { ok: false, message: e.message };
              }
            });
            if (!ok) return;
          } else {
            await openWithPIN("");
          }
        } else {
          await withDriveOp([], () => api(`/api/v1/doors/io/${encodeURIComponent(mbId)}/close`, { method: "POST", body: JSON.stringify({ actions: queue }) }));
        }
        state.mailboxQueues[mbId] = [];
        refresh();
      }));
      head.appendChild(headActions);
    }
    groupEl.appendChild(head);

    const grid = document.createElement("div");
    grid.className = "grid";
    for (const io of group.ioslots) {
      const card = document.createElement("div");
      card.className = "card" + (io.volume ? "" : " empty");
      if (state.showLibraryColors) applyLibraryBorder(card, maps.ioslotColor[io.address]);
      let html = `<div class="addr">I/O Slot ${io.label || io.address}</div>`;
      // Only reachable to toggle while the mailbox door is open, matching
      // Library.findAccessibleVolumeForWriteProtectLocked's rule - a real
      // tab can't be flipped while sealed behind a closed door.
      html += io.volume ? cartridgeLabelHTML(io.volume.barcode, io.volume.tape_set, io.volume.cleaning ? undefined : io.volume.write_protected, canOperate && ioOpen) : `<div>empty</div>`;
      card.innerHTML = html;
      if (io.volume) wireWriteProtectSwitch(card, io.volume.barcode, io.volume.write_protected);
      if (canOperate) {
        const actions = document.createElement("div");
        actions.className = "actions";
        if (io.volume) {
          if (!ioOpen) {
            // A cartridge cannot be robotically moved while its mailbox
            // door is open - only offer Move/Load once the door is closed.
            actions.appendChild(mkBtn("Move", async () => {
              const options = nonDriveMoveDestinationOptions();
              if (!options.length) {
                alert("No destination available (need an empty storage slot or I/O slot).");
                return;
              }
              const v = await openDialog("Move from I/O slot " + (io.label || io.address), [{ name: "to", label: "Destination", type: "select", options }]);
              if (!v) return;
              const to = parseMoveTarget(v.to);
              await withElementOp(`ioslot:${io.address}`, () =>
                withDriveOp([], () => api("/api/v1/move", { method: "POST", body: JSON.stringify({ from_kind: "ioslot", from_address: io.address, to_kind: to.kind, to_address: to.address }) }))
              );
              refresh();
            }));
            actions.appendChild(mkBtn("Load", async () => {
              const options = driveOptions();
              if (!options.length) {
                alert("No destination available (need an empty, non-faulted drive).");
                return;
              }
              const v = await openDialog("Load I/O slot " + (io.label || io.address) + " into drive", [{ name: "to", label: "Drive", type: "select", options }]);
              if (!v) return;
              const to = parseMoveTarget(v.to);
              const op = io.volume.cleaning ? withCleaningOp : withDriveOp;
              await withElementOp(`ioslot:${io.address}`, () =>
                op([to.address], () => api("/api/v1/load", { method: "POST", body: JSON.stringify({ from_kind: "ioslot", from_address: io.address, drive: to.address }) }))
              );
              refresh();
            }));
          }
          if (ioOpen) {
            const selected = queue.find((q) => q.address === io.address && q.action === "pickup");
            actions.appendChild(mkBtn(selected ? "Pickup queued" : "Pickup", async () => {
              state.mailboxQueues[mbId] = queueAction(queue, selected ? "" : "pickup", io.address);
              refresh();
            }));
          }
        } else {
          if (ioOpen) {
            const opts = outsideOptions();
            if (opts.length) {
              const selected = queue.find((q) => q.address === io.address && q.action === "load");
              actions.appendChild(mkBtn(selected ? `Load queued (${selected.barcode})` : "Load", async () => {
                if (selected) {
                  state.mailboxQueues[mbId] = queueAction(queue, "", io.address);
                  refresh();
                  return;
                }
                const v = await openDialog("Load outside tape into IO slot " + (io.label || io.address), [{ name: "barcode", label: "Tape", type: "select", options: opts }]);
                if (!v || !v.barcode) return;
                state.mailboxQueues[mbId] = queueAction(queue, "load", io.address, v.barcode);
                refresh();
              }));
            }
          }
        }
        card.appendChild(actions);
      }
      applyCardProcessingOverlay(card, `ioslot:${io.address}`, undefined, busyElements.has(`ioslot:${io.address}`));
      grid.appendChild(card);
    }
    groupEl.appendChild(grid);
    applyGroupLockOverlay(groupEl, phases["mailbox:" + mbId]);
    container.appendChild(groupEl);
  }
  const ioCounts = computeSlotCounts(ioslots);
  setPanelSummary("ioslotsSummary", `slots: ${ioCounts.total} empty: ${ioCounts.free} full: ${ioCounts.occupied}`);
}

function renderSlots(slots, status, maps) {
  const container = document.getElementById("slotsGrid");
  container.innerHTML = "";
  const canOperate = state.role === "admin" || state.role === "operator";
  const openMagazines = (status.doors && status.doors.open_magazines) || [];
  // See the identical comment in renderIOSlots: state.doorPhases (pushed
  // live via SSE) takes precedence over status.doors.phases, since status
  // is only ever a point-in-time snapshot that may already be stale by
  // render time.
  const phases = Object.assign({}, (status.doors && status.doors.phases) || {}, state.doorPhases);
  const busyElements = new Set([...(status.busy_elements || []), ...state.busyElements]);

  const groups = [];
  const groupByID = {};
  for (const s of slots) {
    const magId = s.magazine_id || "";
    if (!groupByID[magId]) {
      groupByID[magId] = { id: magId, slots: [] };
      groups.push(groupByID[magId]);
    }
    groupByID[magId].slots.push(s);
  }

  for (const group of groups) {
    const magId = group.id;
    // A group is hidden only if NONE of its slots are assigned to a logical
    // library AND none currently hold a tape - a magazine with even one
    // occupied slot must stay visible regardless of assignment.
    const assigned = group.slots.some((s) => maps.slotColor[s.address]);
    const hasVolume = group.slots.some((s) => s.volume);
    if (state.hideUnassigned && !assigned && !hasVolume) continue;
    const stOpen = openMagazines.includes(magId);
    if (!state.storageQueues[magId]) state.storageQueues[magId] = [];
    const queue = state.storageQueues[magId];

    const groupEl = document.createElement("div");
    groupEl.className = "magazine-group";

    const head = document.createElement("div");
    head.className = "panel-head magazine-group-head";
    const label = document.createElement("h3");
    label.textContent = magId ? `Magazine ${magId}` : "Unassigned";
    head.appendChild(label);

    if (canOperate) {
      const headActions = document.createElement("div");
      headActions.className = "panel-head-actions";
      const pill = document.createElement("span");
      pill.className = "pill " + (stOpen ? "ok" : "err");
      pill.textContent = stOpen ? `open (${queue.length} queued)` : "closed";
      headActions.appendChild(pill);
      headActions.appendChild(mkBtn(stOpen ? "Close storage door" : "Open storage door", async () => {
        if (!stOpen) {
          const openWithPIN = (pin) => withDriveOp([], () => api(`/api/v1/doors/storage/${encodeURIComponent(magId)}/open`, { method: "POST", body: JSON.stringify({ pin }) }));
          if (status.magazine_pin_required) {
            const ok = await openPINPrompt(`Enter PIN for magazine ${magId}`, async (pin) => {
              try {
                await openWithPIN(pin);
                return { ok: true };
              } catch (e) {
                return { ok: false, message: e.message };
              }
            });
            if (!ok) return;
          } else {
            await openWithPIN("");
          }
        } else {
          // Closing a storage door also triggers the arm's post-close
          // magazine scan server-side (RobotMoveScan + MagazineScan latency,
          // see library.CloseStorageDoor) - the arm stays "busy" for that
          // whole window, not just the door swing itself.
          await withDriveOp([], () => api(`/api/v1/doors/storage/${encodeURIComponent(magId)}/close`, { method: "POST", body: JSON.stringify({ actions: queue }) }));
        }
        state.storageQueues[magId] = [];
        refresh();
      }));
      if (stOpen) {
        headActions.appendChild(mkBtn("Bulk load...", async () => {
          const opts = outsideOptions();
          const emptySlots = group.slots
            .filter((s) => !s.volume && !queue.some((entry) => entry.address === s.address))
            .sort((a, b) => a.address - b.address);
          if (!opts.length || !emptySlots.length) {
            alert("No outside tapes or no empty slots available to bulk load.");
            return;
          }
          const v = await openDialog(`Bulk load into magazine ${magId}`, [
            { name: "barcodes", label: "Tapes to load", type: "checkboxgroup", options: opts },
          ]);
          if (!v || !v.barcodes || !v.barcodes.length) return;
          const slotsToUse = emptySlots.slice(0, v.barcodes.length);
          if (v.barcodes.length > slotsToUse.length) {
            alert(`Only ${slotsToUse.length} empty slot(s) available; loading the first ${slotsToUse.length} selected tape(s).`);
          }
          let updated = queue;
          v.barcodes.slice(0, slotsToUse.length).forEach((barcode, i) => {
            updated = queueAction(updated, "load", slotsToUse[i].address, barcode);
          });
          state.storageQueues[magId] = updated;
          refresh();
        }));
        headActions.appendChild(mkBtn("Pickup all", async () => {
          const loaded = group.slots
            .filter((s) => s.volume && !queue.some((entry) => entry.address === s.address && entry.action === "pickup"))
            .sort((a, b) => a.address - b.address);
          if (!loaded.length) {
            alert("No tapes in this magazine to pick up.");
            return;
          }
          let updated = queue;
          for (const s of loaded) {
            updated = queueAction(updated, "pickup", s.address);
          }
          state.storageQueues[magId] = updated;
          refresh();
        }));
      }
      head.appendChild(headActions);
    }
    groupEl.appendChild(head);

    const grid = document.createElement("div");
    grid.className = "grid";
    for (const s of group.slots) {
      const card = document.createElement("div");
      card.className = "card" + (s.volume ? "" : " empty");
      if (state.showLibraryColors) applyLibraryBorder(card, maps.slotColor[s.address]);
      applyCleaningTooltip(card, s.volume, status.cleaning_max_uses);
      let html = `<div class="addr">Slot ${s.label || s.address}</div>`;
      // Only reachable to toggle while the magazine door is open, matching
      // Library.findAccessibleVolumeForWriteProtectLocked's rule - a real
      // tab can't be flipped while sealed behind a closed door.
      html += s.volume ? cartridgeLabelHTML(s.volume.barcode, s.volume.tape_set, s.volume.cleaning ? undefined : s.volume.write_protected, canOperate && stOpen) : `<div>empty</div>`;
      card.innerHTML = html;
      if (s.volume) wireWriteProtectSwitch(card, s.volume.barcode, s.volume.write_protected);
      // Bulk load/unload drag paths, additive alongside the Move/Load/
      // Pickup buttons below - only meaningful while this magazine's door
      // is open, same restriction the buttons already have. An occupied
      // slot is draggable out (onto the outside-tapes grid, queues a
      // pickup); an empty slot accepts a drop from the outside-tapes grid
      // (queues a load), reusing the same queueAction staged-queue the
      // buttons already use, committed atomically on door close.
      if (canOperate && stOpen) {
        if (s.volume) {
          wireDraggable(card, { kind: "loadedTape", magId, address: s.address, barcode: s.volume.barcode });
        } else {
          wireDropZone(card, {
            accepts: (p) => p.kind === "tape",
            onDrop: (p) => {
              state.storageQueues[magId] = queueAction(queue, "load", s.address, p.barcode);
              refresh();
            },
          });
        }
      }
      if (canOperate) {
        const actions = document.createElement("div");
        actions.className = "actions";
        if (s.volume) {
          if (!stOpen) {
            // A cartridge cannot be robotically moved while its magazine
            // door is open - only offer Move/Load once the door is closed.
            actions.appendChild(mkBtn("Move", async () => {
              const options = nonDriveMoveDestinationOptions();
              if (!options.length) {
                alert("No destination available (need an empty storage slot or I/O slot).");
                return;
              }
              const v = await openDialog("Move from storage slot " + (s.label || s.address), [{ name: "to", label: "Destination", type: "select", options }]);
              if (!v) return;
              const to = parseMoveTarget(v.to);
              await withElementOp(`slot:${s.address}`, () =>
                withDriveOp([], () => api("/api/v1/move", { method: "POST", body: JSON.stringify({ from_kind: "slot", from_address: s.address, to_kind: to.kind, to_address: to.address }) }))
              );
              refresh();
            }));
            actions.appendChild(mkBtn("Load", async () => {
              const options = driveOptions();
              if (!options.length) {
                alert("No destination available (need an empty, non-faulted drive).");
                return;
              }
              const v = await openDialog("Load storage slot " + (s.label || s.address) + " into drive", [{ name: "to", label: "Drive", type: "select", options }]);
              if (!v) return;
              const to = parseMoveTarget(v.to);
              const op = s.volume.cleaning ? withCleaningOp : withDriveOp;
              await withElementOp(`slot:${s.address}`, () =>
                op([to.address], () => api("/api/v1/load", { method: "POST", body: JSON.stringify({ from_kind: "slot", from_address: s.address, drive: to.address }) }))
              );
              refresh();
            }));
          }
          if (stOpen) {
            const selected = queue.find((q) => q.address === s.address && q.action === "pickup");
            actions.appendChild(mkBtn(selected ? "Pickup queued" : "Pickup", async () => {
              state.storageQueues[magId] = queueAction(queue, selected ? "" : "pickup", s.address);
              refresh();
            }));
          }
        } else {
          if (stOpen) {
            const opts = outsideOptions();
            if (opts.length) {
              const selected = queue.find((q) => q.address === s.address && q.action === "load");
              actions.appendChild(mkBtn(selected ? `Load queued (${selected.barcode})` : "Load", async () => {
                if (selected) {
                  state.storageQueues[magId] = queueAction(queue, "", s.address);
                  refresh();
                  return;
                }
                const v = await openDialog("Load outside tape into slot " + (s.label || s.address), [{ name: "barcode", label: "Tape", type: "select", options: opts }]);
                if (!v || !v.barcode) return;
                state.storageQueues[magId] = queueAction(queue, "load", s.address, v.barcode);
                refresh();
              }));
            }
          }
        }
        card.appendChild(actions);
      }
      applyCardProcessingOverlay(card, `slot:${s.address}`, undefined, busyElements.has(`slot:${s.address}`));
      grid.appendChild(card);
    }
    groupEl.appendChild(grid);
    applyGroupLockOverlay(groupEl, phases["magazine:" + magId]);
    container.appendChild(groupEl);
  }
  const slotCounts = computeSlotCounts(slots);
  setPanelSummary("slotsSummary", `slots: ${slotCounts.total} empty: ${slotCounts.free} full: ${slotCounts.occupied}`);
}

function renderEvents(events) {
  const list = document.getElementById("eventList");
  list.innerHTML = "";
  for (const e of events.slice(0, 100)) {
    const li = document.createElement("li");
    const t = new Date(e.time).toLocaleTimeString();
    const code = e.code || e.type || "SYSTEM.EVENT.SUCCESS";
    const sev = (e.severity || "information").toLowerCase();
    const outcome = (e.outcome || "success").toLowerCase();

    const ts = document.createElement("span");
    ts.className = "time";
    ts.textContent = t;
    li.appendChild(ts);

    const codeEl = document.createElement("span");
    codeEl.className = "code";
    codeEl.textContent = code;
    li.appendChild(codeEl);

    const meta = document.createElement("span");
    meta.className = `meta ${sev} ${outcome}`;
    const parts = [outcome, sev];
    if (e.category) parts.push(e.category);
    if (e.actor) parts.push(`user=${e.actor}`);
    const sourceIP = e.detail && e.detail.source_ip ? e.detail.source_ip : "";
    if (sourceIP) parts.push(`ip=${sourceIP}`);
    meta.textContent = parts.join(" /");
    li.appendChild(meta);

    const msg = document.createElement("span");
    msg.className = "message";
    msg.textContent = e.message || "";
    li.appendChild(msg);

    list.appendChild(li);
  }
}

// pushArmStep adds one live-pushed arm-narration step (see connectStream's
// "arm" handler) to the front of the rolling buffer - this is what makes
// an in-flight, multi-second Move/Load/Unload visible step by step. Never
// derived from /api/v1/events: the arm's step narration is live-only,
// exactly like a door phase transition (see PhaseNotifier's doc comment
// in internal/library/types.go) - there is no persisted source of truth
// to reconcile against, only the SSE reconnect catch-up (see
// connectStream/handleStream's ArmSteps() replay).
function pushArmStep(time, message) {
  state.robotActivity.unshift({ time, message });
  if (state.robotActivity.length > ROBOT_ACTIVITY_MAX) state.robotActivity.length = ROBOT_ACTIVITY_MAX;
  renderRobotActivity();
}

// renderRobotActivity draws the terminal-style "what is the robotic arm
// doing right now" feed next to the Robotic Arm card - newest entry on
// top, scrollable past ROBOT_ACTIVITY_VISIBLE lines (see .robot-activity
// in style.css, sized to never grow taller than the Robotic Arm panel).
function renderRobotActivity() {
  const el = document.getElementById("robotActivity");
  if (!el) return;
  el.innerHTML = "";
  const entries = state.robotActivity.slice(0, ROBOT_ACTIVITY_VISIBLE);
  if (!entries.length) {
    const empty = document.createElement("div");
    empty.className = "robot-activity-empty";
    empty.textContent = "No robotic activity yet.";
    el.appendChild(empty);
    return;
  }
  for (const e of entries) {
    const row = document.createElement("div");
    row.className = "robot-activity-entry";
    const ts = document.createElement("span");
    ts.className = "time";
    ts.textContent = new Date(e.time).toLocaleTimeString();
    row.appendChild(ts);
    const msg = document.createElement("span");
    msg.className = "message";
    msg.textContent = e.message || "";
    row.appendChild(msg);
    el.appendChild(row);
  }
}

// ==================== Live updates ====================
// Replaces the old fixed-interval poll with a genuinely live dashboard:
// an EventSource connection to /api/v1/stream nudges refresh() the instant
// something changes server-side, instead of waiting up to 4s to notice.
// The old poll loop is kept as a fallback, only running while the SSE
// connection is down (initial connect, or after a drop, until it recovers -
// EventSource reconnects natively, so no custom backoff is implemented).

let sseFallbackTimer = null;
let eventSource = null;

function startFallbackPolling() {
  if (sseFallbackTimer) return;
  sseFallbackTimer = setInterval(() => { if (state.username) refresh(); }, 4000);
}

function stopFallbackPolling() {
  if (sseFallbackTimer) {
    clearInterval(sseFallbackTimer);
    sseFallbackTimer = null;
  }
}

function connectStream() {
  disconnectStream();
  // A stale phase/arm-state/busy-element set from before a drop/reconnect
  // must not linger forever - the server's reconnect catch-up (see
  // handleStream) always re-sends the current arm state and only re-sends
  // door phases/busy elements that are still actually active, so anything
  // not re-sent here needs to start cleared, not carried over.
  state.doorPhases = {};
  state.armState = null;
  state.robotActivity = [];
  state.busyElements = new Set();
  eventSource = new EventSource("/api/v1/stream");
  eventSource.addEventListener("update", (e) => {
    if (!state.username) return;
    refresh();
  });
  eventSource.addEventListener("phase", (e) => {
    // Door phase transitions are pushed with their actual data rather than
    // as a bare nudge: GET /api/v1/status only ever returns a point-in-time
    // snapshot, which may already be stale by the time a refresh()-driven
    // catch-up renders it - a direct push is what makes an in-progress
    // phase visible as it happens, not just eventually. Render straight
    // from the cached status instead of re-fetching it.
    let msg;
    try {
      msg = JSON.parse(e.data);
    } catch (err) {
      return;
    }
    const key = msg.kind + ":" + msg.id;
    if (msg.phase) {
      state.doorPhases[key] = msg.phase;
    } else {
      delete state.doorPhases[key];
    }
    renderDashboard();
  });
  eventSource.addEventListener("arm", (e) => {
    // Same reasoning as "phase" above: a fetched status snapshot may
    // already be stale by render time, so this direct push is what shows
    // the arm's busy/position state - and its step-by-step narration -
    // live, in real time, regardless of which client (this tab, another
    // tab, or a real Bareos job) triggered the operation.
    let msg;
    try {
      msg = JSON.parse(e.data);
    } catch (err) {
      return;
    }
    state.armState = { busy: !!msg.busy, position: msg.position || {} };
    if (msg.step) pushArmStep(msg.step_time || new Date().toISOString(), msg.step);
    renderDashboard();
  });
  eventSource.addEventListener("busy", (e) => {
    // Same reasoning as "phase"/"arm" above: this is what keeps a slot/
    // drive grayed out across a page refresh for as long as its
    // Move/Load/Unload is genuinely still running server-side, instead of
    // only for as long as this tab remembers its own in-flight request
    // (state.pendingElementOps, lost on reload) - see
    // applyCardProcessingOverlay. keys is plural (both ends of a Move/
    // Load/Unload, marked/cleared together in one message) - see
    // library.Library.setElementsBusy's doc comment for why batching
    // these matters for the shared SSE channel's buffer.
    let msg;
    try {
      msg = JSON.parse(e.data);
    } catch (err) {
      return;
    }
    for (const key of msg.keys || []) {
      if (msg.busy) {
        state.busyElements.add(key);
      } else {
        state.busyElements.delete(key);
      }
    }
    renderDashboard();
  });
  eventSource.onopen = () => stopFallbackPolling();
  eventSource.onerror = () => startFallbackPolling();
  // Poll once immediately too, so the dashboard doesn't sit blank until the
  // connection finishes opening.
  startFallbackPolling();
}

function disconnectStream() {
  if (eventSource) {
    eventSource.close();
    eventSource = null;
  }
}

// "Create tape" always creates within a tape set now (issue: tapes outside
// the library must still obey the same barcode/tape-set rules as tapes in
// slots) - it no longer opens a standalone label/barcode/capacity dialog.
document.getElementById("outsideCreateBtn").addEventListener("click", async () => {
  const sets = await api("/api/v1/tape-sets");
  if (!sets || !sets.length) {
    if (state.role === "admin") {
      alert("No tape sets configured yet. Create one first.");
      switchView("admin");
      selectAdminSection("tape-sets");
    } else {
      alert("No tape sets configured yet. Ask an administrator to create one.");
    }
    return;
  }
  const v = await openDialog("Add tape to a tape set", [
    { name: "tape_set", label: "Tape Set", type: "select", options: sets.map((ts) => ({ value: ts.name, label: `${ts.name} (${ts.tape_type})` })) },
    { name: "mode", label: "Mode", type: "select", options: [
      { value: "auto", label: "Auto-generate barcode" },
      { value: "manual", label: "Enter barcode manually" },
    ] },
    { name: "count", label: "Count (auto mode)", type: "number", value: "1" },
    { name: "barcode", label: "Barcode (manual mode)" },
  ]);
  if (!v || !v.tape_set) return;
  const body = v.mode === "manual" ? { barcode: v.barcode } : { count: parseInt(v.count, 10) || 1 };
  try {
    await api(`/api/v1/tape-sets/${encodeURIComponent(v.tape_set)}/tapes`, { method: "POST", body: JSON.stringify(body) });
    refresh();
  } catch (e) {
    alert(e.message);
  }
});

// ==================== Admin: Users ====================

async function loadUsers() {
  const users = await api("/api/v1/users");
  const tbody = document.getElementById("usersTable");
  tbody.innerHTML = "";
  for (const u of users) {
    const tr = document.createElement("tr");
    const status = u.must_change_password ? "must change password" : "active";
    tr.innerHTML = `<td>${u.username}</td><td>${u.role}</td><td>${status}</td><td>${new Date(u.created_at).toLocaleDateString()}</td>`;
    const td = document.createElement("td");
    td.appendChild(mkBtn("Change role", async () => {
      const v = await openDialog("Change role for " + u.username, [
        { name: "role", label: "Role", type: "select", options: [{ value: "admin", label: "admin" }, { value: "operator", label: "operator" }, { value: "viewer", label: "viewer" }] },
      ]);
      if (!v) return;
      await api(`/api/v1/users/${encodeURIComponent(u.username)}/role`, { method: "POST", body: JSON.stringify({ role: v.role }) });
      loadUsers();
    }));
    td.appendChild(mkBtn("Reset password", async () => {
      const v = await openDialog("Reset password for " + u.username, [{ name: "password", label: "New password", type: "password" }]);
      if (!v) return;
      await api(`/api/v1/users/${encodeURIComponent(u.username)}/reset-password`, { method: "POST", body: JSON.stringify({ new_password: v.password }) });
      loadUsers();
    }));
    td.appendChild(mkBtn("Delete", async () => {
      if (!confirm(`Delete user ${u.username}?`)) return;
      await api(`/api/v1/users/${encodeURIComponent(u.username)}`, { method: "DELETE" });
      loadUsers();
    }));
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
}

document.getElementById("newUserBtn").addEventListener("click", async () => {
  const v = await openDialog("New user", [
    { name: "username", label: "Username" },
    { name: "role", label: "Role", type: "select", options: [{ value: "viewer", label: "viewer" }, { value: "operator", label: "operator" }, { value: "admin", label: "admin" }] },
    { name: "password", label: "Initial password (user must change it at next login)", type: "password" },
  ]);
  if (!v || !v.username) return;
  try {
    await api("/api/v1/users", { method: "POST", body: JSON.stringify(v) });
    loadUsers();
  } catch (e) {
    alert(e.message);
  }
});

// ==================== Admin: Tokens ====================

async function loadTokens() {
  const toks = await api("/api/v1/tokens");
  const tbody = document.getElementById("tokensTable");
  tbody.innerHTML = "";
  for (const t of toks) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${t.name}</td><td>${t.role}</td><td>${new Date(t.created_at).toLocaleDateString()}</td>`;
    const td = document.createElement("td");
    td.appendChild(mkBtn("Revoke", async () => {
      if (!confirm(`Revoke token ${t.name}?`)) return;
      await api(`/api/v1/tokens/${encodeURIComponent(t.name)}`, { method: "DELETE" });
      loadTokens();
    }));
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
}

document.getElementById("newTokenBtn").addEventListener("click", async () => {
  const v = await openDialog("New API token", [
    { name: "name", label: "Name" },
    { name: "role", label: "Scope", type: "select", options: [{ value: "viewer", label: "viewer" }, { value: "operator", label: "operator" }, { value: "admin", label: "admin" }] },
  ]);
  if (!v || !v.name) return;
  try {
    const created = await api("/api/v1/tokens", { method: "POST", body: JSON.stringify(v) });
    alert(`Token created (copy it now, it will not be shown again):\n\n${created.token}`);
    loadTokens();
  } catch (e) {
    alert(e.message);
  }
});

// ==================== Admin: Settings ====================

async function loadSettings() {
  const result = await api("/api/v1/settings");
  const cfg = result.config;
  document.getElementById("s_default_capacity").value = cfg.library.default_capacity || "";
  document.getElementById("s_poll_interval").value = cfg.poll_interval || "";
  document.getElementById("s_log_level").value = cfg.log_level || "info";
  document.getElementById("s_snmp_enabled").checked = !!cfg.snmp.enabled;
  document.getElementById("s_snmp_oid").value = cfg.snmp.enterprise_oid || "";
  document.getElementById("s_snmp_agent").value = cfg.snmp.agent_address || "";
  document.getElementById("s_snmp_targets").value = (cfg.snmp.targets || []).map((t) => `${t.host}:${t.port}:${t.community}`).join("\n");
  document.getElementById("s_offsite_location").checked = !!cfg.library.offsite_location;
  document.getElementById("s_offsite_rotation_interval").value = cfg.library.offsite_rotation_interval || "";
  document.getElementById("s_offsite_rotation_count").value = cfg.library.offsite_rotation_count || "";
  document.getElementById("restartHint").textContent = "Fields requiring a service restart to take effect: " + (result.restart_required_fields || []).join(", ");
  document.getElementById("rawConfig").textContent = JSON.stringify(cfg, null, 2);
}

document.getElementById("settingsForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const targets = document.getElementById("s_snmp_targets").value
    .split("\n").map((l) => l.trim()).filter(Boolean)
    .map((l) => {
      const [host, port, community] = l.split(":");
      return { host, port: Number(port), community: community || "public", version: "2c" };
    });
  const req = {
    default_capacity: document.getElementById("s_default_capacity").value,
    poll_interval: document.getElementById("s_poll_interval").value,
    log_level: document.getElementById("s_log_level").value,
    snmp_enabled: document.getElementById("s_snmp_enabled").checked,
    snmp_enterprise_oid: document.getElementById("s_snmp_oid").value,
    snmp_agent_address: document.getElementById("s_snmp_agent").value,
    snmp_targets: targets,
    offsite_location: document.getElementById("s_offsite_location").checked,
    offsite_rotation_interval: document.getElementById("s_offsite_rotation_interval").value,
    offsite_rotation_count: Number(document.getElementById("s_offsite_rotation_count").value || 0),
  };
  try {
    await api("/api/v1/settings", { method: "PUT", body: JSON.stringify(req) });
    document.getElementById("settingsError").textContent = "";
    loadSettings();
  } catch (err) {
    document.getElementById("settingsError").textContent = err.message;
  }
});

// ==================== Admin: Latency ====================

let latencyDefaults = null;

// Only the 7 duration fields - deliberately excludes "Enable", so "Load
// defaults" can prefill just the timing values without silently flipping
// latency simulation on/off (that's a separate, deliberate admin decision).
function fillLatencyDurations(ls) {
  document.getElementById("lt_drive_load").value = ls.drive_load || "";
  document.getElementById("lt_drive_unload").value = ls.drive_unload || "";
  document.getElementById("lt_tape_positioning").value = ls.tape_positioning || "";
  document.getElementById("lt_robot_move_tape").value = ls.robot_move_tape || "";
  document.getElementById("lt_robot_move_scan").value = ls.robot_move_scan || "";
  document.getElementById("lt_magazine_scan").value = ls.magazine_scan || "";
  document.getElementById("lt_door_action").value = ls.door_action || "";
}

function fillLatencyForm(ls) {
  document.getElementById("lt_enabled").checked = !!ls.enabled;
  fillLatencyDurations(ls);
}

async function loadLatencySettings() {
  const result = await api("/api/v1/settings/latency");
  latencyDefaults = result.defaults;
  fillLatencyForm(result.settings);
}

document.getElementById("latencyLoadDefaultsBtn").addEventListener("click", () => {
  // Only prefills the 7 duration fields for review (not "Enable" - loading
  // default timings shouldn't silently toggle simulation on/off); the admin
  // still has to click Save to actually persist these values.
  if (latencyDefaults) fillLatencyDurations(latencyDefaults);
});

document.getElementById("latencySettingsForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const req = {
    enabled: document.getElementById("lt_enabled").checked,
    drive_load: document.getElementById("lt_drive_load").value,
    drive_unload: document.getElementById("lt_drive_unload").value,
    tape_positioning: document.getElementById("lt_tape_positioning").value,
    robot_move_tape: document.getElementById("lt_robot_move_tape").value,
    robot_move_scan: document.getElementById("lt_robot_move_scan").value,
    magazine_scan: document.getElementById("lt_magazine_scan").value,
    door_action: document.getElementById("lt_door_action").value,
  };
  try {
    await api("/api/v1/settings/latency", { method: "PUT", body: JSON.stringify(req) });
    document.getElementById("latencySettingsError").textContent = "";
    loadLatencySettings();
  } catch (err) {
    document.getElementById("latencySettingsError").textContent = err.message;
  }
});

// ==================== Admin: Prometheus ====================

async function loadPrometheusSettings() {
  const result = await api("/api/v1/settings/prometheus");
  document.getElementById("prom_enabled").checked = !!result.enabled;
  document.getElementById("prom_metrics_path").textContent = result.metrics_path;
  document.getElementById("prom_status").textContent = result.enabled ? "enabled" : "disabled";
}

document.getElementById("prometheusSettingsForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const req = { enabled: document.getElementById("prom_enabled").checked };
  try {
    await api("/api/v1/settings/prometheus", { method: "PUT", body: JSON.stringify(req) });
    document.getElementById("prometheusSettingsError").textContent = "";
    loadPrometheusSettings();
  } catch (err) {
    document.getElementById("prometheusSettingsError").textContent = err.message;
  }
});

document.getElementById("downloadGrafanaDashboardBtn").addEventListener("click", () => {
  window.location.href = "/api/v1/prometheus/dashboard";
});

// ==================== Admin: Telemetry ====================

async function loadTelemetrySettings() {
  const result = await api("/api/v1/settings/telemetry");
  document.getElementById("tel_enabled").checked = !!result.enabled;
  document.getElementById("tel_endpoint").textContent = result.endpoint;
  document.getElementById("tel_status").textContent = result.enabled ? "enabled" : "disabled";
  document.getElementById("tel_preview").textContent = JSON.stringify(result.payload, null, 2);
}

document.getElementById("telemetrySettingsForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const req = { enabled: document.getElementById("tel_enabled").checked };
  try {
    await api("/api/v1/settings/telemetry", { method: "PUT", body: JSON.stringify(req) });
    document.getElementById("telemetrySettingsError").textContent = "";
    loadTelemetrySettings();
  } catch (err) {
    document.getElementById("telemetrySettingsError").textContent = err.message;
  }
});

// ==================== Admin: Security (PIN codes) ====================

async function loadSecuritySettings() {
  const result = await api("/api/v1/settings/pin");
  document.getElementById("magazinePinStatus").textContent = result.configured
    ? "A magazine PIN is currently configured."
    : "No magazine PIN is configured - every magazine opens with no prompt.";
  document.getElementById("s_magazine_pin").value = "";
}

document.getElementById("magazinePinForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const pin = document.getElementById("s_magazine_pin").value;
  try {
    await api("/api/v1/settings/pin", { method: "PUT", body: JSON.stringify({ magazine_pin: pin }) });
    document.getElementById("magazinePinError").textContent = "";
    loadSecuritySettings();
  } catch (err) {
    document.getElementById("magazinePinError").textContent = err.message;
  }
});

// ==================== Admin: Cleaning Tapes ====================

let cleaningDefaults = null;
let cleaningCurrentSettings = null;

// Only the tunables - deliberately excludes "Enable"/"Mode", mirroring
// fillLatencyDurations' own reasoning: "Load defaults" should only
// prefill the numeric/duration values for review, never silently flip
// cleaning management on/off or switch its operating mode.
function fillCleaningTunables(cs) {
  document.getElementById("cl_max_uses").value = cs.max_uses || "";
  document.getElementById("cl_mount_threshold").value = cs.mount_threshold || "";
  document.getElementById("cl_duration").value = cs.duration || "";
}

function fillCleaningForm(cs) {
  document.getElementById("cl_enabled").checked = !!cs.enabled;
  document.getElementById("cl_mode").value = cs.mode || "backup_software";
  fillCleaningTunables(cs);
}

async function loadCleaningSettings() {
  const result = await api("/api/v1/settings/cleaning");
  cleaningDefaults = result.defaults;
  cleaningCurrentSettings = result.settings;
  fillCleaningForm(result.settings);
}

document.getElementById("cleaningLoadDefaultsBtn").addEventListener("click", () => {
  if (cleaningDefaults) fillCleaningTunables(cleaningDefaults);
});

document.getElementById("cleaningSettingsForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const req = {
    enabled: document.getElementById("cl_enabled").checked,
    mode: document.getElementById("cl_mode").value,
    max_uses: parseInt(document.getElementById("cl_max_uses").value, 10) || 0,
    mount_threshold: parseInt(document.getElementById("cl_mount_threshold").value, 10) || 0,
    duration: document.getElementById("cl_duration").value,
  };
  try {
    await api("/api/v1/settings/cleaning", { method: "PUT", body: JSON.stringify(req) });
    document.getElementById("cleaningSettingsError").textContent = "";
    loadCleaningSettings();
  } catch (err) {
    document.getElementById("cleaningSettingsError").textContent = err.message;
  }
});

// locateVolumeLabel finds where barcode currently sits (slot/ioslot/
// drive/outside) by scanning a /api/v1/status snapshot, the same way
// outsideOptions/renderOutside already cross-reference volumes against
// live status elsewhere in this file.
function locateVolumeLabel(barcode, status) {
  for (const s of status.slots || []) if (s.volume && s.volume.barcode === barcode) return `slot ${s.label || s.address}`;
  for (const io of status.ioslots || []) if (io.volume && io.volume.barcode === barcode) return `ioslot ${io.label || io.address}`;
  for (const d of status.drives || []) if (d.volume && d.volume.barcode === barcode) return `drive ${d.index}`;
  for (const v of status.outside_volumes || []) if (v.barcode === barcode) return "outside";
  return "unknown";
}

async function loadCleaningTapes() {
  const [tapes, status] = await Promise.all([api("/api/v1/cleaning/tapes"), api("/api/v1/status")]);
  const grid = document.getElementById("cleaningTapesGrid");
  grid.innerHTML = "";
  const maxUses = (cleaningCurrentSettings && cleaningCurrentSettings.max_uses) || 0;
  for (const v of (tapes || []).slice().sort((a, b) => a.barcode.localeCompare(b.barcode))) {
    const card = document.createElement("div");
    card.className = "card" + (v.cleaning_state === "expired" ? " empty" : "");
    applyCleaningTooltip(card, v, maxUses);
    card.innerHTML = `${cartridgeLabelHTML(v.barcode, null)}
      <div>${v.cleaning_state || "available"}${v.cleaning_state === "expired" ? ` <span class="full">EXPIRED</span>` : ""}</div>
      <div>Location: ${locateVolumeLabel(v.barcode, status)}</div>`;
    grid.appendChild(card);
  }
  document.getElementById("newCleaningTapeBtn").disabled = (tapes || []).length >= 5;
}

async function loadCleaningAdmin() {
  await loadCleaningSettings();
  await loadCleaningTapes();
}

document.getElementById("newCleaningTapeBtn").addEventListener("click", async () => {
  try {
    await api("/api/v1/cleaning/tapes", { method: "POST", body: JSON.stringify({}) });
    loadCleaningTapes();
  } catch (err) {
    alert("Error: " + err.message);
  }
});

// ==================== Admin: Backup ====================

async function loadBackup() {
  // The whole Admin view is Admin-only now (see switchView), so this
  // section no longer renders a reduced, operator-safe subset - the
  // scheduling/browse/restore controls that used to be gated behind
  // backupAdminOnly are simply always shown here.
  document.getElementById("backupAdminOnly").hidden = false;
  const sched = await api("/api/v1/backup/schedule");
  document.getElementById("bk_interval").value = sched.interval || "";
  document.getElementById("bk_retention").value = sched.retention || "";
  document.getElementById("bk_last_run").textContent = sched.last_run
    ? "Last scheduled backup: " + new Date(sched.last_run).toLocaleString()
    : "No scheduled backup has run yet.";
  await loadBackupsList();
}

async function loadBackupsList() {
  const files = await api("/api/v1/backups");
  const tbody = document.getElementById("backupsTable");
  tbody.innerHTML = "";
  for (const f of files || []) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${f.name}</td><td>${fmtBytes(f.size_bytes)}</td><td>${new Date(f.created_at).toLocaleString()}</td>`;
    const td = document.createElement("td");
    td.appendChild(mkBtn("Download", () => {
      window.location.href = `/api/v1/backups/${encodeURIComponent(f.name)}/download`;
    }));
    td.appendChild(mkBtn("Delete", async () => {
      if (!confirm(`Delete backup ${f.name}?`)) return;
      await api(`/api/v1/backups/${encodeURIComponent(f.name)}`, { method: "DELETE" });
      loadBackupsList();
    }));
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
}

document.getElementById("downloadBackupBtn").addEventListener("click", () => {
  window.location.href = "/api/v1/backup/download";
});

document.getElementById("backupScheduleForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const interval = document.getElementById("bk_interval").value;
  const retention = Number(document.getElementById("bk_retention").value || 0);
  try {
    await api("/api/v1/backup/schedule", { method: "PUT", body: JSON.stringify({ interval, retention }) });
    document.getElementById("backupScheduleError").textContent = "";
    loadBackup();
  } catch (err) {
    document.getElementById("backupScheduleError").textContent = err.message;
  }
});

// Polls /api/v1/auth/state until the daemon answers again (Restore triggers
// a deliberate process exit so systemd restarts it against the new
// database - see internal/api/backup.go). A network error/refused
// connection is expected for the first few attempts while it's down.
async function waitForServiceRestart(maxAttempts = 40) {
  for (let i = 0; i < maxAttempts; i++) {
    await new Promise((r) => setTimeout(r, 1000));
    try {
      const res = await fetch("/api/v1/auth/state", { credentials: "same-origin" });
      if (res.ok) return true;
    } catch (e) {
      // still restarting; keep polling
    }
  }
  return false;
}

// Shared by the Admin > Backup restore form and the wizard step 1 "restore
// instead" option.
async function submitRestore(file, statusEl, errorEl) {
  errorEl.textContent = "";
  statusEl.textContent = "Uploading backup...";
  try {
    const data = await file.arrayBuffer();
    await api("/api/v1/restore", { method: "POST", headers: { "Content-Type": "application/octet-stream" }, body: data });
  } catch (err) {
    errorEl.textContent = err.message;
    statusEl.textContent = "";
    return;
  }
  statusEl.textContent = "Restore accepted. The service is restarting - waiting for it to come back (this can take a few seconds)...";
  const back = await waitForServiceRestart();
  if (back) {
    statusEl.textContent = "Service is back. You will need to sign in again - reloading...";
    setTimeout(boot, 500);
  } else {
    statusEl.textContent = "Timed out waiting for the service to come back. Reload the page manually once it's back up.";
  }
}

document.getElementById("restoreForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const file = document.getElementById("restoreFile").files[0];
  const errorEl = document.getElementById("restoreError");
  if (!file) {
    errorEl.textContent = "Choose a backup file first.";
    return;
  }
  if (!confirm("This replaces the entire database and restarts the service. Continue?")) return;
  await submitRestore(file, document.getElementById("restoreStatus"), errorEl);
});

// ==================== Admin: Reset ====================

let resetExpectedName = "RESET";

async function loadReset() {
  document.getElementById("resetStatus").textContent = "";
  document.getElementById("resetError").textContent = "";
  document.getElementById("resetConfirmName").value = "";
  document.getElementById("resetDeleteVolumes").checked = false;
  // Read the VTL name from /api/v1/status (live, from the running Library)
  // rather than /api/v1/settings - the latter's cached copy of vtl_name
  // can go stale right after the wizard sets it (same class of bug the
  // Latency page hit - see Settings.CurrentLatency's doc comment).
  const st = await api("/api/v1/status");
  resetExpectedName = st && st.name ? st.name : "RESET";
  document.getElementById("resetExpectedName").textContent = resetExpectedName;
  updateResetSubmitEnabled();
}

function updateResetSubmitEnabled() {
  const typed = document.getElementById("resetConfirmName").value;
  document.getElementById("resetSubmitBtn").disabled = typed !== resetExpectedName;
}

document.getElementById("resetConfirmName").addEventListener("input", updateResetSubmitEnabled);

async function submitReset(confirmName, deleteVolumes, statusEl, errorEl) {
  errorEl.textContent = "";
  statusEl.textContent = "Resetting...";
  try {
    await api("/api/v1/reset", { method: "POST", body: JSON.stringify({ confirm_name: confirmName, delete_volumes: deleteVolumes }) });
  } catch (err) {
    errorEl.textContent = err.message;
    statusEl.textContent = "";
    return;
  }
  statusEl.textContent = "Reset accepted. The service is restarting - waiting for it to come back (this can take a few seconds)...";
  const back = await waitForServiceRestart();
  if (back) {
    statusEl.textContent = "Service is back. Reloading...";
    setTimeout(boot, 500);
  } else {
    statusEl.textContent = "Timed out waiting for the service to come back. Reload the page manually once it's back up.";
  }
}

document.getElementById("resetForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const confirmName = document.getElementById("resetConfirmName").value;
  const deleteVolumes = document.getElementById("resetDeleteVolumes").checked;
  const errorEl = document.getElementById("resetError");
  if (confirmName !== resetExpectedName) {
    errorEl.textContent = "Typed name does not match.";
    return;
  }
  const warning = deleteVolumes
    ? "This permanently deletes the entire VTL, all users/API tokens, AND every cartridge file on disk. This cannot be undone except from the automatic safety backup. Continue?"
    : "This permanently deletes the entire VTL and all users/API tokens, and restarts the service. Continue?";
  if (!confirm(warning)) return;
  await submitReset(confirmName, deleteVolumes, document.getElementById("resetStatus"), errorEl);
});

// ==================== Admin: Drive Types ====================

async function loadDriveTypes() {
  const dts = await api("/api/v1/drive-types");
  const tbody = document.getElementById("driveTypesTable");
  tbody.innerHTML = "";
  for (const dt of dts || []) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${dt.name}</td><td>${dt.model || ""}</td><td>${dt.generation || ""}</td><td>${dt.speed}</td><td>${dt.capacity}</td><td>${dt.description}</td>`;
    const td = document.createElement("td");
    td.appendChild(mkBtn("Edit", async () => {
      const v = await openDialog("Edit drive type " + dt.name, [
        { name: "model", label: "Model", value: dt.model || "" },
        { name: "generation", label: "Generation", value: dt.generation || "" },
        { name: "speed", label: "Speed", value: dt.speed },
        { name: "capacity", label: "Capacity", value: dt.capacity },
        { name: "description", label: "Description", value: dt.description },
      ]);
      if (!v) return;
      await api(`/api/v1/drive-types/${encodeURIComponent(dt.name)}`, { method: "PUT", body: JSON.stringify({ name: dt.name, ...v }) });
      loadDriveTypes();
    }));
    td.appendChild(mkBtn("Delete", async () => {
      if (!confirm(`Delete drive type ${dt.name}?`)) return;
      await api(`/api/v1/drive-types/${encodeURIComponent(dt.name)}`, { method: "DELETE" });
      loadDriveTypes();
    }));
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
}

document.getElementById("newDriveTypeBtn").addEventListener("click", async () => {
  const v = await openDialog("New drive type", [
    { name: "name", label: "Name" },
    { name: "model", label: "Model", placeholder: "IBM TS1160" },
    { name: "generation", label: "Generation", placeholder: "LTO-9" },
    { name: "speed", label: "Speed", placeholder: "300MB/s" },
    { name: "capacity", label: "Capacity", placeholder: "12TB" },
    { name: "description", label: "Description" },
  ]);
  if (!v || !v.name) return;
  try {
    await api("/api/v1/drive-types", { method: "POST", body: JSON.stringify(v) });
    loadDriveTypes();
  } catch (e) {
    alert(e.message);
  }
});

// ==================== Admin: Tape Types ====================

const barcodeFamilyOptions = [
  { value: "lto", label: "LTO" },
  { value: "dlt", label: "DLT" },
  { value: "sdlt", label: "SDLT" },
  { value: "dds", label: "DDS/DAT" },
  { value: "ait", label: "AIT/SAIT" },
  { value: "3592", label: "IBM 3592" },
  { value: "generic", label: "Generic (non-physical)" },
];

function tapeTypeFields(tt) {
  return [
    { name: "capacity", label: "Capacity", value: tt.capacity, placeholder: "12TB" },
    { name: "description", label: "Description", value: tt.description },
    { name: "barcode_family", label: "Barcode Family", type: "select", value: tt.barcode_family, options: barcodeFamilyOptions },
    { name: "media_id", label: "Media ID (e.g. L8)", value: tt.media_id },
    { name: "volser_length", label: "Volume ID Length", type: "number", value: String(tt.volser_length || 6) },
  ];
}

function formatBarcodeFormat(tt) {
  const family = (barcodeFamilyOptions.find((f) => f.value === tt.barcode_family) || {}).label || tt.barcode_family;
  return tt.media_id ? `${family} (${tt.media_id})` : family;
}

async function loadTapeTypes() {
  const tts = await api("/api/v1/tape-types");
  const tbody = document.getElementById("tapeTypesTable");
  tbody.innerHTML = "";
  for (const tt of tts || []) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${tt.name}</td><td>${tt.capacity}</td><td>${formatBarcodeFormat(tt)}</td><td>${tt.description}</td>`;
    const td = document.createElement("td");
    td.appendChild(mkBtn("Edit", async () => {
      const v = await openDialog("Edit tape type " + tt.name, tapeTypeFields(tt));
      if (!v) return;
      try {
        v.volser_length = parseInt(v.volser_length, 10) || 0;
        await api(`/api/v1/tape-types/${encodeURIComponent(tt.name)}`, { method: "PUT", body: JSON.stringify({ name: tt.name, ...v }) });
        loadTapeTypes();
      } catch (e) {
        alert(e.message);
      }
    }));
    td.appendChild(mkBtn("Delete", async () => {
      if (!confirm(`Delete tape type ${tt.name}?`)) return;
      await api(`/api/v1/tape-types/${encodeURIComponent(tt.name)}`, { method: "DELETE" });
      loadTapeTypes();
    }));
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
  loadTapeSetFamilyMap(); // barcode family badges on the dashboard depend on tape types too
  loadBackupLastRun();
}

document.getElementById("newTapeTypeBtn").addEventListener("click", async () => {
  const v = await openDialog("New tape type", [
    { name: "name", label: "Name" },
    ...tapeTypeFields({ barcode_family: "lto", volser_length: 6 }),
  ]);
  if (!v || !v.name) return;
  try {
    v.volser_length = parseInt(v.volser_length, 10) || 0;
    await api("/api/v1/tape-types", { method: "POST", body: JSON.stringify(v) });
    loadTapeTypes();
  } catch (e) {
    alert(e.message);
  }
});

// ==================== Admin: Tape Sets ====================

async function loadTapeSets() {
  const [sets, vols] = await Promise.all([api("/api/v1/tape-sets"), api("/api/v1/volumes")]);
  const cartridgeCount = {};
  for (const v of vols || []) {
    if (v.tape_set) cartridgeCount[v.tape_set] = (cartridgeCount[v.tape_set] || 0) + 1;
  }
  const tbody = document.getElementById("tapeSetsTable");
  tbody.innerHTML = "";
  for (const ts of sets || []) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${ts.name}</td><td>${ts.tape_type}</td><td>${ts.storage_folder}</td><td>${cartridgeCount[ts.name] || 0}</td>`;
    const td = document.createElement("td");
    td.appendChild(mkBtn("Edit", async () => {
      const options = await loadTapeTypeOptions();
      const v = await openDialog("Edit tape set " + ts.name, [
        { name: "tape_type", label: "Tape Type", type: "select", value: ts.tape_type, options },
        { name: "storage_folder", label: "Storage Folder", type: "folderpicker", value: ts.storage_folder },
      ]);
      if (!v) return;
      try {
        await api(`/api/v1/tape-sets/${encodeURIComponent(ts.name)}`, { method: "PUT", body: JSON.stringify({ name: ts.name, ...v }) });
        loadTapeSets();
      } catch (e) {
        alert(e.message);
      }
    }));
    td.appendChild(mkBtn("Add tapes", async () => {
      const v = await openDialog("Add tapes to " + ts.name, [
        { name: "mode", label: "Mode", type: "select", options: [
          { value: "auto", label: "Auto-generate barcodes (bulk)" },
          { value: "manual", label: "Enter barcode manually (single)" },
        ] },
        { name: "count", label: "Count (auto mode)", type: "number", value: "1" },
        { name: "barcode", label: "Barcode (manual mode)" },
      ]);
      if (!v) return;
      try {
        const body = v.mode === "manual" ? { barcode: v.barcode } : { count: parseInt(v.count, 10) || 1 };
        const created = await api(`/api/v1/tape-sets/${encodeURIComponent(ts.name)}/tapes`, { method: "POST", body: JSON.stringify(body) });
        alert(`Added ${(created || []).length} cartridge(s): ${(created || []).map((c) => c.barcode).join(", ")}`);
        loadTapeSets();
      } catch (e) {
        alert(e.message);
      }
    }));
    td.appendChild(mkBtn("Delete", async () => {
      if (!confirm(`Delete tape set ${ts.name}?`)) return;
      try {
        await api(`/api/v1/tape-sets/${encodeURIComponent(ts.name)}`, { method: "DELETE" });
        loadTapeSets();
      } catch (e) {
        alert(e.message);
      }
    }));
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
  loadTapeSetFamilyMap();
  loadBackupLastRun();
}

document.getElementById("newTapeSetBtn").addEventListener("click", async () => {
  const options = await loadTapeTypeOptions();
  if (!options.length) {
    alert("Create a tape type first (Tape Types tab).");
    return;
  }
  const v = await openDialog("New tape set", [
    { name: "name", label: "Name" },
    { name: "tape_type", label: "Tape Type", type: "select", options },
    { name: "storage_folder", label: "Storage Folder", type: "folderpicker", placeholder: "/var/lib/gotochanger/tapesets/..." },
    { name: "initial_tape_count", label: "Number of tapes", type: "number", value: "1" },
  ]);
  if (!v || !v.name) return;
  const err = validateTapeSetInput({ name: v.name, tape_type: v.tape_type, storage_folder: v.storage_folder, tape_count: v.initial_tape_count });
  if (err) { alert(err); return; }
  try {
    const res = await api("/api/v1/tape-sets", { method: "POST", body: JSON.stringify({ ...v, initial_tape_count: parseInt(v.initial_tape_count, 10) }) });
    alert(`Created tape set ${res.name} with ${(res.cartridges || []).length} cartridge(s).`);
    loadTapeSets();
  } catch (e) {
    alert(e.message);
  }
});

// ==================== Admin: Drives ====================

// Builds a physical-drive-index -> discovered real SCSI path lookup from
// GET /api/v1/kernel-mode/devices' report set (see api.KernelModeDeviceReport -
// keyed by instance/logical-library name, each holding its own drives map
// keyed by physical drive index). A drive belongs to at most one logical
// library, so at most one instance's report should ever contain a given
// index - if more than one somehow does (a stale report from before a
// reassignment that hasn't been cleared yet), the last one wins, which is
// no worse than showing nothing.
function buildDrivePathIndex(kernelModeDevices) {
  const byIndex = {};
  for (const [instance, report] of Object.entries(kernelModeDevices || {})) {
    for (const [driveIndex, paths] of Object.entries(report.drives || {})) {
      byIndex[driveIndex] = { ...paths, instance };
    }
  }
  return byIndex;
}

// Renders a drive's "Device Path" cell: the real SCSI path (kernel mode,
// once gotochanger-tcmud has reported it) takes priority over the
// changer-script-mode symlink path, since kernel mode never reads/writes
// that symlink at all - showing it as if it mattered would be misleading
// once real SCSI devices are what Bareos actually talks to. Within the
// kernel-mode path itself, the stable /dev/tape/by-id/... path (see
// internal/scsi/vpd.go) is preferred over the raw /dev/sgN/dev/nstN
// number, since only the by-id path survives a gotochanger-tcmud
// restart - falls back to the raw path when udev hasn't reported a
// stable symlink yet (e.g. right after startup).
function driveDevicePathCell(d, drivePaths) {
  const kp = drivePaths[d.index];
  if (!kp) return d.device_path;
  const path = kp.stable_tape || kp.stable_generic || `${kp.generic}${kp.tape ? ` / ${kp.tape}` : ""}`;
  return `${path} <span class="hint">(kernel mode, ${kp.instance})</span>`;
}

async function loadDrives() {
  const [drives, driveTypes, kernelModeDevices] = await Promise.all([api("/api/v1/drives"), api("/api/v1/drive-types"), api("/api/v1/kernel-mode/devices")]);
  const drivePaths = buildDrivePathIndex(kernelModeDevices);
  const driveTypeOptions = [{ value: "", label: "(none)" }, ...(driveTypes || []).map((dt) => ({ value: dt.name, label: dt.name }))];
  const tbody = document.getElementById("drivesTable");
  tbody.innerHTML = "";
  for (const d of drives || []) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${d.index}</td><td>${driveDevicePathCell(d, drivePaths)}</td><td>${d.drive_type || ""}</td><td>${d.model || ""}</td><td>${d.generation || ""}</td><td>${d.capacity || ""}</td>`;
    const td = document.createElement("td");
    td.appendChild(mkBtn("Edit", async () => {
      const v = await openDialog("Edit drive " + d.index, [
        { name: "device_path", label: "Device Path", value: d.device_path },
        { name: "drive_type", label: "Drive Type", type: "select", options: driveTypeOptions, value: d.drive_type || "" },
      ]);
      if (!v) return;
      await api(`/api/v1/drives/${d.index}`, { method: "PUT", body: JSON.stringify({ device_path: v.device_path, drive_type: v.drive_type }) });
      loadDrives();
    }));
    td.appendChild(mkBtn("Delete", async () => {
      if (!confirm(`Delete drive ${d.index}? It must not have a volume loaded.`)) return;
      await api(`/api/v1/drives/${d.index}`, { method: "DELETE" });
      loadDrives();
    }));
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
}

document.getElementById("newDriveBtn").addEventListener("click", async () => {
  const driveTypes = await api("/api/v1/drive-types");
  const driveTypeOptions = [{ value: "", label: "(none)" }, ...(driveTypes || []).map((dt) => ({ value: dt.name, label: dt.name }))];
  const v = await openDialog("New drive", [
    { name: "device_path", label: "Device Path (blank = auto-generate)", placeholder: "/var/lib/gotochanger/drives/driveN" },
    { name: "drive_type", label: "Drive Type", type: "select", options: driveTypeOptions },
  ]);
  if (!v) return;
  try {
    await api("/api/v1/drives", { method: "POST", body: JSON.stringify({ device_path: v.device_path || "", drive_type: v.drive_type }) });
    loadDrives();
  } catch (e) {
    alert(e.message);
  }
});

// ==================== Admin: Magazines ====================

async function loadMagazines() {
  const mags = await api("/api/v1/magazines");
  const tbody = document.getElementById("magazinesTable");
  tbody.innerHTML = "";
  for (const m of mags || []) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${m.id}</td><td>${m.slots}</td>`;
    const td = document.createElement("td");
    td.appendChild(mkBtn("Edit", async () => {
      const v = await openDialog("Edit magazine " + m.id, [
        { name: "slots", label: "Slots (5-20, step 5)", type: "number", value: m.slots },
      ]);
      if (!v) return;
      await api(`/api/v1/magazines/${encodeURIComponent(m.id)}`, { method: "PUT", body: JSON.stringify({ id: m.id, slots: Number(v.slots) }) });
      loadMagazines();
    }));
    td.appendChild(mkBtn("Delete", async () => {
      if (!confirm(`Delete magazine ${m.id}? Its slots must be empty.`)) return;
      await api(`/api/v1/magazines/${encodeURIComponent(m.id)}`, { method: "DELETE" });
      loadMagazines();
    }));
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
}

document.getElementById("newMagazineBtn").addEventListener("click", async () => {
  const v = await openDialog("New magazine", [
    { name: "id", label: "ID", placeholder: "Magazine2" },
    { name: "slots", label: "Slots (5-20, step 5)", type: "number", value: "5" },
  ]);
  if (!v || !v.id) return;
  try {
    await api("/api/v1/magazines", { method: "POST", body: JSON.stringify({ id: v.id, slots: Number(v.slots) }) });
    loadMagazines();
  } catch (e) {
    alert(e.message);
  }
});

// ==================== Admin: Mailboxes ====================

async function loadMailboxes() {
  const mbs = await api("/api/v1/mailboxes");
  const tbody = document.getElementById("mailboxesTable");
  tbody.innerHTML = "";
  for (const m of mbs || []) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${m.id}</td><td>${m.slots}</td><td>${m.pin_set ? "Set" : "—"}</td>`;
    const td = document.createElement("td");
    td.appendChild(mkBtn("Edit", async () => {
      const v = await openDialog("Edit mailbox " + m.id, [
        { name: "slots", label: "Slots (1-5)", type: "number", value: m.slots },
        { name: "pin", label: "New 4-digit PIN (blank = leave unchanged)", type: "password", placeholder: "1234" },
      ]);
      if (!v) return;
      const body = { id: m.id, slots: Number(v.slots) };
      // Blank means "leave unchanged" here, not "clear" - the pin key is
      // omitted entirely rather than sent as "", which the backend would
      // read as an explicit clear (see resolveMailboxPINHash/mailboxRequest's
      // pointer semantics). Use the separate "Clear PIN" button to clear.
      if (v.pin) body.pin = v.pin;
      await api(`/api/v1/mailboxes/${encodeURIComponent(m.id)}`, { method: "PUT", body: JSON.stringify(body) });
      loadMailboxes();
    }));
    if (m.pin_set) {
      td.appendChild(mkBtn("Clear PIN", async () => {
        if (!confirm(`Clear the PIN for mailbox ${m.id}?`)) return;
        await api(`/api/v1/mailboxes/${encodeURIComponent(m.id)}`, { method: "PUT", body: JSON.stringify({ id: m.id, slots: m.slots, pin: "" }) });
        loadMailboxes();
      }));
    }
    td.appendChild(mkBtn("Delete", async () => {
      if (!confirm(`Delete mailbox ${m.id}? Its slots must be empty.`)) return;
      await api(`/api/v1/mailboxes/${encodeURIComponent(m.id)}`, { method: "DELETE" });
      loadMailboxes();
    }));
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
}

document.getElementById("newMailboxBtn").addEventListener("click", async () => {
  const v = await openDialog("New mailbox", [
    { name: "id", label: "ID", placeholder: "Mailbox2" },
    { name: "slots", label: "Slots (1-5)", type: "number", value: "1" },
    { name: "pin", label: "PIN (optional)", type: "password", placeholder: "1234" },
  ]);
  if (!v || !v.id) return;
  try {
    const body = { id: v.id, slots: Number(v.slots) };
    if (v.pin) body.pin = v.pin;
    await api("/api/v1/mailboxes", { method: "POST", body: JSON.stringify(body) });
    loadMailboxes();
  } catch (e) {
    alert(e.message);
  }
});

// ==================== Admin: Logical Libraries ====================

async function logicalLibraryPickerData() {
  const [status, mags, mbs, logicalLibraries, kernelModeDevices] = await Promise.all([
    api("/api/v1/status"),
    api("/api/v1/magazines"),
    api("/api/v1/mailboxes"),
    api("/api/v1/logical-libraries"),
    api("/api/v1/kernel-mode/devices"),
  ]);
  return {
    drives: status.drives || [],
    magazines: mags || [],
    mailboxes: mbs || [],
    logicalLibraries: logicalLibraries || [],
    // Real SCSI paths a running gotochanger-tcmud instance has reported for
    // itself, so the drive chip labels below show the same path Admin >
    // Drives does instead of the changer-script-mode symlink kernel mode
    // never actually uses - found stale by the user right after the
    // equivalent fix landed for Admin > Drives/Logical Libraries' own
    // tables, which don't go through this shared picker at all.
    drivePaths: buildDrivePathIndex(kernelModeDevices),
  };
}

// Dispatches to the Changer-Command-Script or kernel-mode variant based
// on the deployment's operational_mode - the two need genuinely
// different Bareos config shapes (a script-driven file-backed changer
// vs. a real SCSI autochanger), not just cosmetic differences.
function buildBareosConfig(lib, vtlName, operationalMode, kernelModeReport) {
  return operationalMode === "kernel"
    ? buildBareosConfigKernelMode(lib, vtlName, kernelModeReport)
    : buildBareosConfigChangerMode(lib, vtlName);
}

// Renders the Bareos storage-daemon config (Autochanger + one Device per
// drive) needed to point a Bareos Autochanger at this logical library,
// plus a director-side Storage resource skeleton. Mirrors the "Bareos
// integration" example in README.md, filled in with this library's real
// drive indices/device paths and its --logical-library flag.
function buildBareosConfigChangerMode(lib, vtlName) {
  const drives = (lib.drives || []).slice().sort((a, b) => a.index - b.index);
  const lines = [];
  if (!drives.length) {
    lines.push("# WARNING: this logical library has no drives assigned yet -");
    lines.push("# Bareos needs at least one Device to use it. Assign a drive via Edit first.");
    lines.push("");
  }
  lines.push(`# Storage daemon config for logical library "${lib.name}"${vtlName ? ` (VTL: ${vtlName})` : ""}`);
  lines.push(`# ${drives.length} drive(s), ${(lib.slots || []).length} storage slot(s), ${(lib.io_slots || []).length} I/O slot(s)`);
  lines.push("");
  lines.push("Autochanger {");
  lines.push(`  Name = ${lib.name}`);
  lines.push(`  Device = ${drives.map((d) => `Drive${d.index}`).join(", ")}`);
  lines.push(`  Changer Device = /dev/null              # unused, kept for compatibility`);
  lines.push(`  Changer Command = "/usr/bin/gotochanger-changer %c %o %S %a %d %V --logical-library=${lib.name}"`);
  lines.push("}");
  // Drive Index is required whenever an Autochanger has more than one
  // Device - Bareos defaults it to 0 for any Device resource that
  // doesn't set it explicitly, so two or more drives without this line
  // all silently collapse to "drive 0" from Bareos's own point of view
  // (Device Name/Archive Device are irrelevant to that; only the
  // directive counts). It must be the drive's 0-based position within
  // *this* Autochanger's own Device list - i.e. its rank here, not
  // gotochangerd's global physical drive index - matching exactly what
  // gotochanger-changer's --logical-library scoping computes on the
  // receiving end (see presentedAddressing in cmd/gotochanger-changer).
  drives.forEach((d, presentedIndex) => {
    lines.push("");
    lines.push("Device {");
    lines.push(`  Name = Drive${d.index}`);
    lines.push(`  Drive Index = ${presentedIndex}`);
    lines.push(`  Media Type = File`);
    lines.push(`  Archive Device = ${d.device_path}`);
    lines.push(`  Device Type = File`);
    lines.push(`  AutomaticMount = yes`);
    lines.push(`  RemovableMedia = yes`);
    lines.push(`  AutoChanger = yes`);
    lines.push("}");
  });
  lines.push("");
  lines.push("# Director-side (bareos-dir.conf) - fill in Address/SD Port/Password");
  lines.push("# to match the Storage Daemon this logical library is running on:");
  lines.push("Storage {");
  lines.push(`  Name = ${lib.name}`);
  lines.push(`  Address = <storage-daemon-hostname>`);
  lines.push(`  SD Port = <storage-daemon-port>`);
  lines.push(`  Password = "<matches the Storage Daemon's password for this Storage>"`);
  lines.push(`  Device = ${lib.name}`);
  lines.push(`  Media Type = File`);
  lines.push(`  Autochanger = yes`);
  lines.push(`  Maximum Concurrent Jobs = ${drives.length}`);
  lines.push("}");
  return lines.join("\n");
}

// Kernel mode exposes a *real* SCSI medium changer + tape drives (see
// "Kernel Mode Setup"/gotochanger-tcmud), so Bareos talks to them exactly
// like any other physical tape library - via its own bundled mtx-changer
// script, not gotochanger-changer (that's the file-backed changer-script
// mode's own translator, irrelevant once real SCSI is involved).
//
// Unlike changer-script mode's device_path (an admin-chosen, stored
// value), the real /dev/sg*/tape device node the kernel assigns is only
// knowable once gotochanger-tcmud@<name> has actually run and reported
// itself (see kernelModeReport - GET /api/v1/kernel-mode/devices, sent by
// cmd/gotochanger-tcmud after startup). When that report exists, this
// prints the real paths directly; otherwise it falls back to a
// placeholder template with the lookup commands to fill them in by hand.
function buildBareosConfigKernelMode(lib, vtlName, kernelModeReport) {
  const drives = (lib.drives || []).slice().sort((a, b) => a.index - b.index);
  const instance = lib.name;
  const mediaType = (drives[0] && drives[0].drive_type) || "Tape";
  const lines = [];
  if (!drives.length) {
    lines.push("# WARNING: this logical library has no drives assigned yet -");
    lines.push("# Bareos needs at least one Device to use it. Assign a drive via Edit first.");
    lines.push("");
  }
  lines.push(`# Storage daemon config for logical library "${lib.name}"${vtlName ? ` (VTL: ${vtlName})` : ""} - kernel mode (real SCSI via TCMU/LIO)`);
  lines.push(`# ${drives.length} drive(s), ${(lib.slots || []).length} storage slot(s), ${(lib.io_slots || []).length} I/O slot(s)`);
  lines.push("#");
  if (kernelModeReport) {
    lines.push("# Real device paths below, as last reported by the running");
    lines.push(`# gotochanger-tcmud@${instance}.service instance - preferring the stable`);
    lines.push("# /dev/tape/by-id/... path (survives a gotochanger-tcmud restart) over");
    lines.push("# the raw /dev/sgN/dev/nstN path (does not) when both are known.");
  } else {
    lines.push(`# Requires: systemctl enable --now gotochanger-tcmud@${instance}.service`);
    lines.push("# (see the \"Kernel Mode Setup\" button) BEFORE the device paths below");
    lines.push("# exist. Once running, this dialog will show the real, stable");
    lines.push("# /dev/tape/by-id/... paths automatically - no manual lookup needed.");
    lines.push("# If they're still missing after that, find the raw paths with:");
    lines.push("#   lsscsi -g | grep GOTOCHNG");
    lines.push("#   sg_inq /dev/sgN            # confirms changer vs. a specific drive");
  }
  lines.push("");
  lines.push("Autochanger {");
  lines.push(`  Name = ${lib.name}`);
  lines.push(`  Device = ${drives.map((d) => `Drive${d.index}`).join(", ")}`);
  lines.push(`  Changer Device = ${(kernelModeReport && (kernelModeReport.changer_stable || kernelModeReport.changer)) || "<this logical library's changer /dev/sgN>"}`);
  lines.push(`  Changer Command = "/usr/lib/bareos/scripts/mtx-changer %c %o %S %a %d"`);
  lines.push("}");
  // Drive Index: same convention as changer-script mode (see
  // buildBareosConfigChangerMode's comment on this) - a real SCSI
  // autochanger's Device Index directive works identically.
  drives.forEach((d, presentedIndex) => {
    const reportedPaths = kernelModeReport && kernelModeReport.drives && kernelModeReport.drives[d.index];
    // Bareos conventionally wants the non-rewinding tape device
    // (Tape - e.g. /dev/nst0) over the bare SCSI generic passthrough
    // (Generic - /dev/sg5); the tape device only exists when the
    // kernel's "st" driver happens to be loaded (see
    // internal/tcmu.DiscoverDevicePaths's own doc comment), so fall back
    // to Generic when it isn't. Within each, the stable /dev/tape/by-id/...
    // path (survives a gotochanger-tcmud restart, see driveDevicePathCell's
    // own comment) is preferred over the raw path.
    const archiveDevice = reportedPaths
      ? reportedPaths.stable_tape || reportedPaths.tape || reportedPaths.stable_generic || reportedPaths.generic
      : "<this drive's tape device, e.g. /dev/nstN or /dev/stN>";
    lines.push("");
    lines.push("Device {");
    lines.push(`  Name = Drive${d.index}`);
    lines.push(`  Drive Index = ${presentedIndex}`);
    lines.push(`  Media Type = ${d.drive_type || "Tape"}`);
    lines.push(`  Archive Device = ${archiveDevice}`);
    lines.push(`  Device Type = Tape`);
    lines.push(`  AutomaticMount = yes`);
    lines.push(`  RemovableMedia = yes`);
    lines.push(`  AutoChanger = yes`);
    lines.push("}");
  });
  lines.push("");
  lines.push("# Director-side (bareos-dir.conf) - fill in Address/SD Port/Password");
  lines.push("# to match the Storage Daemon this logical library is running on:");
  lines.push("Storage {");
  lines.push(`  Name = ${lib.name}`);
  lines.push(`  Address = <storage-daemon-hostname>`);
  lines.push(`  SD Port = <storage-daemon-port>`);
  lines.push(`  Password = "<matches the Storage Daemon's password for this Storage>"`);
  lines.push(`  Device = ${lib.name}`);
  lines.push(`  Media Type = ${mediaType}`);
  lines.push(`  Autochanger = yes`);
  lines.push(`  Maximum Concurrent Jobs = ${drives.length}`);
  lines.push("}");
  return lines.join("\n");
}

const bareosConfigDlg = document.getElementById("bareosConfigDialog");
const bareosConfigTitle = document.getElementById("bareosConfigTitle");
const bareosConfigText = document.getElementById("bareosConfigText");

function showBareosConfig(lib, vtlName, operationalMode, kernelModeReport) {
  bareosConfigTitle.textContent = `Bareos configuration - ${lib.name}`;
  bareosConfigText.textContent = buildBareosConfig(lib, vtlName, operationalMode, kernelModeReport);
  bareosConfigDlg.showModal();
}

document.getElementById("bareosConfigCloseBtn").addEventListener("click", () => bareosConfigDlg.close());
const bareosConfigCopyBtn = document.getElementById("bareosConfigCopyBtn");
bareosConfigCopyBtn.addEventListener("click", async () => {
  // Capture the button before the await - e.currentTarget is nulled out
  // once the event's synchronous dispatch finishes, which is before an
  // async handler resumes.
  const original = bareosConfigCopyBtn.textContent;
  try {
    await navigator.clipboard.writeText(bareosConfigText.textContent);
    bareosConfigCopyBtn.textContent = "Copied!";
    setTimeout(() => { bareosConfigCopyBtn.textContent = original; }, 1500);
  } catch (err) {
    alert("Could not copy to clipboard: " + err.message);
  }
});

// Renders the exact `systemctl enable --now gotochanger-tcmud@...` command
// for one logical library (or the whole physical library, when lib is
// null - the "@default" instance, matching systemd/
// gotochanger-tcmud@.service's own convention for a deployment with
// zero/one logical libraries). Purely a reference/copy-paste panel, like
// buildBareosConfig - it doesn't check live availability (see
// kernelModeAvailable, used by the wizard instead), since the command is
// equally useful to see ahead of actually installing the package.
function buildKernelModeSetup(lib, vtlName) {
  const instance = lib ? lib.name : "default";
  const flag = lib ? ` --logical-library=${lib.name}` : "";
  const lines = [];
  lines.push(`# Kernel-mode (TCMU/LIO) setup for ${lib ? `logical library "${lib.name}"` : "the whole physical library"}${vtlName ? ` (VTL: ${vtlName})` : ""}`);
  lines.push("# Requires the gotochanger-kernel package installed and root.");
  lines.push("");
  lines.push(`sudo systemctl enable --now gotochanger-tcmud@${instance}.service`);
  lines.push("");
  lines.push("# Equivalent to running directly:");
  lines.push(`#   gotochanger-tcmud${flag}`);
  lines.push("");
  lines.push("# Once running, a real SCSI medium changer and one tape drive per");
  lines.push("# configured drive appear under /dev/sg* (confirm with lsscsi/sg_inq).");
  return lines.join("\n");
}

const kernelModeDlg = document.getElementById("kernelModeDialog");
const kernelModeTitle = document.getElementById("kernelModeTitle");
const kernelModeText = document.getElementById("kernelModeText");

function showKernelModeSetup(lib, vtlName) {
  kernelModeTitle.textContent = `Kernel mode setup - ${lib ? lib.name : "whole physical library"}`;
  kernelModeText.textContent = buildKernelModeSetup(lib, vtlName);
  kernelModeDlg.showModal();
}

document.getElementById("kernelModeCloseBtn").addEventListener("click", () => kernelModeDlg.close());
const kernelModeCopyBtn = document.getElementById("kernelModeCopyBtn");
kernelModeCopyBtn.addEventListener("click", async () => {
  const original = kernelModeCopyBtn.textContent;
  try {
    await navigator.clipboard.writeText(kernelModeText.textContent);
    kernelModeCopyBtn.textContent = "Copied!";
    setTimeout(() => { kernelModeCopyBtn.textContent = original; }, 1500);
  } catch (err) {
    alert("Could not copy to clipboard: " + err.message);
  }
});
document.getElementById("wholeLibraryKernelModeBtn").addEventListener("click", async () => {
  const status = await api("/api/v1/status");
  showKernelModeSetup(null, status.name);
});

// Derives {drives, magazines, mailboxes} straight from a
// /api/v1/logical-libraries entry's own drives/slots/io_slots - the exact
// shape Add/Update already send, and what the Edit dialog's assignment
// board field uses to seed its "currently in this library" pane.
function logicalLibraryMembership(lib) {
  const drives = (lib.drives || []).map((d) => d.index);
  const magazines = [];
  for (const s of lib.slots || []) if (s.magazine_id && !magazines.includes(s.magazine_id)) magazines.push(s.magazine_id);
  const mailboxes = [];
  for (const io of lib.io_slots || []) if (io.mailbox_id && !mailboxes.includes(io.mailbox_id)) mailboxes.push(io.mailbox_id);
  return { drives, magazines, mailboxes };
}

// Builds the New/Edit logical library dialog's assignment-board field
// descriptor: one "kind" entry per drive/magazine/mailbox category, each
// bundling its catalog, id/label accessors, cross-library ownership map
// (items already claimed by a *different* library, shown but not
// draggable), and the initial set of ids currently in this library
// (empty for New). Consumed by openDialog's "assignmentboard" field type.
function logicalLibraryAssignmentKinds(picker, owners, current) {
  return [
    { kind: "drive", field: "drives", label: "Drives", items: picker.drives, idOf: (d) => d.index, labelOf: (d) => `Drive ${d.index} (${driveDevicePathCell(d, picker.drivePaths)})`, owner: owners.driveOwner, initial: current.drives },
    { kind: "magazine", field: "magazines", label: "Magazines", items: picker.magazines, idOf: (m) => m.id, labelOf: (m) => m.id, owner: owners.magazineOwner, initial: current.magazines },
    { kind: "mailbox", field: "mailboxes", label: "Mailboxes", items: picker.mailboxes, idOf: (m) => m.id, labelOf: (m) => m.id, owner: owners.mailboxOwner, initial: current.mailboxes },
  ];
}

async function loadLogicalLibraries() {
  // wizard state is the only place operational_mode is exposed today
  // (see api.WizardResponse) - fetched here purely to pick the right
  // Bareos Config variant, unrelated to actual wizard progress.
  const [libs, status, wizardState, kernelModeDevices] = await Promise.all([api("/api/v1/logical-libraries"), api("/api/v1/status"), api("/api/v1/wizard"), api("/api/v1/kernel-mode/devices")]);
  const operationalMode = wizardState.operational_mode;
  const tbody = document.getElementById("logicalLibrariesTable");
  tbody.innerHTML = "";
  for (const lib of libs || []) {
    const tr = document.createElement("tr");
    const mailboxCount = new Set((lib.io_slots || []).map((io) => io.mailbox_id).filter(Boolean)).size;
    // A logical library's own instance reports under its own name (see
    // kernelModeInstanceName in cmd/gotochanger-tcmud) - empty/dash when
    // gotochanger-tcmud@<lib.name> isn't currently running.
    const report = (kernelModeDevices || {})[lib.name];
    const scsiPath = report ? report.changer_stable || report.changer || "-" : "-";
    tr.innerHTML = `<td><span style="display:inline-block;width:0.7em;height:0.7em;border-radius:50%;background:${lib.color || "#4285F4"};margin-right:0.4em;"></span>${lib.name}</td><td>${(lib.drives || []).length}</td><td>${(lib.slots || []).length}</td><td>${mailboxCount}</td><td>${scsiPath}</td>`;
    const td = document.createElement("td");
    td.appendChild(mkBtn("Bareos Config", () => {
      showBareosConfig(lib, status.name, operationalMode, report);
    }));
    td.appendChild(mkBtn("Kernel Mode Setup", () => {
      showKernelModeSetup(lib, status.name);
    }));
    td.appendChild(mkBtn("Edit", async () => {
      const picker = await logicalLibraryPickerData();
      // Exclude this library's own assignments from the ownership map -
      // its own elements must never render as "in use by" itself, and
      // must start out in the right-hand "This library" pane, not
      // disabled on the left.
      const owners = computeLogicalLibraryOwners(libs, lib.name);
      const kinds = logicalLibraryAssignmentKinds(picker, owners, logicalLibraryMembership(lib));
      const v = await openDialog("Edit logical library " + lib.name, [
        { name: "color", label: "Color", type: "color", value: lib.color || "#4285F4" },
        { name: "membership", type: "assignmentboard", rightLabel: lib.name, kinds },
      ]);
      if (!v) return;
      await api(`/api/v1/logical-libraries/${encodeURIComponent(lib.name)}`, {
        method: "PUT",
        body: JSON.stringify({ name: lib.name, color: v.color, ...v.membership }),
      });
      loadLogicalLibraries();
    }));
    td.appendChild(mkBtn("Delete", async () => {
      if (!confirm(`Delete logical library ${lib.name}?`)) return;
      await api(`/api/v1/logical-libraries/${encodeURIComponent(lib.name)}`, { method: "DELETE" });
      loadLogicalLibraries();
    }));
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
}

document.getElementById("newLogicalLibBtn").addEventListener("click", async () => {
  const picker = await logicalLibraryPickerData();
  const owners = computeLogicalLibraryOwners(picker.logicalLibraries);
  const kinds = logicalLibraryAssignmentKinds(picker, owners, { drives: [], magazines: [], mailboxes: [] });
  const v = await openDialog("New logical library", [
    { name: "name", label: "Name" },
    { name: "color", label: "Color", type: "color", value: nextLogicalLibraryColor(picker.logicalLibraries.length) },
    { name: "membership", type: "assignmentboard", rightLabel: "New library", kinds },
  ]);
  if (!v || !v.name) return;
  try {
    await api("/api/v1/logical-libraries", {
      method: "POST",
      body: JSON.stringify({ name: v.name, color: v.color, ...v.membership }),
    });
    loadLogicalLibraries();
  } catch (e) {
    alert(e.message);
  }
});

// ==================== Offsite vault (dashboard) ====================

function renderOffsite(vols) {
  const grid = document.getElementById("offsiteGrid");
  if (!grid) return;
  grid.innerHTML = "";
  const canOperate = state.role === "admin" || state.role === "operator";
  for (const v of vols || []) {
    const card = document.createElement("div");
    card.className = "card";
    card.innerHTML = `${cartridgeLabelHTML(v.barcode, v.tape_set, v.cleaning ? undefined : v.write_protected, canOperate)}<div>${fmtBytes(v.written_bytes)} / ${fmtBytes(v.capacity_bytes)}</div>`;
    wireWriteProtectSwitch(card, v.barcode, v.write_protected);
    const actions = document.createElement("div");
    actions.className = "actions";
    actions.appendChild(mkBtn("Recall", async () => {
      const opts = emptySlotOptions();
      if (!opts.length) { alert("No empty storage slots to recall into."); return; }
      const sel = await openDialog(`Recall ${v.barcode} to`, [{ name: "dest", label: "Destination", type: "select", options: opts }]);
      if (!sel) return;
      const [toKind, toAddr] = sel.dest.split(":");
      await api("/api/v1/offsite/recall", { method: "POST", body: JSON.stringify({ barcode: v.barcode, to_kind: toKind, to_address: Number(toAddr) }) });
      refresh();
    }));
    card.appendChild(actions);
    grid.appendChild(card);
  }
  setPanelSummary("offsiteSummary", `tapes: ${(vols || []).length}`);
}

document.getElementById("offsiteSendBtn")?.addEventListener("click", async () => {
  const slots = (state.status && state.status.slots) || [];
  const opts = slots.filter((s) => s.volume).sort((a, b) => a.address - b.address).map((s) => ({ value: String(s.address), label: `Slot ${s.label || s.address} (${s.volume.barcode})` }));
  if (!opts.length) { alert("No volumes in storage slots to send offsite."); return; }
  const v = await openDialog("Send to offsite vault", [{ name: "slot", label: "Slot", type: "select", options: opts }]);
  if (!v) return;
  try {
    await api("/api/v1/offsite/send", { method: "POST", body: JSON.stringify({ from_kind: "slot", from_address: Number(v.slot) }) });
    refresh();
  } catch (e) {
    alert(e.message);
  }
});

function loadAdminExtra(section) {
  if (section === "drive-types") loadDriveTypes();
  else if (section === "tape-types") loadTapeTypes();
  else if (section === "tape-sets") loadTapeSets();
  else if (section === "drives") loadDrives();
  else if (section === "magazines") loadMagazines();
  else if (section === "mailboxes") loadMailboxes();
  else if (section === "logical-libraries") loadLogicalLibraries();
}

boot();
