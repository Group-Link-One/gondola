// Deploy API Frontend
// Simple vanilla JS SPA

const API_BASE = '/api/v1'; // API version prefix

// State
let state = {
    token: localStorage.getItem('deploy_token'),
    user: null,
    repos: [],
    buildsByApp: {},
    buildPageSize: parseInt(localStorage.getItem('builds_page_size') || '5'),
    apps: [],
    deployments: [],
    currentDeployments: {}, // keyed by app_name -> env -> deployment info (Feature 1)
    environments: {},       // env config from backend (Feature 4)
    systemInfo: null,       // system info from /system/info
    adminConfig: null,      // full config from /admin/config
    webhooks: [],           // webhook deliveries from /admin/webhooks
    webhookRetrying: new Set(), // webhook IDs currently being retried
    discovered: { apps: [], skipped: [], lastScannedAt: null }, // ArgoCD auto-discovery
    discoverScanning: false, // true while POST /admin/repos/discover is in flight
    discoverError: null,     // last discovery error (string) or null
    llmTestRunning: false,   // true while POST /admin/llm/test is in flight
    llmTestResult: null,     // last LLM test result { provider, model, prompt, response, tokens, elapsed_ms } or null
    llmTestError: null,      // last LLM test error (string) or null
    showSkipped: false,      // whether the skipped section is expanded
    experimentBuildsByApp: {}, // Experiment (non-main branch) builds
    currentTab: 'dashboard',
    loading: false,
    error: null,
    expandedCommits: new Set(), // Track which build commits are expanded
    expandedDeploymentCommits: new Set(), // Track which deployment commits are expanded
    pollingBuilds: new Set(), // Builds being polled for analysis status
    pagination: {
        deployments: { cursor: '', limit: 5, hasMore: true },
        activity: { cursor: '', limit: 50, hasMore: true }
    },
    activity: [],                 // unified activity feed items
    activityLive: null,           // last live snapshot { running_tasks, git_ops, ... }
    activityLiveLoading: false,   // true while GET /activity/live is in flight
    activityLiveError: null,      // last live-snapshot error (string) or null
    showDeletedRepos: false,  // Show deleted repos in the list
    dashFilter: '',           // Dashboard: app-name/repo text filter
    dashMode: 'all',          // Dashboard: active stat filter (see DASH_MODES)
    dashInfos: [],            // Dashboard: per-app rollup, rebuilt each full render
    expandedApps: new Set(),  // Dashboard: app rows expanded into build detail
    showOlderBuilds: {},      // app_name -> bool: show builds older than the staging-deployed one
    deployStatus: {},         // `${buildId}:${env}` -> { phase: 'pending'|'reset'|'error', message }
    resetPolls: {},           // `${buildId}:${env}` -> true while an env reset is being polled
    showPasswordModal: false,
    passwordModalError: null,
    showDeletedApps: false,   // Dashboard: include soft-deleted apps in /apps
    appGroups: [],            // Admin: app groups from /auth/groups
    editingUserId: null,      // Admin: user row expanded into the scope editor
    editingGroupId: null,     // Admin: group row expanded into the pattern editor
    creatingGroup: false,     // Admin: new-group form visible
    memories: [],             // Memory: current memory rows from GET /memory
    memoryRepo: null,         // Memory: selected repo id
    memoryDraft: '',          // Memory: textarea content (not re-rendered while typing)
    memorySaved: '',          // Memory: last persisted content, for dirty detection
    memoryVersions: [],       // Memory: history of the selected repo
    memoryLoading: false,     // Memory: true while loading a repo's memory
    memorySaving: false,      // Memory: true while a save/rollback is in flight
    memoryError: null,        // Memory: last error (string) or null
    memorySummary: '',        // Memory: the "what changed" note, kept so a re-render cannot eat it
    memoryOpenVersion: null,  // Memory: version number expanded in the history list
    memorySuggestions: [],    // Memory: pending LLM-proposed updates
    memoryPendingByRepo: {},  // Memory: repo id -> pending suggestion count
    memoryEditingSuggestion: null, // Memory: suggestion id open in the edit box
    memoryBusySuggestion: null     // Memory: suggestion id with a request in flight
};

// RBAC helpers
function userCanWrite() { return state.user?.role !== 'viewer'; }
function userCanDeployTo(env) {
    if (!state.user) return false;
    if (state.user.role === 'admin') return true;
    if (state.user.role === 'viewer') return false;
    // deployer: check allowed_environments
    const allowed = state.user.allowed_environments;
    if (!allowed || allowed.length === 0) return true;
    return allowed.includes(env);
}
function userIsAdmin() { return state.user?.role === 'admin'; }

// App-name scope: group patterns plus the user's own allow list, minus the
// deny list. Mirrors domain.AppScope in Go — the server is still the authority,
// this only decides whether to draw the button.
function userAppScope(user) {
    const u = user || state.user;
    if (!u || u.role === 'admin') return { allow: [], deny: [] };
    const allow = [];
    for (const g of (u.groups || [])) allow.push(...(g.patterns || []));
    allow.push(...(u.allowed_apps || []));
    return { allow, deny: [...(u.denied_apps || [])] };
}

// Patterns match the whole app name, same anchoring as the Go side.
function appPatternMatches(appName, pattern) {
    try {
        return new RegExp(`^(?:${pattern})$`).test(appName);
    } catch (e) {
        return false;
    }
}

function scopePermits(scope, appName) {
    if (scope.deny.some(p => appPatternMatches(appName, p))) return false;
    if (scope.allow.length === 0) return true;
    return scope.allow.some(p => appPatternMatches(appName, p));
}

function userCanDeployApp(appName, user) {
    const u = user || state.user;
    if (!u) return false;
    if (u.role === 'admin') return true;
    if (u.role === 'viewer') return false;
    return scopePermits(userAppScope(u), appName);
}

// Inputs renderApp() refocuses after a re-render. Their value is rebuilt from
// `state`, so only the caret needs restoring.
const RESTORABLE_INPUTS = ['dash-filter', 'mem-content', 'mem-summary'];

// Polling interval (5 seconds)
const POLLING_INTERVAL = 5000;
let pollingTimer = null;

// In-flight load-more requests (prevents duplicate clicks)
const loadMoreInFlight = new Set();

// Per-app build bucket helpers
function ensureAppBucket(appName) {
    if (!state.buildsByApp[appName]) {
        state.buildsByApp[appName] = {
            items: [],
            itemsById: {},
            cursor: '',
            hasMore: true,
            loading: false,
            initialized: false
        };
    }
    return state.buildsByApp[appName];
}

function setAppBuilds(appName, builds, nextCursor) {
    const bucket = ensureAppBucket(appName);
    bucket.items = builds;
    bucket.itemsById = {};
    for (const b of builds) bucket.itemsById[b.id] = b;
    bucket.cursor = nextCursor || '';
    bucket.hasMore = !!nextCursor;
    bucket.initialized = true;
}

function appendAppBuilds(appName, builds, nextCursor) {
    const bucket = ensureAppBucket(appName);
    const existingIds = bucket.itemsById;
    for (const b of builds) {
        if (!existingIds[b.id]) {
            bucket.items.push(b);
        }
        existingIds[b.id] = b;
    }
    bucket.cursor = nextCursor || '';
    bucket.hasMore = !!nextCursor;
}

function updateBuildInPlace(build) {
    for (const appName of Object.keys(state.buildsByApp)) {
        const bucket = state.buildsByApp[appName];
        if (bucket.itemsById[build.id]) {
            bucket.itemsById[build.id] = build;
            const idx = bucket.items.findIndex(b => b.id === build.id);
            if (idx !== -1) bucket.items[idx] = build;
            return;
        }
    }
}

function getAllBuilds() {
    const all = [];
    for (const bucket of Object.values(state.buildsByApp)) {
        all.push(...bucket.items);
    }
    return all;
}

// DOM Elements
const app = document.getElementById('app');

// Initialize
function init() {
    if (state.token) {
        state.loading = true;
        render();
        fetchUser()
            .then(() => loadDashboardData())
            .then(() => { state.loading = false; render(); })
            .catch(() => { state.loading = false; render(); });
    } else {
        render();
    }
}

// API Helpers
async function api(path, options = {}) {
    const url = `${API_BASE}${path}`;
    const headers = {
        'Content-Type': 'application/json',
        ...options.headers
    };

    if (state.token) {
        headers['Authorization'] = `Bearer ${state.token}`;
    }

    const response = await fetch(url, {
        ...options,
        headers
    });

    if (response.status === 401) {
        logout();
        throw new Error('Session expired');
    }

    const data = await response.json().catch(() => null);

    if (!response.ok) {
        throw new Error(data?.error || `HTTP ${response.status}`);
    }

    return data;
}

// Auth
async function login(username, password) {
    const data = await api('/auth/login', {
        method: 'POST',
        body: JSON.stringify({ username, password })
    });

    state.token = data.token;
    state.user = data.user;
    localStorage.setItem('deploy_token', state.token);

    return data;
}

async function fetchUser() {
    try {
        const user = await api('/auth/me');
        state.user = user;
    } catch (err) {
        logout();
        throw err;
    }
}

function logout() {
    state.token = null;
    state.user = null;
    localStorage.removeItem('deploy_token');
    stopPolling();
    render();
}

// Data fetching
async function fetchRepos() {
    const url = state.showDeletedRepos ? '/repos?include_deleted=true' : '/repos';
    const repos = await api(url);
    state.repos = repos.data || [];
}

// Get configured environments from backend, with sensible fallback
function getConfiguredEnvironments() {
    const keys = Object.keys(state.environments || {});
    return keys.length > 0 ? keys : ['staging', 'prod'];
}

// Get allowed environments for a build (using its source_repo)
function getBuildEnvironments(build) {
    const allEnvs = getConfiguredEnvironments();
    let envs = allEnvs;

    if (build && build.source_repo) {
        // Find the repo config
        const repo = state.repos.find(r => r.id === build.source_repo);
        if (repo) {
            // If environments is specified, use those; otherwise allow all
            if (repo.environments && repo.environments.length > 0) {
                envs = repo.environments;
            }
        }
    }

    // Experiment gate: experiment builds can only land in envs whose target
    // argocd repo has allow_experiments=true. Mirrors the backend gate in
    // PerformDeployment so the deploy button doesn't appear for envs that
    // would 400 anyway.
    if (build && isExperimentBuild(build)) {
        envs = envs.filter(env => {
            const argoRepo = state.repos.find(r => r.repo_type === 'argocd' && r.environment === env);
            return argoRepo && argoRepo.allow_experiments === true;
        });
    }

    return envs;
}

// Experiment build helpers
function getRepoMainBranch(repoId) {
    const repo = state.repos.find(r => r.id === repoId);
    return repo?.branch || 'main';
}

function getAppRepoId(appName) {
    return state.apps.find(a => a.name === appName)?.repo_id;
}

function isExperimentBuild(build) {
    if (!build?.source_repo || !build?.branch) return false;
    return build.branch !== getRepoMainBranch(build.source_repo);
}

async function fetchBuilds(appName, loadMore = false) {
    const limit = state.buildPageSize;
    const bucket = ensureAppBucket(appName);
    const cursor = loadMore ? bucket.cursor : '';

    // Find the main branch for this app's repo to filter out experiment builds
    const appObj = state.apps.find(a => a.name === appName);
    const mainBranch = appObj?.repo_id ? getRepoMainBranch(appObj.repo_id) : '';

    let url = `/builds?limit=${limit}`;
    if (cursor) url += `&cursor=${encodeURIComponent(cursor)}`;
    if (appName) url += `&app=${encodeURIComponent(appName)}`;
    if (mainBranch) url += `&branch=${encodeURIComponent(mainBranch)}`;

    const data = await api(url);
    const newBuilds = data.data || [];

    if (loadMore) {
        appendAppBuilds(appName, newBuilds, data.next_cursor);
    } else {
        setAppBuilds(appName, newBuilds, data.next_cursor);
    }

    checkAndStartPolling();
}

// Fetch the latest builds for every app in a single request (replaces the
// old one-request-per-app fan-out). mode: 'main' fills state.buildsByApp with
// main-branch builds; 'experiment' fills state.experimentBuildsByApp with
// non-main-branch builds. Load-more still pages per app via fetchBuilds /
// fetchExperimentBuilds using the server-issued next_cursor.
async function fetchBuildsBatch(mode = 'main') {
    const limit = state.buildPageSize;
    const appNames = state.apps.map(a => a.name);
    if (appNames.length === 0) return;

    const url = `/builds/batch?per_app=${limit}&mode=${mode}`
        + `&apps=${encodeURIComponent(appNames.join(','))}`;
    const data = await api(url);
    const byApp = data.data || {};

    for (const app of state.apps) {
        const entry = byApp[app.name] || { builds: [], next_cursor: '' };
        const builds = entry.builds || [];
        if (mode === 'experiment') {
            const bucket = ensureExperimentBucket(app.name);
            bucket.items = builds;
            bucket.itemsById = {};
            for (const b of builds) bucket.itemsById[b.id] = b;
            bucket.cursor = entry.next_cursor || '';
            bucket.hasMore = !!entry.next_cursor;
            bucket.initialized = true;
        } else {
            setAppBuilds(app.name, builds, entry.next_cursor);
        }
    }

    if (mode !== 'experiment') checkAndStartPolling();
}

async function fetchBuildById(buildId) {
    const data = await api(`/builds/${buildId}`);
    return data.data;
}

// Experiment build bucket helpers
function ensureExperimentBucket(appName) {
    if (!state.experimentBuildsByApp[appName]) {
        state.experimentBuildsByApp[appName] = {
            items: [],
            itemsById: {},
            cursor: '',
            hasMore: true,
            loading: false,
            initialized: false
        };
    }
    return state.experimentBuildsByApp[appName];
}

async function fetchExperimentBuilds(appName, loadMore = false) {
    const limit = state.buildPageSize;
    const bucket = ensureExperimentBucket(appName);
    const cursor = loadMore ? bucket.cursor : '';

    // Find main branch for this app's repo to exclude it
    const app = state.apps.find(a => a.name === appName);
    const repoId = app?.repo_id;
    const mainBranch = getRepoMainBranch(repoId);

    let url = `/builds?limit=${limit}&branch_exclude=${encodeURIComponent(mainBranch)}`;
    if (cursor) url += `&cursor=${encodeURIComponent(cursor)}`;
    if (appName) url += `&app=${encodeURIComponent(appName)}`;

    const data = await api(url);
    const newBuilds = data.data || [];

    if (loadMore) {
        const existingIds = bucket.itemsById;
        for (const b of newBuilds) {
            if (!existingIds[b.id]) {
                bucket.items.push(b);
            }
            existingIds[b.id] = b;
        }
    } else {
        bucket.items = newBuilds;
        bucket.itemsById = {};
        for (const b of newBuilds) bucket.itemsById[b.id] = b;
    }
    bucket.cursor = data.next_cursor || '';
    bucket.hasMore = !!data.next_cursor;
    bucket.initialized = true;
}

async function loadMoreExperimentBuilds(appName) {
    if (loadMoreInFlight.has('exp-' + appName)) return;
    loadMoreInFlight.add('exp-' + appName);

    const bucket = ensureExperimentBucket(appName);
    bucket.loading = true;
    renderApp();

    try {
        await fetchExperimentBuilds(appName, true);
    } catch (err) {
        state.error = err.message;
    } finally {
        bucket.loading = false;
        loadMoreInFlight.delete('exp-' + appName);
        renderApp();
    }
}

async function loadMoreBuilds(appName) {
    if (loadMoreInFlight.has(appName)) return;
    loadMoreInFlight.add(appName);

    const bucket = ensureAppBucket(appName);
    bucket.loading = true;
    renderApp();

    try {
        await fetchBuilds(appName, true);
    } catch (err) {
        state.error = err.message;
    } finally {
        bucket.loading = false;
        loadMoreInFlight.delete(appName);
        renderApp();
    }
}

async function fetchApps() {
    const data = await api(state.showDeletedApps ? '/apps?include_deleted=true' : '/apps');
    state.apps = data.data || [];
}

async function fetchDeployments(appName = '', loadMore = false) {
    const limit = state.pagination.deployments.limit;
    const cursor = loadMore ? state.pagination.deployments.cursor : '';

    let url = `/deployments?limit=${limit}`;
    if (cursor) url += `&cursor=${encodeURIComponent(cursor)}`;
    if (appName) url += `&app=${encodeURIComponent(appName)}`;

    const data = await api(url);
    const newDeployments = data.data || [];

    if (loadMore) {
        state.deployments = [...state.deployments, ...newDeployments];
    } else {
        state.deployments = newDeployments;
    }

    state.pagination.deployments.cursor = data.next_cursor || '';
    state.pagination.deployments.hasMore = !!data.next_cursor;
}

async function loadMoreDeployments() {
    state.loading = true;
    renderApp();

    try {
        await fetchDeployments('', true);
    } catch (err) {
        state.error = err.message;
    } finally {
        state.loading = false;
        renderApp();
    }
}

// Activity feed: unified DB-backed history (builds, deployments, memory, webhooks)
async function fetchActivity(loadMore = false) {
    const limit = state.pagination.activity.limit;
    const cursor = loadMore ? state.pagination.activity.cursor : '';

    let url = `/activity?limit=${limit}`;
    if (cursor) url += `&cursor=${encodeURIComponent(cursor)}`;

    const data = await api(url);
    const items = data.data || [];

    if (loadMore) {
        state.activity = [...state.activity, ...items];
    } else {
        state.activity = items;
    }

    state.pagination.activity.cursor = data.next_cursor || '';
    state.pagination.activity.hasMore = !!data.next_cursor;
}

async function loadMoreActivity() {
    if (loadMoreInFlight.has('activity')) return;
    loadMoreInFlight.add('activity');
    state.loading = true;
    renderApp();
    try {
        await fetchActivity(true);
    } catch (err) {
        state.error = err.message;
    } finally {
        loadMoreInFlight.delete('activity');
        state.loading = false;
        renderApp();
    }
}

// Activity live: point-in-time snapshot of in-flight background work.
// No auto-polling — refreshed only on tab open and via the Refresh button.
async function fetchActivityLive() {
    state.activityLiveError = null;
    try {
        const data = await api('/activity/live');
        state.activityLive = data.data || null;
    } catch (err) {
        state.activityLiveError = err.message;
    }
}

async function refreshActivityLive() {
    state.activityLiveLoading = true;
    renderApp();
    try {
        await fetchActivityLive();
    } finally {
        state.activityLiveLoading = false;
        renderApp();
    }
}

// Feature 1: Fetch current deployments per app/env
async function fetchCurrentDeployments() {
    try {
        const data = await api('/deployments/current');
        const items = data.data || [];
        const map = {};
        for (const d of items) {
            if (!map[d.app_name]) map[d.app_name] = {};
            map[d.app_name][d.environment] = d;
        }
        state.currentDeployments = map;
    } catch (err) {
        console.error('Failed to fetch current deployments:', err);
    }
}

// Feature 4: Fetch environments config
async function fetchEnvironments() {
    try {
        const data = await api('/environments');
        state.environments = data.data || {};
    } catch (err) {
        console.error('Failed to fetch environments:', err);
    }
}

// Actions
async function promoteBuild(buildId, environment) {
    return api('/deployments', {
        method: 'POST',
        body: JSON.stringify({
            build_id: buildId,
            environment: environment
        })
    });
}

// Environment reset gate
async function fetchEnvReset(resetId) {
    const data = await api(`/env-resets/${resetId}`);
    return data.data;
}

async function fetchActiveEnvResets() {
    const data = await api('/env-resets?active=true');
    return data.data || [];
}

// Feature 3: Pre-flight deployment check
async function checkDeployment(buildId, environment) {
    const data = await api('/deployments/check', {
        method: 'POST',
        body: JSON.stringify({
            build_id: buildId,
            environment: environment
        })
    });
    return data.data;
}

async function regenerateAnalysis(build) {
    // Queue analysis task via API
    await api(`/builds/${build.id}/analyze`, { method: 'POST' });
}

async function fetchChangelog(build) {
    // Get changelog using source_repo
    if (!build.source_repo) {
        throw new Error('Build has no source repo');
    }
    const changelog = await api(`/changelog?repo=${encodeURIComponent(build.source_repo)}&from=${encodeURIComponent(build.commit_sha)}~10&to=${encodeURIComponent(build.commit_sha)}`);
    return changelog;
}

// Sync a repository
async function syncRepo(repoId) {
    state.loading = true;
    renderApp();

    try {
        await api(`/repos/${encodeURIComponent(repoId)}/sync`, { method: 'POST' });
        await fetchRepos();
    } catch (err) {
        state.error = err.message;
    } finally {
        state.loading = false;
        renderApp();
    }
}

// Polling for analysis status
function checkAndStartPolling() {
    const allBuilds = getAllBuilds();
    const needsPolling = allBuilds.some(b =>
        b.analysis_status === 'pending' || b.analysis_status === 'in_progress'
    );

    if (needsPolling && !pollingTimer) {
        startPolling();
    } else if (!needsPolling && pollingTimer) {
        stopPolling();
    }
}

function startPolling() {
    if (pollingTimer) return;

    pollingTimer = setInterval(async () => {
        // Collect build IDs that need polling from all buckets
        const idsToPoll = [];
        for (const bucket of Object.values(state.buildsByApp)) {
            for (const b of bucket.items) {
                if (b.analysis_status === 'pending' || b.analysis_status === 'in_progress') {
                    idsToPoll.push(b.id);
                }
            }
        }

        if (idsToPoll.length === 0) {
            stopPolling();
            return;
        }

        // Poll each build individually by ID — preserves load-more progress
        try {
            const results = await Promise.all(idsToPoll.map(id => fetchBuildById(id).catch(() => null)));
            let changed = false;
            for (const build of results) {
                if (build) {
                    updateBuildInPlace(build);
                    changed = true;
                }
            }
            if (changed) renderApp();
        } catch (err) {
            console.error('Polling error:', err);
        }
    }, POLLING_INTERVAL);
}

function stopPolling() {
    if (pollingTimer) {
        clearInterval(pollingTimer);
        pollingTimer = null;
    }
}

// Toggle commit expansion
function toggleCommits(buildId) {
    if (state.expandedCommits.has(buildId)) {
        state.expandedCommits.delete(buildId);
    } else {
        state.expandedCommits.add(buildId);
    }
    renderApp();
}

function toggleDeploymentCommits(deploymentId) {
    if (state.expandedDeploymentCommits.has(deploymentId)) {
        state.expandedDeploymentCommits.delete(deploymentId);
    } else {
        state.expandedDeploymentCommits.add(deploymentId);
    }
    renderApp();
}

// Rendering
function render() {
    if (!state.token) {
        renderLogin();
        return;
    }

    renderApp();
}

function renderLogin() {
    app.innerHTML = `
        <div class="login-box">
            <h2>Deploy API</h2>
            ${state.error ? `<div class="error">${escapeHtml(state.error)}</div>` : ''}
            <form id="login-form">
                <div class="form-group">
                    <label>Username</label>
                    <input type="text" id="username" required autocomplete="username">
                </div>
                <div class="form-group">
                    <label>Password</label>
                    <input type="password" id="password" required autocomplete="current-password">
                </div>
                <button type="submit" style="width: 100%">Sign In</button>
            </form>
        </div>
    `;

    document.getElementById('login-form').onsubmit = async (e) => {
        e.preventDefault();

        // Get values BEFORE any re-rendering
        const username = document.getElementById('username').value;
        const password = document.getElementById('password').value;

        state.error = null;
        state.loading = true;
        renderLogin();

        try {
            await login(username, password);
            await loadDashboardData();
            state.loading = false;
            render();
        } catch (err) {
            state.error = err.message;
            state.loading = false;
            renderLogin();
        }
    };
}

function renderApp() {
    // Preserve scroll position during re-render
    const scrollPos = window.scrollY;
    // Polling re-renders the page every 5s; don't yank the caret out of a field
    // someone is typing in. Only inputs whose value lives in `state` can be
    // restored this way, since innerHTML rebuilds them from it.
    const active = document.activeElement;
    const restoreFocus = active && RESTORABLE_INPUTS.includes(active.id)
        ? { id: active.id, start: active.selectionStart, end: active.selectionEnd }
        : null;
    
    app.innerHTML = `
        <header>
            <div class="container">
                <div class="logo"><img src="/static/logo.svg" alt="Deploy API"></div>
                <div class="user-info">
                    <span>${escapeHtml(state.user?.username || '')}</span>
                    ${state.user?.role ? `<span class="role-badge role-${escapeHtml(state.user.role)}">${escapeHtml(state.user.role)}</span>` : ''}
                    <button class="secondary" onclick="openPasswordModal()">Change password</button>
                    <button class="secondary" onclick="logout()">Logout</button>
                </div>
            </div>
        </header>
        <div class="container">
            ${state.error ? `
                <div class="error-banner">
                    ${escapeHtml(state.error)}
                    <span class="error-banner-close" onclick="state.error=null; renderApp()">x</span>
                </div>
            ` : ''}
            <div class="tabs">
                <button class="tab ${state.currentTab === 'dashboard' ? 'active' : ''}" onclick="switchTab('dashboard')">Dashboard</button>
                <button class="tab ${state.currentTab === 'builds' ? 'active' : ''}" onclick="switchTab('builds')">Builds</button>
                <button class="tab ${state.currentTab === 'deployments' ? 'active' : ''}" onclick="switchTab('deployments')">Deployments</button>
                <button class="tab ${state.currentTab === 'repos' ? 'active' : ''}" onclick="switchTab('repos')">Repositories</button>
                <button class="tab ${state.currentTab === 'memory' ? 'active' : ''}" onclick="switchTab('memory')">Memory${memoryPendingTotal() > 0 ? `<span class="tab-badge">${memoryPendingTotal()}</span>` : ''}</button>
                <button class="tab ${state.currentTab === 'experiments' ? 'active' : ''}" onclick="switchTab('experiments')">Experiments</button>
                <button class="tab ${state.currentTab === 'activity' ? 'active' : ''}" onclick="switchTab('activity')">Activity</button>
                ${userIsAdmin() ? `<button class="tab ${state.currentTab === 'admin' ? 'active' : ''}" onclick="switchTab('admin')">Admin</button>` : ''}
            </div>
            <div id="content">
                ${state.loading ? '<div class="loading"><span class="spinner"></span>Loading...</div>' : renderContent()}
            </div>
        </div>
        ${state.showPasswordModal ? renderPasswordModal() : ''}
    `;

    // Restore scroll position after render
    window.scrollTo(0, scrollPos);

    if (restoreFocus) {
        const el = document.getElementById(restoreFocus.id);
        if (el) {
            el.focus();
            el.setSelectionRange(restoreFocus.start, restoreFocus.end);
        }
    }

    if (state.showPasswordModal) {
        const form = document.getElementById('change-password-form');
        if (form) form.addEventListener('submit', submitChangePassword);
        const cur = document.getElementById('current-password');
        if (cur) cur.focus();
    }
}

function renderPasswordModal() {
    return `
        <div class="modal-overlay" onclick="if (event.target === this) closePasswordModal()">
            <div class="modal">
                <h3>Change password</h3>
                ${state.passwordModalError ? `<div class="error-banner" style="margin-bottom: 12px;">${escapeHtml(state.passwordModalError)}</div>` : ''}
                <form id="change-password-form">
                    <div class="form-group">
                        <label>Current password</label>
                        <input type="password" id="current-password" required autocomplete="current-password">
                    </div>
                    <div class="form-group">
                        <label>New password (min 8 chars)</label>
                        <input type="password" id="new-password-self" required minlength="8" autocomplete="new-password">
                    </div>
                    <div class="form-group">
                        <label>Confirm new password</label>
                        <input type="password" id="new-password-confirm" required minlength="8" autocomplete="new-password">
                    </div>
                    <div class="modal-actions">
                        <button type="button" class="secondary" onclick="closePasswordModal()">Cancel</button>
                        <button type="submit">Update password</button>
                    </div>
                </form>
            </div>
        </div>
    `;
}

function openPasswordModal() {
    state.showPasswordModal = true;
    state.passwordModalError = null;
    renderApp();
}

function closePasswordModal() {
    state.showPasswordModal = false;
    state.passwordModalError = null;
    renderApp();
}

async function submitChangePassword(e) {
    e.preventDefault();
    const current = document.getElementById('current-password').value;
    const next = document.getElementById('new-password-self').value;
    const confirm = document.getElementById('new-password-confirm').value;

    if (next !== confirm) {
        state.passwordModalError = 'New passwords do not match';
        renderApp();
        return;
    }
    if (current === next) {
        state.passwordModalError = 'New password must differ from current password';
        renderApp();
        return;
    }

    try {
        await api('/auth/me/password', {
            method: 'PUT',
            body: JSON.stringify({ current_password: current, new_password: next })
        });
        closePasswordModal();
        alert('Password updated.');
    } catch (err) {
        state.passwordModalError = err.message || 'Failed to update password';
        renderApp();
    }
}

// Map an activity source to an icon + label + accent class.
const ACTIVITY_SOURCE_META = {
    build:      { icon: '\u{1F528}', label: 'Build' },       // hammer
    deployment: { icon: '\u{1F680}', label: 'Deploy' },      // rocket
    memory:     { icon: '\u{1F4DD}', label: 'Memory' },      // memo
    webhook:    { icon: '\u{1F514}', label: 'Webhook' }      // bell
};

// Status/risk -> badge class, reusing the existing risk-* palette.
function activityBadgeClass(item) {
    const s = (item.status || '').toLowerCase();
    if (s === 'failed') return 'risk-critical';
    if (s === 'pushed' || s === 'succeeded' || s === 'completed') return 'risk-low';
    if (s === 'in_progress' || s === 'delivering' || s === 'pending') return 'risk-medium';
    return 'risk-unknown';
}

// Human-readable primary line for one activity item.
function activityHeadline(item) {
    const app = item.app ? escapeHtml(item.app) : '';
    const title = item.title ? escapeHtml(item.title) : '';
    switch (item.source) {
        case 'build':
            return `${app}${title ? ` <span class="build-tag">${title}</span>` : ''}`;
        case 'deployment':
            return `${app}${item.environment ? ` &rarr; <strong>${escapeHtml(item.environment)}</strong>` : ''}${title ? ` <span class="build-tag">${title}</span>` : ''}`;
        case 'memory':
            return `${app}${title ? ` <span class="build-tag">${title}</span>` : ''}`;
        case 'webhook':
            return title || 'event';
        default:
            return app || title;
    }
}

function renderActivity() {
    return `
        <div class="activity-page">
            ${renderActivityLive()}
            ${renderActivityFeed()}
        </div>
    `;
}

function renderActivityLive() {
    const live = state.activityLive;
    const loading = state.activityLiveLoading;
    const err = state.activityLiveError;

    const refreshBtn = `
        <button class="secondary" onclick="refreshActivityLive()" ${loading ? 'disabled' : ''}>
            ${loading ? 'Refreshing…' : '↻ Refresh'}
        </button>`;

    let body;
    if (err) {
        body = `<div class="empty">Failed to load live status: ${escapeHtml(err)}</div>`;
    } else if (!live) {
        body = `<div class="empty">${loading ? 'Loading…' : 'No live data yet.'}</div>`;
    } else {
        const tasks = live.running_tasks || [];
        const ops = live.git_ops || [];

        const tasksHtml = tasks.length === 0
            ? `<div class="empty">No background tasks running</div>`
            : tasks.map(t => `
                <div class="build-item">
                    <div class="build-info">
                        <span class="risk-badge ${t.state === 'processing' ? 'risk-medium' : 'risk-unknown'}">${escapeHtml(t.state)}</span>
                        <strong>${escapeHtml(t.type)}</strong>
                        ${t.app ? ` &middot; ${escapeHtml(t.app)}` : ''}
                        ${t.build_id ? ` &middot; build #${t.build_id}` : ''}
                        ${t.environment ? ` &rarr; ${escapeHtml(t.environment)}` : ''}
                    </div>
                    <div class="build-meta">${dateSpan(t.started_at || t.queued_at)}</div>
                </div>`).join('');

        const opsHtml = ops.length === 0
            ? `<div class="empty">No git operations in progress</div>`
            : ops.map(o => `
                <div class="build-item">
                    <div class="build-info">
                        <span class="risk-badge risk-medium">${escapeHtml(o.kind)}</span>
                        <strong>${escapeHtml(o.repo)}</strong>
                    </div>
                    <div class="build-meta">${dateSpan(o.started_at)}</div>
                </div>`).join('');

        body = `
            <div class="activity-live-vitals">
                <span>Goroutines: <strong>${live.goroutines ?? '?'}</strong></span>
                <span>Uptime: <strong>${formatDuration(live.uptime_seconds)}</strong></span>
                <span>Snapshot: ${dateSpan(live.server_time)}</span>
            </div>
            <h4 class="activity-subhead">Background tasks (${tasks.length})</h4>
            ${tasksHtml}
            <h4 class="activity-subhead">Git operations (${ops.length})</h4>
            ${opsHtml}
        `;
    }

    return `
        <div class="card activity-live">
            <div class="activity-live-header">
                <h3>Live &mdash; what's running now</h3>
                ${refreshBtn}
            </div>
            ${body}
        </div>
    `;
}

function renderActivityFeed() {
    const items = state.activity || [];
    const hasMore = state.pagination.activity.hasMore;
    const isLoading = loadMoreInFlight.has('activity');

    const listHtml = items.length === 0
        ? `<div class="empty">No activity recorded yet</div>`
        : items.map(item => {
            const meta = ACTIVITY_SOURCE_META[item.source] || { icon: '•', label: item.source };
            const badge = item.status
                ? `<span class="risk-badge ${activityBadgeClass(item)}">${escapeHtml(item.status)}</span>`
                : '';
            const actor = item.actor ? `<span class="activity-actor">${escapeHtml(item.actor)}</span>` : '';
            const detail = item.detail
                ? `<div class="activity-detail" title="${escapeHtml(item.detail)}">${escapeHtml(item.detail)}</div>`
                : '';
            return `
                <div class="build-item activity-row">
                    <div class="build-info">
                        <span class="activity-icon" title="${escapeHtml(meta.label)}">${meta.icon}</span>
                        <span class="activity-source">${escapeHtml(meta.label)}</span>
                        <span class="activity-headline">${activityHeadline(item)}</span>
                        ${badge}
                        ${actor}
                        ${detail}
                    </div>
                    <div class="build-meta">${dateSpan(item.timestamp)}</div>
                </div>`;
        }).join('');

    const loadMoreHtml = (items.length > 0 && hasMore) ? `
        <div class="load-more">
            <button class="secondary" onclick="loadMoreActivity()" ${isLoading ? 'disabled' : ''}>
                ${isLoading ? 'Loading…' : 'Load More'}
            </button>
        </div>` : '';

    return `
        <div class="card">
            <div class="activity-live-header">
                <h3>Recent activity</h3>
            </div>
            ${listHtml}
            ${loadMoreHtml}
        </div>
    `;
}

// formatDuration turns a seconds count into a compact "2h 3m" / "5m 10s" string.
function formatDuration(seconds) {
    if (seconds == null || isNaN(seconds)) return '?';
    seconds = Math.floor(seconds);
    const d = Math.floor(seconds / 86400);
    const h = Math.floor((seconds % 86400) / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    const s = seconds % 60;
    if (d > 0) return `${d}d ${h}h`;
    if (h > 0) return `${h}h ${m}m`;
    if (m > 0) return `${m}m ${s}s`;
    return `${s}s`;
}

function renderContent() {
    switch (state.currentTab) {
        case 'dashboard':
            return renderDashboard();
        case 'builds':
            return renderBuilds();
        case 'deployments':
            return renderDeployments();
        case 'repos':
            return renderRepos();
        case 'memory':
            return renderMemory();
        case 'experiments':
            return renderExperiments();
        case 'activity':
            return renderActivity();
        case 'admin':
            return userIsAdmin() ? renderAdmin() : renderDashboard();
        default:
            return renderDashboard();
    }
}

// Get environment badge HTML
function getEnvironmentBadge(deployment) {
    if (!deployment) return '';

    const colors = {
        'staging': '#3fb950',   // green
        'prod': '#58a6ff',      // blue
        'newprod': '#a371f7'    // purple
    };

    const color = colors[deployment.environment] || '#8b949e';
    const envName = deployment.environment.toUpperCase();

    return `<span class="env-badge" style="background: ${color}20; color: ${color}; border: 1px solid ${color}40;">${envName}</span>`;
}

// Feature 2: Get all environment badges for a build
function getAllDeploymentBadges(build) {
    const deps = build.all_deployments;
    if (!deps || deps.length === 0) {
        return '<span class="not-deployed-badge">Not Deployed</span>';
    }
    return deps.map(d => getEnvironmentBadge(d)).join('');
}

// Get analysis status badge HTML
function getAnalysisStatusBadge(build) {
    const status = build.analysis_status;

    switch (status) {
        case 'pending':
            return `<span class="status-badge pending" title="Analysis queued">Queued</span>`;
        case 'in_progress':
            return `<span class="status-badge in-progress" title="Analysis in progress">Analyzing...</span>`;
        case 'failed':
            return `<span class="status-badge failed" title="Analysis failed">Failed</span>`;
        case 'completed':
        default:
            return '';
    }
}

// Get risk level badge HTML
function riskClassFor(level) {
    switch (level) {
        case 'critical': return 'risk-critical';
        case 'high':     return 'risk-high';
        case 'medium':   return 'risk-medium';
        case 'unknown':  return 'risk-unknown';
        default:         return 'risk-low';
    }
}

function getRiskBadge(build) {
    if (!build.risk_level) return '';
    return `<span class="${riskClassFor(build.risk_level)}">Risk: ${escapeHtml(build.risk_level)}</span>`;
}

// Get deployment status label with icon
function getDeploymentStatusLabel(status) {
    switch (status) {
        case 'pushed':  return 'Pushed';
        case 'failed':  return 'Failed';
        case 'pending': return 'Pending';
        default:        return escapeHtml(status || 'unknown');
    }
}

// Get cumulative commits from baseline (prod) deployment to this build
function getCumulativeCommits(build) {
    const appName = build.app_name;
    const appDeploys = state.currentDeployments[appName] || {};

    // Find the baseline environment (same logic as backend analysis)
    const envCfg = state.environments || {};
    const baselineEnv = envCfg['staging']?.baseline || 'prod';
    const baselineDeploy = appDeploys[baselineEnv];
    const baselineBuildId = baselineDeploy ? baselineDeploy.build_id : null;

    const ownCommits = build.commits || [];
    const own = { commits: ownCommits, isCumulative: false, baselineEnv: null, shipped: false };

    // Once on the baseline, count what that deploy shipped, not what is still pending
    const shippedDeploy = (build.all_deployments || []).find(d => d.environment === baselineEnv && d.status === 'pushed')
        || (baselineDeploy && baselineDeploy.build_id === build.id ? baselineDeploy : null);
    const shipped = !!shippedDeploy;
    const stopBuildId = shipped ? shippedDeploy.previous_build_id : baselineBuildId;

    // A stop at or after this build means a rollback or a first deploy: nothing to accumulate
    if (!stopBuildId || stopBuildId >= build.id) {
        return own;
    }

    // Build a map of loaded builds by ID for this app
    const builds = state.buildsByApp[appName]?.items || [];
    const buildMap = {};
    for (const b of builds) {
        buildMap[b.id] = b;
    }

    // Walk backwards from this build via previous_build_id, collecting commits
    const allCommits = [];
    let current = build;
    const visited = new Set();

    while (current && !visited.has(current.id)) {
        visited.add(current.id);

        // Stop BEFORE the baseline-deployed build (its commits are already deployed)
        if (current.id === stopBuildId) break;

        if (current.commits && current.commits.length > 0) {
            allCommits.push(...current.commits);
        }

        if (current.previous_build_id && buildMap[current.previous_build_id]) {
            current = buildMap[current.previous_build_id];
        } else {
            break;
        }
    }

    if (allCommits.length > ownCommits.length) {
        return { commits: allCommits, isCumulative: true, baselineEnv, shipped };
    }
    return own;
}

// Render commit list for a build
function renderCommitList(build, isExpanded) {
    const { commits, isCumulative, baselineEnv, shipped } = getCumulativeCommits(build);
    if (!commits || commits.length === 0) {
        return '';
    }

    const commitCount = commits.length;
    const plural = `${commitCount} commit${commitCount !== 1 ? 's' : ''}`;
    const label = !isCumulative ? plural
        : shipped ? `${plural} shipped to ${baselineEnv}`
        : `${plural} since ${baselineEnv}`;

    if (!isExpanded) {
        return `
            <div class="commit-summary" onclick="toggleCommits(${build.id})">
                <span class="commit-count">${label}</span>
                <span class="expand-hint">Click to expand</span>
            </div>
        `;
    }

    return `
        <div class="commit-list-container">
            <div class="commit-summary" onclick="toggleCommits(${build.id})">
                <span class="commit-count">${label}</span>
                <span class="expand-hint">Click to collapse</span>
            </div>
            <div class="commit-list">
                ${commits.map(c => `
                    <div class="commit-item">
                        ${renderShaLink(c.sha, build.source_repo)}
                        <span class="commit-message" title="${escapeHtml(c.message || '')}">${escapeHtml((c.message || '').split('\n')[0].substring(0, 60))}</span>
                        <span class="commit-author">${escapeHtml(c.author || '')}</span>
                        ${dateSpan(c.timestamp, 'commit-date')}
                    </div>
                `).join('')}
            </div>
        </div>
    `;
}

// Get environments for an app based on its source repo config
function getAppEnvironments(appName) {
    const app = state.apps.find(a => a.name === appName);
    if (app && app.repo_id) {
        const repo = state.repos.find(r => r.id === app.repo_id);
        if (repo && repo.environments && repo.environments.length > 0) {
            return repo.environments;
        }
    }
    return getConfiguredEnvironments();
}

// Feature 1: Render "Currently Deployed" grid for an app
function renderDeployGrid(appName) {
    const envs = getAppEnvironments(appName);
    const appDeploys = state.currentDeployments[appName] || {};
    const repoId = getAppRepoId(appName);

    return `
        <div class="deploy-grid" data-deploy-grid="${escapeAttr(appName)}">
            ${envs.map(env => {
                const d = appDeploys[env];
                if (!d) {
                    return `
                        <div class="deploy-cell deploy-cell-empty">
                            <div class="deploy-cell-env">${escapeHtml(env.toUpperCase())}</div>
                            <div class="deploy-cell-tag">---</div>
                        </div>
                    `;
                }
                const isExp = d.branch && d.branch !== getRepoMainBranch(repoId);
                return `
                    <div class="deploy-cell${isExp ? ' deploy-cell-experiment' : ''}">
                        <div class="deploy-cell-env">${escapeHtml(env.toUpperCase())}</div>
                        <div class="deploy-cell-tag">${escapeHtml(d.image_tag || '---')}${isExp ? ` <span class="experiment-branch-badge">${escapeHtml(d.branch)}</span>` : ''}</div>
                        <div class="deploy-cell-meta">
                            ${renderShaLink(d.commit_sha, repoId)}
                            ${dateSpan(d.deployed_at)}
                        </div>
                        ${d.deployed_by ? `<div class="deploy-cell-meta">${escapeHtml(d.deployed_by)}</div>` : ''}
                    </div>
                `;
            }).join('')}
        </div>
    `;
}

// ---------------------------------------------------------------------------
// Dashboard — deployment matrix
// One row per app, one column per environment. Rows expand into the full
// build list (same renderer as the Builds tab, deploy buttons included).
// ---------------------------------------------------------------------------

// Promotion order first, anything unrecognised after it (stable, alphabetical).
const ENV_ORDER = ['staging', 'prod', 'newprod'];
function envRank(env) {
    const i = ENV_ORDER.indexOf(env);
    return i === -1 ? ENV_ORDER.length : i;
}

// Only envs that at least one app targets or has a deployment in. A column
// that would be n/a for every row is dead width.
function dashEnvColumns() {
    const cols = new Set();
    state.apps.forEach(a => getAppEnvironments(a.name).forEach(e => cols.add(e)));
    Object.values(state.currentDeployments || {}).forEach(m => Object.keys(m).forEach(e => cols.add(e)));
    return [...cols].sort((a, b) => envRank(a) - envRank(b) || a.localeCompare(b));
}

// Per-app rollup used by both the stat strip and the table rows.
function getAppDashInfo(app) {
    const bucket = state.buildsByApp[app.name];
    const builds = bucket?.items || [];
    const hasMore = bucket?.hasMore ?? false;
    const latest = builds[0] || null;
    const deploys = state.currentDeployments[app.name] || {};
    const envs = getAppEnvironments(app.name);
    const mainBranch = getRepoMainBranch(app.repo_id);

    let maxDeployedId = 0;
    for (const d of Object.values(deploys)) {
        if (d && d.build_id > maxDeployedId) maxDeployedId = d.build_id;
    }

    const undeployed = builds.filter(b => b.id > maxDeployedId).length;
    const envState = {};
    let behindEnvs = 0, expEnvs = 0, deployedEnvs = 0;
    // Once an app is live in prod, only prod drift counts: staging running ahead or behind is normal.
    const driftEnvs = deploys.prod ? new Set(['prod']) : null;

    // Union, not just the declared envs: a live deployment in an env the repo
    // config no longer lists must still show up rather than reading as n/a.
    for (const env of new Set([...envs, ...Object.keys(deploys)])) {
        const d = deploys[env];
        if (!d) { envState[env] = null; continue; }
        deployedEnvs++;
        const isExp = d.is_experiment || (d.branch && d.branch !== mainBranch);
        const behind = builds.filter(b => b.id > d.build_id).length;
        if (behind > 0 && (!driftEnvs || driftEnvs.has(env))) behindEnvs++;
        if (isExp) expEnvs++;
        envState[env] = { d, isExp, behind, behindCapped: behind > 0 && behind === builds.length && hasMore };
    }

    return {
        builds, hasMore, latest, envs, envState, undeployed,
        undeployedCapped: undeployed > 0 && undeployed === builds.length && hasMore,
        behindEnvs, expEnvs,
        allOnLatest: deployedEnvs > 0 && behindEnvs === 0,
        failedAnalyses: builds.filter(b => b.analysis_status === 'failed').length,
        changesTip: dashChangesTip(builds, deploys.prod, mainBranch)
    };
}

// Hover text for the app cell: the changelog of the newest main-branch build,
// which staging analysis diffs against prod, so it reads as "what prod lacks".
function dashChangesTip(builds, prodDeploy, mainBranch) {
    const b = builds.find(x => !mainBranch || x.branch === mainBranch) || builds[0];
    if (!b) return 'No builds yet';
    const inProd = prodDeploy && prodDeploy.build_id >= b.id;
    const head = prodDeploy
        ? (inProd ? `Live in prod (${b.image_tag})` : `Not in prod yet (${b.image_tag})`)
        : `Latest build (${b.image_tag})`;

    let body = (b.changelog_text || '').trim();
    if (!body) body = (b.llm_analysis && b.llm_analysis.summary) || '';
    if (!body && Array.isArray(b.commits)) {
        body = b.commits.map(c => `- ${(c.message || '').split('\n')[0]}`).join('\n');
    }
    body = body
        .replace(/^#+[ \t]*(.*?)[ \t]*#*$/gm, '$1:')
        .replace(/\*\*(.*?)\*\*/g, '$1')
        .replace(/`/g, '')
        .replace(/\n{3,}/g, '\n\n');
    if (body.length > 1500) body = body.slice(0, 1500) + '\n…';
    return body ? `${head}\n\n${body}` : `${head}\n\nNo changelog yet`;
}

const DASH_MODES = {
    all:        { label: 'apps',            match: () => true },
    drift:      { label: 'envs behind',     match: i => i.behindEnvs > 0,     count: i => i.behindEnvs,      tone: 'warn' },
    experiments:{ label: 'experiments live',match: i => i.expEnvs > 0,        count: i => i.expEnvs,         tone: 'warn' },
    undeployed: { label: 'builds waiting',  match: i => i.undeployed > 0,     count: i => i.undeployed },
    failed:     { label: 'analyses failed', match: i => i.failedAnalyses > 0, count: i => i.failedAnalyses,  tone: 'bad' }
};

function renderDashStats(infos) {
    const totals = { all: infos.length };
    for (const [mode, cfg] of Object.entries(DASH_MODES)) {
        if (mode === 'all') continue;
        totals[mode] = infos.reduce((sum, i) => sum + cfg.count(i), 0);
    }

    const chips = Object.entries(DASH_MODES).map(([mode, cfg]) => {
        const n = totals[mode];
        const active = state.dashMode === mode;
        const clickable = mode === 'all' || n > 0;
        const cls = ['dash-stat'];
        if (n > 0 && cfg.tone) cls.push(`dash-stat-${cfg.tone}`);
        if (n === 0) cls.push('dash-stat-zero');
        if (clickable) cls.push('dash-stat-clickable');
        if (active) cls.push('active');
        return `<div class="${cls.join(' ')}"${clickable ? ` data-dash-mode="${mode}"` : ''}>
            <strong>${n}</strong><span>${cfg.label}</span>
        </div>`;
    }).join('');

    return `<div class="dash-stats" id="dash-stats">${chips}</div>`;
}

function renderDashToolbar(shown, total) {
    const filtering = state.dashMode !== 'all' || state.dashFilter;
    return `
        <div class="dash-toolbar">
            <input type="text" id="dash-filter" placeholder="Filter apps..."
                   value="${escapeAttr(state.dashFilter)}" oninput="onDashFilter(this.value)">
            <span class="dash-count" id="dash-count">${shown} of ${total}</span>
            ${filtering ? `<button class="btn-small secondary" onclick="clearDashFilter()">Clear filter</button>` : ''}
            <span class="dash-spacer"></span>
            <span class="dash-legend"><span class="dash-behind">&#9650;N</span> newer builds exist</span>
            <span class="dash-legend"><span class="dash-exp-mark">&#9888;</span> experiment live</span>
            ${userIsAdmin() ? `
                <label class="dash-legend" style="cursor: pointer; white-space: nowrap;">
                    <input type="checkbox" ${state.showDeletedApps ? 'checked' : ''}
                           onchange="toggleShowDeletedApps(this.checked)"> show deleted
                </label>` : ''}
        </div>
    `;
}

async function toggleShowDeletedApps(checked) {
    state.showDeletedApps = checked;
    await fetchApps();
    renderApp();
}

function dashVisibleApps(infos) {
    const q = state.dashFilter.trim().toLowerCase();
    const matcher = DASH_MODES[state.dashMode]?.match || (() => true);
    let pool = infos.filter(({ info }) => matcher(info));
    if (!q) return pool;
    // Name matches win outright; repo matching is only a fallback so that
    // typing an app name never drags in every sibling app from its repo.
    const byName = pool.filter(({ app }) => app.name.toLowerCase().includes(q));
    if (byName.length > 0) return byName;
    return pool.filter(({ app }) => (app.repo_id || '').toLowerCase().includes(q));
}

function renderDashEnvCell(appName, env, info) {
    const st = info.envState[env];
    if (!st) {
        // "—" means targets this env but nothing deployed; "n/a" means out of scope.
        const marker = info.envs.includes(env)
            ? '<span class="dash-cell-none">&mdash;</span>'
            : '<span class="dash-cell-na">n/a</span>';
        return `<td class="dash-env-cell">${marker}</td>`;
    }
    const { d, isExp, behind, behindCapped } = st;
    const tip = [
        d.image_tag,
        d.commit_sha ? d.commit_sha.substring(0, 7) : '',
        d.branch ? `branch ${d.branch}` : '',
        d.deployed_by ? `by ${d.deployed_by}` : '',
        d.deployed_at
    ].filter(Boolean).join(' · ');

    return `
        <td class="dash-env-cell${isExp ? ' dash-exp' : ''}" title="${escapeAttr(tip)}">
            ${isExp ? '<span class="dash-exp-mark">&#9888;</span>' : ''}
            <span class="dash-cell-tag">${escapeHtml(isExp ? (d.branch || d.image_tag) : d.image_tag)}</span>
            <span class="dash-cell-age">${escapeHtml(formatAge(d.deployed_at))}</span>
            ${behind > 0 ? `<span class="dash-behind" title="${behind}${behindCapped ? '+' : ''} newer build(s) available">&#9650;${behind}${behindCapped ? '+' : ''}</span>` : ''}
        </td>
    `;
}

function renderDashLatestCell(info) {
    if (!info.latest) {
        return `<td class="dash-latest-cell"><span class="dash-cell-none">no builds</span></td>`;
    }
    const b = info.latest;
    const status = getAnalysisStatusBadge(b);
    // low/unknown is the common case and adds nothing but noise at this density
    const notable = ['medium', 'high', 'critical'].includes(b.risk_level);
    const risk = notable ? `<span class="dash-risk ${riskClassFor(b.risk_level)}">${escapeHtml(b.risk_level)}</span>` : '';
    return `
        <td class="dash-latest-cell" title="${escapeAttr(`${b.image_tag} · ${b.branch || ''} · ${b.built_by || ''} · ${b.built_at}`)}">
            <span class="dash-cell-tag">${escapeHtml(b.image_tag)}</span>
            <span class="dash-cell-age">${escapeHtml(formatAge(b.built_at))}</span>
            ${info.undeployed > 0
                ? `<span class="dash-behind" title="${info.undeployed}${info.undeployedCapped ? '+' : ''} build(s) not deployed anywhere">&#9650;${info.undeployed}${info.undeployedCapped ? '+' : ''}</span>`
                : (info.allOnLatest && info.expEnvs === 0 ? '<span class="dash-ok" title="Every environment is on the latest build">&#10003;</span>' : '')}
            ${risk}${status}
        </td>
    `;
}

const ICON_TRASH = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 6h18"/><path d="M8 6V4h8v2"/><path d="M6 6l1 14h10l1-14"/><path d="M10 11v5"/><path d="M14 11v5"/></svg>';
const ICON_RESTORE = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 12a9 9 0 1 0 3-6.7"/><path d="M3 4v5h5"/></svg>';

function renderDashRows() {
    const cols = dashEnvColumns();
    const visible = dashVisibleApps(state.dashInfos || []);

    if (visible.length === 0) {
        return `<tr><td class="dash-empty" colspan="${cols.length + 3}">No apps match the current filter.</td></tr>`;
    }

    return visible.map(({ app, info }) => {
        const expanded = state.expandedApps.has(app.name);
        const badges = (app.deleted_at ? '<span class="dash-badge dash-badge-deleted">deleted</span>' : '')
            + (isAppDiscovered(app.name) ? '<span class="dash-badge dash-badge-auto" title="Found in an ArgoCD Application manifest by auto-discovery">auto</span>' : '');

        const row = `
            <tr class="dash-row${expanded ? ' expanded' : ''}" data-app="${escapeAttr(app.name)}">
                <td class="dash-app-cell" title="${escapeAttr(info.changesTip)}">
                    <span class="dash-chevron">&#9654;</span>
                    <span class="dash-app-name">${escapeHtml(app.name)}</span>${badges}
                    ${userIsAdmin() ? (app.deleted_at
                        ? `<button class="dash-app-action" data-app-action="restore" data-app="${escapeAttr(app.name)}" title="Restore this app" aria-label="Restore ${escapeAttr(app.name)}">${ICON_RESTORE}</button>`
                        : `<button class="dash-app-action" data-app-action="delete" data-app="${escapeAttr(app.name)}" title="Hide this app (history is kept)" aria-label="Hide ${escapeAttr(app.name)}">${ICON_TRASH}</button>`) : ''}
                </td>
                ${cols.map(env => renderDashEnvCell(app.name, env, info)).join('')}
                ${renderDashLatestCell(info)}
                <td class="dash-fill-col"></td>
            </tr>
        `;

        if (!expanded) return row;

        const body = info.builds.length === 0
            ? '<div class="dash-detail-empty">No builds yet for this app.</div>'
            : info.builds.map(b => renderBuildItem(b)).join('');

        return row + `
            <tr class="dash-detail-row">
                <td class="dash-detail-cell" colspan="${cols.length + 3}">
                    <div class="dash-detail">
                        <div class="dash-detail-head">
                            Repo: ${escapeHtml(app.repo_id || 'unknown')}
                            ${info.hasMore ? `<span class="dash-detail-hint">showing ${info.builds.length} most recent &mdash; full history in the Builds tab</span>` : ''}
                        </div>
                        ${body}
                    </div>
                </td>
            </tr>
        `;
    }).join('');
}

function renderDashboard() {
    if (state.apps.length === 0) {
        return '<div class="empty">No apps found. Register a build to get started.</div>';
    }

    // Sort apps by their most-recent build's built_at (desc). Apps with no
    // loaded builds keep their server order, falling to the end.
    const sortedApps = [...state.apps].sort((a, b) => {
        const aTs = state.buildsByApp[a.name]?.items?.[0]?.built_at || '';
        const bTs = state.buildsByApp[b.name]?.items?.[0]?.built_at || '';
        if (aTs && bTs) return bTs.localeCompare(aTs);
        if (aTs) return -1;
        if (bTs) return 1;
        return 0;
    });

    // Computed once per render and reused by the stat strip, the filter and
    // the rows (renderDashRows also runs standalone on partial re-render).
    state.dashInfos = sortedApps.map(app => ({ app, info: getAppDashInfo(app) }));

    const cols = dashEnvColumns();
    const visibleCount = dashVisibleApps(state.dashInfos).length;

    return renderDashStats(state.dashInfos.map(x => x.info))
        + renderDashToolbar(visibleCount, state.dashInfos.length)
        + `
        <div class="dash-table-wrap">
            <table class="dash-table">
                <thead>
                    <tr>
                        <th class="dash-app-col">App</th>
                        ${cols.map(e => `<th>${escapeHtml(e)}</th>`).join('')}
                        <th>Latest build</th>
                        <th class="dash-fill-col"></th>
                    </tr>
                </thead>
                <tbody id="dash-body">${renderDashRows()}</tbody>
            </table>
        </div>
    `;
}

// Partial re-render: only the tbody changes, so the filter input keeps focus.
function refreshDashBody() {
    const body = document.getElementById('dash-body');
    if (!body) { renderApp(); return; }
    body.innerHTML = renderDashRows();
    const count = document.getElementById('dash-count');
    if (count) count.textContent = `${dashVisibleApps(state.dashInfos || []).length} of ${(state.dashInfos || []).length}`;
}

function onDashFilter(value) {
    state.dashFilter = value;
    refreshDashBody();
}

function clearDashFilter() {
    state.dashFilter = '';
    state.dashMode = 'all';
    renderApp();
}

function setDashMode(mode) {
    state.dashMode = state.dashMode === mode ? 'all' : mode;
    renderApp();
}

function toggleDashApp(appName) {
    if (state.expandedApps.has(appName)) {
        state.expandedApps.delete(appName);
    } else {
        state.expandedApps.add(appName);
    }
    refreshDashBody();
}

// Delegated so app names never have to survive a trip through an inline
// onclick attribute.
document.addEventListener('click', (e) => {
    const action = e.target.closest('[data-app-action]');
    if (action) {
        e.stopPropagation();
        runAppAction(action.dataset.appAction, action.dataset.app);
        return;
    }
    const stat = e.target.closest('.dash-stat[data-dash-mode]');
    if (stat) { setDashMode(stat.dataset.dashMode); return; }
    const row = e.target.closest('.dash-row[data-app]');
    if (row) toggleDashApp(row.dataset.app);
});

// Soft delete / restore an app from the dashboard row (admin only).
// Delete only hides it: history stays and a new build brings the app back.
async function runAppAction(action, appName) {
    if (action === 'delete' && !confirm(
        `Hide app "${appName}" from Gondola?\n\n` +
        'Build and deployment history is kept, and registering a new build for ' +
        'this name brings it back.')) return;

    try {
        if (action === 'delete') {
            await api(`/apps/${encodeURIComponent(appName)}`, { method: 'DELETE' });
            state.expandedApps.delete(appName);
        } else {
            await api(`/apps/${encodeURIComponent(appName)}/restore`, { method: 'POST' });
        }
        await fetchApps();
        renderApp();
    } catch (err) {
        alert(`Failed to ${action} app: ` + err.message);
    }
}

// Feature 5: Render deploy button with current state context
function deployKey(buildId, env) { return `${buildId}:${env}`; }

function renderDeployButton(build, env) {
    if (!userCanDeployTo(env)) return '';
    if (!userCanDeployApp(build.app_name)) return '';

    const key = deployKey(build.id, env);
    const status = state.deployStatus[key];
    const wrap = (btn, subtitle) =>
        `<div class="deploy-btn-wrapper" data-deploy-key="${escapeAttr(key)}">${btn}${subtitle}</div>`;
    const sub = (text, color) =>
        `<div class="btn-subtitle"${color ? ` style="color: ${color};"` : ''}>${text}</div>`;

    if (status && status.phase === 'pending') {
        return wrap(
            `<button class="btn-small" disabled title="Deploying to ${escapeAttr(env)}...">
                <span class="spinner spinner-inline"></span>${escapeHtml(env)}
            </button>`,
            sub('deploying...', '#58a6ff')
        );
    }

    if (status && status.phase === 'reset') {
        const msg = status.message || 'waiting for env reset';
        return wrap(
            `<button class="btn-small" disabled title="${escapeAttr(status.title || msg)}">
                <span class="spinner spinner-inline"></span>${escapeHtml(env)}
            </button>`,
            sub(escapeHtml(msg), '#d29922')
        );
    }

    const appDeploys = state.currentDeployments[build.app_name] || {};
    const currentDeploy = appDeploys[env];
    const currentTag = currentDeploy ? currentDeploy.image_tag : null;

    if (status && status.phase === 'error') {
        const msg = status.message || 'deploy failed';
        return wrap(
            `<button class="btn-small btn-rollback" onclick="promote(${build.id}, '${env}')" title="${escapeAttr(msg)} (click to retry)">
                \u21BA ${escapeHtml(env)}
            </button>`,
            sub(escapeHtml(msg), '#f85149')
        );
    }

    // Check if this build is already deployed to this environment
    const isAlreadyDeployed = currentDeploy && currentDeploy.build_id === build.id;
    if (isAlreadyDeployed) {
        return wrap(
            `<button class="btn-small" disabled style="opacity: 0.5; cursor: not-allowed;" title="Already deployed to ${escapeAttr(env)}">
                \u2713 ${escapeHtml(env)}
            </button>`,
            sub('deployed', '#3fb950')
        );
    }

    // Check if an experiment is currently deployed to this env
    const repoId = getAppRepoId(build.app_name);
    const currentIsExp = currentDeploy &&
        currentDeploy.branch &&
        currentDeploy.branch !== getRepoMainBranch(repoId);

    if (currentIsExp) {
        return wrap(
            `<button class="btn-small btn-replace-experiment" onclick="promote(${build.id}, '${env}')">
                \u26A0 ${escapeHtml(env)}
            </button>`,
            sub(`exp: ${escapeHtml(currentDeploy.branch)}`, '#d29922')
        );
    }

    // Determine if this would be a rollback
    const isRollback = currentDeploy && currentDeploy.build_id > build.id;
    const btnClass = isRollback ? 'btn-small btn-rollback' : 'btn-small';
    const icon = isRollback ? '\u23EA ' : '\u25B6 ';

    return wrap(
        `<button class="${btnClass}" onclick="promote(${build.id}, '${env}')" title="Deploy to ${escapeAttr(env)}">
            ${icon}${escapeHtml(env)}
        </button>`,
        sub(currentTag ? `now: ${escapeHtml(currentTag)}` : 'now: ---')
    );
}

function setDeployStatus(buildId, env, status) {
    const key = deployKey(buildId, env);
    if (status) state.deployStatus[key] = status;
    else delete state.deployStatus[key];
    refreshDeployButtons(b => b.id === buildId);
}

function findLoadedBuild(buildId) {
    for (const bucket of [...Object.values(state.buildsByApp), ...Object.values(state.experimentBuildsByApp)]) {
        const b = (bucket.items || []).find(x => x.id === buildId);
        if (b) return b;
    }
    return null;
}

// Repaint deploy buttons in place (the same build+env can be on screen twice:
// Builds tab and an expanded dashboard row).
function refreshDeployButtons(filter) {
    document.querySelectorAll('.deploy-btn-wrapper[data-deploy-key]').forEach(el => {
        const [rawId, env] = el.dataset.deployKey.split(':');
        const build = findLoadedBuild(Number(rawId));
        if (!build || (filter && !filter(build, env))) return;
        const html = renderDeployButton(build, env);
        if (html) el.outerHTML = html;
    });
}

// Feature 4: Get regenerate button tooltip
function getRegenerateTooltip(build) {
    if (build.analysis_from_sha && build.analysis_to_sha) {
        const from = build.analysis_from_sha.substring(0, 7);
        const to = build.analysis_to_sha.substring(0, 7);
        return `Re-analyze changes ${from}..${to}`;
    }

    // Try to find baseline info
    const envCfg = state.environments || {};
    // Default: staging baseline is prod
    const baselineEnv = envCfg['staging']?.baseline || 'prod';
    const appDeploys = state.currentDeployments[build.app_name] || {};
    const baselineDeploy = appDeploys[baselineEnv];
    if (baselineDeploy && baselineDeploy.commit_sha) {
        return `Will compare against ${baselineEnv}'s last deploy (${baselineDeploy.commit_sha.substring(0, 7)})`;
    }

    return 'Will analyze last ~20 commits';
}

// Full build item for builds tab (with actions)
function renderBuildItem(build) {
    const analysis = build.llm_analysis;
    const isExpanded = state.expandedCommits.has(build.id);
    const deploymentBadges = getAllDeploymentBadges(build);
    const statusBadge = getAnalysisStatusBadge(build);
    const riskBadge = build.risk_level ? getRiskBadge(build) : '';

    // Format commit range display
    let commitRangeHtml = '';
    if (build.analysis_from_sha && build.analysis_to_sha) {
        const fromShort = build.analysis_from_sha.substring(0, 7);
        const toShort = build.analysis_to_sha.substring(0, 7);
        if (fromShort === toShort) {
            commitRangeHtml = `<div class="analysis-range" style="font-size: 11px; color: #8b949e; font-family: monospace; margin-top: 4px;">No changes (same as deployed)</div>`;
        } else {
            commitRangeHtml = `<div class="analysis-range" style="font-size: 11px; color: #8b949e; font-family: monospace; margin-top: 4px;">${fromShort}..${toShort}</div>`;
        }
    }

    // Changelog section
    let changelogHtml = '';
    if (build.changelog_text) {
        changelogHtml = `
            <details class="changelog-section" style="margin-top: 8px;">
                <summary style="font-size: 12px; color: #58a6ff; cursor: pointer;">Changelog</summary>
                <pre class="changelog-content" style="font-size: 12px; background: #161b22; padding: 8px; border-radius: 4px; overflow-x: auto; margin-top: 4px;">${escapeHtml(build.changelog_text)}</pre>
            </details>
        `;
    }

    const regenTooltip = getRegenerateTooltip(build);

    return `
        <div class="build-item">
            <div class="build-info">
                <div>
                    <span class="build-tag">${escapeHtml(build.image_tag)}</span>
                    ${renderShaLink(build.commit_sha, build.source_repo)}
                    ${deploymentBadges}
                </div>
                <div class="build-meta">
                    ${escapeHtml(build.branch || 'unknown')}${build.built_by ? ' &middot; ' + escapeHtml(build.built_by) : ''} &middot; ${dateSpan(build.built_at)}
                </div>
                ${(statusBadge || riskBadge) ? `
                    <div class="build-badges">
                        ${statusBadge} ${riskBadge}
                    </div>
                ` : ''}
                ${renderCommitList(build, isExpanded)}
                ${analysis ? `
                    <div class="analysis">
                        ${analysis.needs_review ? `
                            <div class="analysis-header">
                                <span class="needs-review-badge">Needs Review</span>
                            </div>
                        ` : ''}
                        ${commitRangeHtml}
                        <div style="color: #8b949e; margin-top: 4px;">${escapeHtml(analysis.summary || '')}</div>
                        ${analysis.concerns && analysis.concerns.length > 0 ? `
                            <details class="analysis-concerns">
                                <summary>Concerns (${analysis.concerns.length})</summary>
                                <ul>${analysis.concerns.map(c => `<li>${escapeHtml(c)}</li>`).join('')}</ul>
                            </details>
                        ` : ''}
                        ${changelogHtml}
                    </div>
                ` : ''}
            </div>
            ${userCanWrite() ? `
            <div class="build-actions">
                ${getBuildEnvironments(build).map(env => renderDeployButton(build, env)).join('')}
                <button class="btn-small secondary" onclick="regenerateBuildAnalysis(${build.id})" title="${escapeHtml(regenTooltip)}">
                    Regen
                </button>
            </div>
            ` : ''}
        </div>
    `;
}

function renderBuilds() {
    // Get all known apps from both registered apps and loaded buckets,
    // ordered by most-recent build first (apps with no loaded builds go last,
    // alphabetised among themselves so loading state is stable).
    const allApps = [...new Set([...state.apps.map(a => a.name), ...Object.keys(state.buildsByApp)])]
        .sort((a, b) => {
            const aTs = state.buildsByApp[a]?.items?.[0]?.built_at || '';
            const bTs = state.buildsByApp[b]?.items?.[0]?.built_at || '';
            if (aTs && bTs) return bTs.localeCompare(aTs);
            if (aTs) return -1;
            if (bTs) return 1;
            return a.localeCompare(b);
        });

    if (allApps.length === 0) {
        return '<div class="empty">No builds found</div>';
    }

    const pageSizeHtml = `
        <div class="page-size-selector">
            <label>Per app:</label>
            <select onchange="changeBuildPageSize(this.value)">
                ${[5, 10, 20].map(n => `<option value="${n}" ${state.buildPageSize === n ? 'selected' : ''}>${n}</option>`).join('')}
            </select>
        </div>
    `;

    return pageSizeHtml + allApps.map(appName => renderAppBuildsCard(appName)).join('');
}

// One app's build card. Rendered by the Builds tab and repainted standalone
// after an inline deploy.
function renderAppBuildsCard(appName) {
    const bucket = state.buildsByApp[appName];
    const builds = bucket?.items || [];
    const isLoading = bucket?.loading || false;
    const hasMore = bucket?.hasMore ?? true;
    const initialized = bucket?.initialized || false;

    // Per-app loading spinner for uninitialized buckets
    if (!initialized) {
        return `
        <div class="card" data-app-card="${escapeAttr(appName)}">
            <div class="card-header">
                <div class="card-title">${escapeHtml(appName)}</div>
            </div>
            <div class="loading"><span class="spinner"></span>Loading...</div>
        </div>`;
    }

    // Hide builds older than the staging-deployed build by default. The
    // staging build itself and anything newer (e.g. just-registered, not
    // yet deployed) stays visible. No-op when staging deploy is unknown.
    const stagingDeploy = state.currentDeployments[appName]?.staging;
    const stagingBuildId = stagingDeploy?.build_id;
    const showOlder = !!state.showOlderBuilds[appName];
    let visibleBuilds = builds;
    let hiddenCount = 0;
    if (stagingBuildId && !showOlder) {
        visibleBuilds = builds.filter(b => b.id >= stagingBuildId);
        hiddenCount = builds.length - visibleBuilds.length;
    }

    const olderToggleHtml = stagingBuildId && (hiddenCount > 0 || showOlder) ? `
        <div style="text-align: center; margin: 8px 0;">
            <button class="btn-small secondary" onclick="toggleOlderBuilds('${escapeHtml(appName)}')">
                ${showOlder ? 'Hide older builds' : `Show older builds (${hiddenCount})`}
            </button>
        </div>
    ` : '';

    // Load More button per app (uses per-app loading state)
    const loadMoreHtml = hasMore ? `
        <div style="text-align: center; margin-top: 12px; padding-top: 12px; border-top: 1px solid #30363d;">
            <button class="secondary" onclick="loadMoreBuilds('${escapeHtml(appName)}')" ${isLoading ? 'disabled' : ''}>
                ${isLoading ? '<span class="spinner" style="width:14px;height:14px;border-width:2px;margin-right:4px;"></span>Loading...' : 'Load More'}
            </button>
            <span style="font-size: 11px; color: #6e7681; margin-left: 8px;">${builds.length} loaded</span>
        </div>
    ` : (builds.length > 0 ? `
        <div style="text-align: center; margin-top: 12px; padding-top: 12px; border-top: 1px solid #30363d; color: #6e7681; font-size: 12px;">
            All ${builds.length} builds loaded
        </div>
    ` : '');

    return `
    <div class="card" data-app-card="${escapeAttr(appName)}">
        <div class="card-header">
            <div class="card-title">${escapeHtml(appName)}</div>
        </div>
        <div>
            ${builds.length === 0
                ? '<div style="color: #8b949e; font-size: 14px; padding: 16px 0;">No builds found for this app</div>'
                : (visibleBuilds.length === 0
                    ? '<div style="color: #8b949e; font-size: 14px; padding: 16px 0;">No newer builds. Click "Show older builds" to view history.</div>'
                    : visibleBuilds.map(b => renderBuildItem(b)).join(''))
            }
        </div>
        ${olderToggleHtml}
        ${loadMoreHtml}
    </div>
`;
}

function toggleOlderBuilds(appName) {
    state.showOlderBuilds[appName] = !state.showOlderBuilds[appName];
    renderApp();
}

function changeBuildPageSize(value) {
    const size = parseInt(value) || 5;
    state.buildPageSize = size;
    localStorage.setItem('builds_page_size', String(size));
    // Reset all buckets and refetch
    state.buildsByApp = {};
    if (state.apps.length > 0) {
        Promise.all(state.apps.map(a => fetchBuilds(a.name, false))).then(() => renderApp());
    }
}

// Experiment build item — simplified (no analysis, risk, changelog, regen)
function renderExperimentBuildItem(build) {
    const deploymentBadges = getAllDeploymentBadges(build);

    return `
        <div class="build-item">
            <div class="build-info">
                <div>
                    <span class="build-tag">${escapeHtml(build.image_tag)}</span>
                    ${renderShaLink(build.commit_sha, build.source_repo)}
                    <span class="experiment-branch-badge">${escapeHtml(build.branch)}</span>
                    ${deploymentBadges}
                </div>
                <div class="build-meta">
                    ${build.built_by ? escapeHtml(build.built_by) + ' &middot; ' : ''}${dateSpan(build.built_at)}
                </div>
            </div>
            ${userCanWrite() ? `
            <div class="build-actions">
                ${getBuildEnvironments(build).map(env => renderDeployButton(build, env)).join('')}
            </div>
            ` : ''}
        </div>
    `;
}

function renderExperiments() {
    // Filter to only apps that have experiment builds
    const appsWithExperiments = state.apps.filter(a => {
        const bucket = state.experimentBuildsByApp[a.name];
        return bucket?.initialized && bucket.items.length > 0;
    });

    if (appsWithExperiments.length === 0) {
        // Check if any buckets are still loading
        const anyLoading = state.apps.some(a => {
            const bucket = state.experimentBuildsByApp[a.name];
            return bucket && !bucket.initialized;
        });
        if (anyLoading) {
            return '<div class="loading"><span class="spinner"></span>Loading...</div>';
        }
        return '<div class="empty">No experiment builds found. Experiment builds are created when a build is registered from a non-main branch.</div>';
    }

    return appsWithExperiments.map(a => renderExperimentAppCard(a.name)).join('');
}

// One app's experiment card. Rendered by the Experiments tab and repainted
// standalone after an inline deploy.
function renderExperimentAppCard(appName) {
    const bucket = state.experimentBuildsByApp[appName];
    const builds = bucket?.items || [];
    const isLoading = bucket?.loading || false;
    const hasMore = bucket?.hasMore ?? true;

    const loadMoreHtml = hasMore ? `
        <div style="text-align: center; margin-top: 12px; padding-top: 12px; border-top: 1px solid #30363d;">
            <button class="secondary" onclick="loadMoreExperimentBuilds('${escapeHtml(appName)}')" ${isLoading ? 'disabled' : ''}>
                ${isLoading ? '<span class="spinner" style="width:14px;height:14px;border-width:2px;margin-right:4px;"></span>Loading...' : 'Load More'}
            </button>
            <span style="font-size: 11px; color: #6e7681; margin-left: 8px;">${builds.length} loaded</span>
        </div>
    ` : (builds.length > 0 ? `
        <div style="text-align: center; margin-top: 12px; padding-top: 12px; border-top: 1px solid #30363d; color: #6e7681; font-size: 12px;">
            All ${builds.length} experiment builds loaded
        </div>
    ` : '');

    return `
    <div class="card" data-exp-card="${escapeAttr(appName)}">
        <div class="card-header">
            <div class="card-title">${escapeHtml(appName)}</div>
        </div>
        ${renderDeployGrid(appName)}
        <div>
            ${builds.map(b => renderExperimentBuildItem(b)).join('')}
        </div>
        ${loadMoreHtml}
    </div>
`;
}

function renderRepos() {
    const headerHtml = `
        <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px;">
            <div style="font-size: 13px; color: #8b949e;">
                ${state.repos.length} repository(s)
            </div>
            <label style="display: flex; align-items: center; gap: 8px; font-size: 13px; color: #c9d1d9; cursor: pointer;">
                <input type="checkbox" ${state.showDeletedRepos ? 'checked' : ''} 
                       onchange="toggleShowDeletedRepos()" 
                       style="cursor: pointer;">
                Show deleted
            </label>
        </div>
    `;

    if (state.repos.length === 0) {
        return headerHtml + '<div class="empty">No repositories configured</div>';
    }

    const reposHtml = state.repos.map(repo => {
        const envBadge = repo.environment
            ? `<span class="env-badge" style="background: #58a6ff20; color: #58a6ff; border: 1px solid #58a6ff40; margin-left: 8px;">${escapeHtml(repo.environment.toUpperCase())}</span>`
            : '';

        const deletedBadge = repo.deleted_at
            ? `<span class="env-badge" style="background: #f8514920; color: #f85149; border: 1px solid #f8514940; margin-left: 8px; font-size: 10px;">DELETED</span>`
            : '';

        // argocd repos only: surface whether this env accepts experiment-branch builds.
        // Mirrors the backend gate; lets operators see at a glance which envs are
        // open to experiments without grepping config.yaml.
        const experimentsBadge = repo.repo_type === 'argocd' && repo.allow_experiments
            ? `<span class="env-badge" style="background: #d2992220; color: #d29922; border: 1px solid #d2992240; margin-left: 8px; font-size: 10px;" title="This argocd repo accepts experiment-branch deploys">EXPERIMENTS</span>`
            : '';

        const allowedEnvs = repo.environments && repo.environments.length > 0
            ? `<div class="repo-envs">Environments: ${repo.environments.map(e => `<span class="repo-env-tag">${escapeHtml(e)}</span>`).join('')}</div>`
            : '';

        // Deployment config display
        let deploymentsHtml = '';
        if (repo.deployments && Object.keys(repo.deployments).length > 0) {
            const deployEntries = Object.entries(repo.deployments);
            deploymentsHtml = `
                <div class="repo-deployments" style="margin-top: 12px; padding: 12px; background: #161b22; border-radius: 6px; border: 1px solid #30363d;">
                    <div style="font-size: 12px; color: #8b949e; margin-bottom: 8px; font-weight: 500;">Deployment Configuration</div>
                    ${deployEntries.map(([env, cfg]) => `
                        <div style="margin-bottom: 8px; padding: 8px; background: #0d1117; border-radius: 4px;">
                            <div style="display: flex; align-items: center; gap: 8px; margin-bottom: 4px;">
                                <span class="env-badge" style="background: #23863620; color: #238636; border: 1px solid #23863640; font-size: 10px;">${escapeHtml(env.toUpperCase())}</span>
                                <span style="font-size: 12px; color: #c9d1d9;">${escapeHtml(cfg.strategy)}</span>
                            </div>
                            <div style="font-size: 11px; color: #8b949e; font-family: monospace;">
                                base: ${escapeHtml(cfg.base_path)}
                            </div>
                            ${cfg.overlay_pattern ? `
                                <div style="font-size: 11px; color: #8b949e;">
                                    pattern: ${escapeHtml(cfg.overlay_pattern)}
                                </div>
                            ` : ''}
                            ${cfg.overlays && cfg.overlays.length > 0 ? `
                                <div style="font-size: 11px; color: #8b949e; margin-top: 4px;">
                                    overlays: ${cfg.overlays.map(o => escapeHtml(o.name)).join(', ')}
                                </div>
                            ` : ''}
                        </div>
                    `).join('')}
                </div>
            `;
        }

        return `
            <div class="card" style="${repo.deleted_at ? 'opacity: 0.6;' : ''}">
                <div class="card-header">
                    <div style="flex: 1; min-width: 0;">
                        <div class="card-title">
                            ${escapeHtml(repo.id)}
                            <span style="font-size: 11px; color: #8b949e; text-transform: uppercase; margin-left: 8px; font-weight: normal;">${escapeHtml(repo.repo_type || 'unknown')}</span>
                            ${envBadge}
                            ${experimentsBadge}
                            ${deletedBadge}
                        </div>
                        ${repo.git_url ? `<div class="card-subtitle repo-url" title="${escapeHtml(repo.git_url)}">${escapeHtml(repo.git_url)}</div>` : ''}
                        ${repo.last_synced
                            ? `<div class="card-subtitle">Last synced: ${dateSpan(repo.last_synced)}</div>`
                            : '<div class="card-subtitle" style="color: #6e7681;">Never synced</div>'}
                        ${allowedEnvs}
                        ${deploymentsHtml}
                    </div>
                    ${userCanWrite() && !repo.deleted_at ? `
                    <button class="btn-small secondary sync-btn" onclick="syncRepo('${escapeHtml(repo.id)}')" title="Sync repository">
                        Sync
                    </button>
                    ` : ''}
                </div>
            </div>
        `;
    }).join('');

    return headerHtml + reposHtml;
}

function toggleShowDeletedRepos() {
    state.showDeletedRepos = !state.showDeletedRepos;
    fetchRepos().then(() => renderApp());
}

function renderDeployments() {
    if (state.deployments.length === 0) {
        return '<div class="empty">No deployments found</div>';
    }

    const deploymentsHtml = state.deployments.map(d => {
        const statusClass = d.status === 'pushed' ? 'risk-low' :
                           d.status === 'failed' ? 'risk-critical' : 'risk-medium';

        const isExpanded = state.expandedDeploymentCommits.has(d.id);

        // Parse changelog if available
        let changelogHtml = '';
        if (d.changelog) {
            try {
                const changelog = JSON.parse(d.changelog);
                if (changelog.commits && changelog.commits.length > 0) {
                    const totalCommits = changelog.commit_count || changelog.commits.length;
                    const displayCommits = isExpanded ? changelog.commits : changelog.commits.slice(0, 3);

                    changelogHtml = `
                        <div class="deployment-commits">
                            <div style="font-weight: 500; margin-bottom: 4px; color: #8b949e;">
                                Changes (${totalCommits} commits):
                            </div>
                            ${displayCommits.map(c => `
                                <div class="commit-row">
                                    ${renderShaLink(c.sha, getAppRepoId(d.app_name))}
                                    <span class="commit-message" title="${escapeHtml(c.message || '')}">${escapeHtml((c.message || '').split('\n')[0].substring(0, 60))}</span>
                                </div>
                            `).join('')}
                            ${changelog.commits.length > 3 ? `
                                <div class="show-all-commits" onclick="toggleDeploymentCommits(${d.id})">
                                    ${isExpanded ? 'Show less' : `Show all ${totalCommits} commits`}
                                </div>
                            ` : ''}
                        </div>
                    `;
                }
            } catch (e) {
                // Ignore parse errors
            }
        }

        const imageTagHtml = d.image_tag
            ? `<span class="build-tag" style="margin-left: 8px; font-size: 11px;">${escapeHtml(d.image_tag)}</span>`
            : '';

        const commitShaHtml = d.commit_sha
            ? renderShaLink(d.commit_sha, getAppRepoId(d.app_name))
            : '';

        return `
            <div class="build-item">
                <div class="build-info">
                    <div>
                        <span class="build-tag">${escapeHtml(d.app_name)}</span>
                        <span style="font-size: 12px; color: #8b949e; margin-left: 8px;">${escapeHtml(d.environment)}</span>
                        <span class="${statusClass}" style="margin-left: 8px; font-size: 11px;">${getDeploymentStatusLabel(d.status)}</span>
                        ${imageTagHtml}
                        ${commitShaHtml}
                    </div>
                    <div class="build-meta">
                        Build #${d.build_id}${d.deployed_by ? ' &middot; ' + escapeHtml(d.deployed_by) : ''} &middot; ${dateSpan(d.deployed_at)}
                    </div>
                    ${d.error_message ? `<div style="color: #f85149; font-size: 12px;">${escapeHtml(d.error_message)}</div>` : ''}
                    ${changelogHtml}
                </div>
            </div>
        `;
    }).join('');

    const loadMoreHtml = state.pagination.deployments.hasMore ? `
        <div style="text-align: center; margin-top: 20px;">
            <button class="secondary" onclick="loadMoreDeployments()" ${state.loading ? 'disabled' : ''}>
                ${state.loading ? 'Loading...' : 'Load More'}
            </button>
        </div>
    ` : '';

    return deploymentsHtml + loadMoreHtml;
}

// Actions with UI feedback
async function switchTab(tab) {
    state.currentTab = tab;
    state.loading = true;
    renderApp();

    try {
        if (tab === 'dashboard') {
            await loadDashboardData();
        } else if (tab === 'builds') {
            state.buildsByApp = {};
            // First fetch apps, then fetch the latest builds for all apps in one batch
            await fetchApps();
            await Promise.all([fetchCurrentDeployments(), fetchEnvironments()]);
            if (state.apps.length > 0) {
                await fetchBuildsBatch('main');
            }
        } else if (tab === 'deployments') {
            state.pagination.deployments = { cursor: '', limit: 5, hasMore: true };
            await fetchDeployments();
        } else if (tab === 'repos') {
            await fetchRepos();
        } else if (tab === 'memory') {
            await Promise.all([fetchRepos(), fetchMemories(), fetchMemorySuggestions()]);
            await selectMemoryRepo(state.memoryRepo || defaultMemoryRepo(), true);
        } else if (tab === 'experiments') {
            state.experimentBuildsByApp = {};
            await Promise.all([fetchApps(), fetchRepos(), fetchCurrentDeployments(), fetchEnvironments()]);
            if (state.apps.length > 0) {
                await fetchBuildsBatch('experiment');
            }
        } else if (tab === 'activity') {
            state.pagination.activity = { cursor: '', limit: 50, hasMore: true };
            await Promise.all([fetchActivity(false), fetchActivityLive()]);
        } else if (tab === 'admin') {
            await Promise.all([fetchAdminUsers(), fetchAppGroups(), fetchApps(), fetchSystemInfo(),
                               fetchAdminConfig(), fetchWebhooks(), fetchDiscovered()]);
        }
    } finally {
        state.loading = false;
        renderApp();
    }
}

async function loadDashboardData() {
    state.buildsByApp = {};

    await Promise.all([fetchApps(), fetchRepos(), fetchCurrentDeployments(), fetchEnvironments()]);
    // Fetch the latest builds for all apps in a single batched request
    if (state.apps.length > 0) {
        await fetchBuildsBatch('main');
    }
    // For admins, prefetch the discovery registry so the dashboard can flag
    // apps as auto-discovered without requiring a visit to the Admin tab.
    if (userIsAdmin()) {
        fetchDiscovered().catch(err => console.error('Discovery prefetch failed:', err));
    }
    resumeEnvResetWatches();
}

// Feature 3: Promote with rollback detection and double confirm on prod
async function promote(buildId, environment) {
    const inFlight = state.deployStatus[deployKey(buildId, environment)]?.phase;
    if (inFlight === 'pending' || inFlight === 'reset') return;

    try {
        // Pre-flight check
        const check = await checkDeployment(buildId, environment);

        if (check.reset_required) {
            // The gate will park this deploy and ask for an environment reset.
            const msg = `\u26A0 EXPERIMENT ACTIVE in ${environment.toUpperCase()}\n\n` +
                `Branch '${check.current_build_branch}' (${check.current_image_tag}) is currently deployed.\n\n` +
                `Deploying ${check.target_image_tag} will first request a reset of ${environment.toUpperCase()} ` +
                `(database, caches, state). The deploy runs by itself once the reset reports back.\n\n` +
                `Request the reset now?`;
            if (!confirm(msg)) return;
        } else if (check.current_is_experiment && !check.target_is_experiment) {
            // Deploying production over an experiment — two-step confirmation
            const msg1 = `\u26A0 EXPERIMENT ACTIVE in ${environment.toUpperCase()}\n\n` +
                `Branch '${check.current_build_branch}' (${check.current_image_tag}) is currently deployed.\n\n` +
                `Before replacing with production build ${check.target_image_tag}, ensure the ` +
                `environment (database, caches, state) has been reset to a clean baseline.\n\n` +
                `Proceed?`;
            if (!confirm(msg1)) return;
            const msg2 = `I confirm the ${environment.toUpperCase()} environment has been reset and is ready ` +
                `for production build ${check.target_image_tag}.`;
            if (!confirm(msg2)) return;
        } else if (check.target_is_experiment) {
            // Deploying an experiment build
            const msg = `Deploy EXPERIMENT build ${check.target_image_tag} ` +
                `(branch: ${check.target_build_branch}) to ${environment}?\n\n` +
                `This environment will be marked as hosting an experiment.`;
            if (!confirm(msg)) return;
        } else if (check.is_rollback) {
            // Rollback scenario
            const msg = `WARNING: ROLLBACK. Deploying build #${buildId} (${check.target_image_tag}) which is older than currently deployed #${check.current_build_id} (${check.current_image_tag}). Continue?`;
            if (!confirm(msg)) return;

            // Double confirm for prod/newprod
            if (environment === 'prod' || environment === 'newprod') {
                const msg2 = `Confirm: rollback ${check.app_name} in ${environment.toUpperCase()} to ${check.target_image_tag}?`;
                if (!confirm(msg2)) return;
            }
        } else {
            // Forward deploy - single confirm
            const currentInfo = check.current_image_tag ? ` (currently: ${check.current_image_tag})` : '';
            if (!confirm(`Deploy build #${buildId} (${check.target_image_tag}) to ${environment}?${currentInfo}`)) return;
        }
    } catch (err) {
        // If pre-flight check fails, fall back to simple confirm
        if (!confirm(`Deploy build #${buildId} to ${environment}?`)) return;
    }

    setDeployStatus(buildId, environment, { phase: 'pending' });

    let result;
    try {
        result = await promoteBuild(buildId, environment);
    } catch (err) {
        setDeployStatus(buildId, environment, { phase: 'error', message: err.message });
        return;
    }

    // 202: parked behind an environment reset. The deploy runs on its own once
    // the reset reports back, so switch to polling instead of refreshing now.
    if (result?.data?.status === 'awaiting_env_reset') {
        watchEnvReset(buildId, environment, result.data.reset, result.data.auto_deploy);
        return;
    }

    // Success: refetch only what the deploy touched, keeping the button on
    // "deploying..." until the fresh data is in, then repaint that app only.
    const appName = findLoadedBuild(buildId)?.app_name;
    try {
        await refreshDeployedApp(appName);
    } catch (err) {
        console.error('Post-deploy refresh failed:', err);
    }
    setDeployStatus(buildId, environment, null);
    if (appName) repaintAppSections(appName);
}

// Refetch this app's builds without losing the pages already loaded.
async function refreshAppBuilds(appName) {
    const bucket = ensureAppBucket(appName);
    const limit = Math.max(state.buildPageSize, bucket.items.length);
    const repoId = getAppRepoId(appName);
    const mainBranch = repoId ? getRepoMainBranch(repoId) : '';

    let url = `/builds?limit=${limit}&app=${encodeURIComponent(appName)}`;
    if (mainBranch) url += `&branch=${encodeURIComponent(mainBranch)}`;

    const data = await api(url);
    setAppBuilds(appName, data.data || [], data.next_cursor);
    checkAndStartPolling();
}

async function refreshDeployedApp(appName) {
    const jobs = [fetchCurrentDeployments()];
    if (appName) {
        jobs.push(refreshAppBuilds(appName));
        if (state.experimentBuildsByApp[appName]?.initialized) {
            jobs.push(fetchExperimentBuilds(appName, false));
        }
    }
    await Promise.all(jobs);
}

// Repaint the parts of the page that show this app's deployment state. No
// full renderApp(): filters, scroll and expanded rows stay put.
function repaintAppSections(appName) {
    if (state.dashInfos?.length) {
        state.dashInfos = state.dashInfos.map(({ app }) => ({ app, info: getAppDashInfo(app) }));
        const stats = document.getElementById('dash-stats');
        if (stats) stats.outerHTML = renderDashStats(state.dashInfos.map(x => x.info));
        if (document.getElementById('dash-body')) refreshDashBody();
    }

    document.querySelectorAll('[data-app-card]').forEach(el => {
        if (el.dataset.appCard === appName) el.outerHTML = renderAppBuildsCard(appName);
    });
    document.querySelectorAll('[data-exp-card]').forEach(el => {
        if (el.dataset.expCard === appName) el.outerHTML = renderExperimentAppCard(appName);
    });
    document.querySelectorAll('[data-deploy-grid]').forEach(el => {
        if (el.dataset.deployGrid === appName) el.outerHTML = renderDeployGrid(appName);
    });

    refreshDeployButtons(b => b.app_name === appName);
}

// --- Environment reset gate -------------------------------------------------
// A deploy that displaces an experiment is parked server-side until the reset
// runner reports back. The button stays on the app while we poll for it.

const RESET_POLL_MS = 5000;
const RESET_POLL_TICKS = 240;       // 20 min of waiting for the runner
const RESET_DEPLOY_TICKS = 60;      // 5 more min for the released deploy
const RESET_POLL_MAX_ERRORS = 3;

function envResetAutoDeploys(environment) {
    return state.environments?.[environment]?.reset_auto_deploy !== false;
}

function envResetMessage(reset) {
    if (reset.status === 'pending') {
        return reset.previous_branch ? `reset queued (${reset.previous_branch})` : 'reset queued';
    }
    if (reset.status === 'running') return 'resetting env...';
    return `reset ${reset.status}`;
}

function envResetTitle(reset) {
    const parts = [`Environment reset #${reset.id} (${reset.status})`];
    if (reset.previous_branch) parts.push(`tearing down experiment ${reset.previous_branch}`);
    if (reset.note) parts.push(reset.note);
    if (reset.error_message) parts.push(reset.error_message);
    return parts.join(' - ');
}

// Park the button on a reset and poll it to completion. One poll loop per
// build+env, so a repaint or a resume never starts a second one.
function watchEnvReset(buildId, environment, reset, autoDeploy) {
    const key = deployKey(buildId, environment);
    setDeployStatus(buildId, environment, {
        phase: 'reset',
        message: envResetMessage(reset),
        title: envResetTitle(reset)
    });
    if (state.resetPolls[key]) return;
    state.resetPolls[key] = true;

    const auto = autoDeploy === undefined ? envResetAutoDeploys(environment) : autoDeploy;
    pollEnvReset(buildId, environment, reset.id, auto).finally(() => {
        delete state.resetPolls[key];
    });
}

async function pollEnvReset(buildId, environment, resetId, autoDeploy) {
    const appName = findLoadedBuild(buildId)?.app_name;
    const finish = async (status) => {
        try {
            await refreshDeployedApp(appName);
        } catch (err) {
            console.error('Post-reset refresh failed:', err);
        }
        setDeployStatus(buildId, environment, status);
        if (appName) repaintAppSections(appName);
    };

    let errors = 0;
    let ticks = RESET_POLL_TICKS;

    for (let i = 0; i < ticks; i++) {
        await new Promise(r => setTimeout(r, RESET_POLL_MS));

        let reset;
        try {
            reset = await fetchEnvReset(resetId);
            errors = 0;
        } catch (err) {
            if (++errors >= RESET_POLL_MAX_ERRORS) {
                setDeployStatus(buildId, environment, { phase: 'error', message: `reset status unavailable: ${err.message}` });
                return;
            }
            continue;
        }

        if (reset.status === 'failed' || reset.status === 'canceled') {
            setDeployStatus(buildId, environment, {
                phase: 'error',
                message: reset.error_message || `reset ${reset.status}`
            });
            return;
        }

        if (reset.status !== 'completed') {
            setDeployStatus(buildId, environment, {
                phase: 'reset',
                message: envResetMessage(reset),
                title: envResetTitle(reset)
            });
            continue;
        }

        // Reset is done. Without auto-deploy nothing else happens server-side:
        // hand the button back so the deploy can be requested again.
        if (!autoDeploy) {
            await finish(null);
            return;
        }
        if (reset.deploy_error) {
            setDeployStatus(buildId, environment, { phase: 'error', message: reset.deploy_error });
            return;
        }
        if (reset.deployment_id) {
            await finish(null);
            return;
        }

        // Reset finished, released deploy still running.
        setDeployStatus(buildId, environment, { phase: 'pending' });
        ticks = Math.min(ticks, i + 1 + RESET_DEPLOY_TICKS);
    }

    setDeployStatus(buildId, environment, { phase: 'error', message: 'reset is taking too long - check the env resets list' });
}

// After a page load, re-attach to resets that are still open so a parked
// deploy does not look like it vanished.
async function resumeEnvResetWatches() {
    let resets;
    try {
        resets = await fetchActiveEnvResets();
    } catch (err) {
        return; // endpoint absent (gate not configured) or not permitted
    }
    for (const reset of resets) {
        if (!userCanDeployTo(reset.environment)) continue;
        watchEnvReset(reset.build_id, reset.environment, reset);
    }
}

async function regenerateBuildAnalysis(buildId) {
    // Fetch build directly from API instead of relying on state
    let build;
    try {
        build = await fetchBuildById(buildId);
    } catch (err) {
        alert('Error: Could not fetch build details: ' + err.message);
        return;
    }

    if (!build) {
        alert('Error: Build not found');
        return;
    }

    state.loading = true;
    renderApp();

    try {
        await regenerateAnalysis(build);
        // Start polling to see the status update
        checkAndStartPolling();
        await loadDashboardData();
    } catch (err) {
        alert('Error: ' + err.message);
    } finally {
        state.loading = false;
        renderApp();
    }
}

// Admin panel
let adminUsers = [];

async function fetchAdminUsers() {
    try {
        const data = await api('/auth/users');
        adminUsers = data.data || [];
    } catch (err) {
        console.error('Failed to fetch users:', err);
        adminUsers = [];
    }
}

async function fetchAppGroups() {
    try {
        const data = await api('/auth/groups');
        state.appGroups = data.data || [];
    } catch (err) {
        console.error('Failed to fetch app groups:', err);
        state.appGroups = [];
    }
}

async function fetchSystemInfo() {
    try {
        const data = await api('/system/info');
        state.systemInfo = data.data || null;
    } catch (err) {
        console.error('Failed to fetch system info:', err);
        state.systemInfo = null;
    }
}

async function fetchAdminConfig() {
    try {
        const data = await api('/admin/config');
        state.adminConfig = data.data || null;
    } catch (err) {
        console.error('Failed to fetch admin config:', err);
        state.adminConfig = null;
    }
}

async function fetchWebhooks() {
    try {
        const data = await api('/admin/webhooks');
        state.webhooks = data.data || [];
    } catch (err) {
        console.error('Failed to fetch webhooks:', err);
        state.webhooks = [];
    }
}

async function testLLM() {
    if (state.llmTestRunning) return;
    state.llmTestRunning = true;
    state.llmTestError = null;
    state.llmTestResult = null;
    renderApp();
    try {
        const promptInput = document.getElementById('llm-test-message');
        const message = promptInput ? promptInput.value : '';
        const data = await api('/admin/llm/test', {
            method: 'POST',
            body: JSON.stringify({ message }),
        });
        state.llmTestResult = data.data || null;
    } catch (err) {
        state.llmTestError = err.message || 'LLM test failed';
    } finally {
        state.llmTestRunning = false;
        renderApp();
    }
}

async function fetchDiscovered() {
    try {
        const data = await api('/admin/repos/discovered');
        const d = data.data || {};
        state.discovered = {
            apps: d.apps || [],
            skipped: d.skipped || [],
            lastScannedAt: d.last_scanned_at || null,
        };
        state.discoverError = null;
    } catch (err) {
        console.error('Failed to fetch discovered apps:', err);
        state.discoverError = err.message || 'Failed to fetch discovered apps';
        state.discovered = { apps: [], skipped: [], lastScannedAt: null };
    }
}

async function triggerDiscoveryScan() {
    if (state.discoverScanning) return; // prevent rapid re-clicks
    state.discoverScanning = true;
    state.discoverError = null;
    renderApp();
    try {
        await api('/admin/repos/discover', { method: 'POST' });
        await fetchDiscovered();
    } catch (err) {
        // Distinguish 504 timeout from other failures.
        const msg = err.message || '';
        if (msg.includes('504') || msg.toLowerCase().includes('timed out')) {
            state.discoverError = 'Scan timed out (60s limit). Some large repos may need a manual git sync first.';
        } else {
            state.discoverError = msg || 'Discovery scan failed';
        }
    } finally {
        state.discoverScanning = false;
        renderApp();
    }
}

function toggleSkipped() {
    state.showSkipped = !state.showSkipped;
    renderApp();
}

// Group skipped apps by category for color-coded display.
// Mirrors the Opus review categorisation: structural / config-issue / policy.
function categorizeSkipReason(reason) {
    if (!reason) return 'structural';
    if (reason.startsWith('annotation:gondola.io/skip')) return 'policy';
    if (reason.startsWith('duplicate-app-name')) return 'policy';
    if (reason === 'helm' || reason === 'multi-source') return 'structural';
    if (reason.startsWith('unsupported-strategy')) return 'structural';
    // Everything else (path-escapes-repo, base-path-not-found, missing-source-path,
    // no-kustomization, invalid-overlays-annotation:*) is a fixable config issue.
    return 'config';
}

function truncateReason(reason, max) {
    if (!reason) return '';
    if (reason.length <= max) return reason;
    return reason.slice(0, max - 1) + '…';
}

// Returns true if the named app is present in the active discovery registry.
function isAppDiscovered(appName) {
    if (!state.discovered || !state.discovered.apps) return false;
    return state.discovered.apps.some(a => a.name === appName);
}

function renderAdmin() {
    const roleOptions = ['admin', 'deployer', 'viewer'];
    const envOptions = getConfiguredEnvironments();

    const info = state.systemInfo || {};
    const cfg = state.adminConfig || {};
    const llm = info.llm || {};
    const cfgLLM = cfg.llm || {};
    const webhooks = state.webhooks || [];

    // Helper to render config value
    const renderValue = (val) => {
        if (val === undefined || val === null || val === '') return '<span style="color: #6e7681; font-style: italic;">not set</span>';
        return escapeHtml(String(val));
    };

    return `
        <div class="card" style="margin-bottom: 16px;">
            <div class="card-header">
                <div class="card-title">User Management</div>
                <button class="btn-small" onclick="showCreateUserForm()">+ New User</button>
            </div>
            <div id="create-user-form" style="display: none; margin-bottom: 16px; padding: 16px; background: #161b22; border-radius: 6px;">
                <form onsubmit="createUser(event)">
                    <div style="display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 8px; margin-bottom: 8px;">
                        <input type="text" id="new-username" placeholder="Username" required style="padding: 6px 10px;">
                        <input type="password" id="new-password" placeholder="Password (min 8 chars)" required minlength="8" style="padding: 6px 10px;">
                        <select id="new-role" style="padding: 6px 10px;">
                            ${roleOptions.map(r => `<option value="${r}">${r}</option>`).join('')}
                        </select>
                    </div>
                    <div style="margin-bottom: 8px;">
                        <label style="font-size: 12px; color: #8b949e;">Allowed Environments (deployer only):</label>
                        <div style="display: flex; gap: 12px; margin-top: 4px;">
                            ${envOptions.map(e => `
                                <label style="font-size: 13px; display: flex; align-items: center; gap: 4px;">
                                    <input type="checkbox" class="new-user-env" value="${e}"> ${e}
                                </label>
                            `).join('')}
                        </div>
                    </div>
                    <div style="margin-bottom: 8px;">
                        <label style="font-size: 12px; color: #8b949e;">Groups (deployer only):</label>
                        <div style="display: flex; gap: 12px; margin-top: 4px; flex-wrap: wrap;">
                            ${state.appGroups.length === 0
                                ? '<span class="scope-hint">no groups defined yet</span>'
                                : state.appGroups.map(g => `
                                    <label style="font-size: 13px; display: flex; align-items: center; gap: 4px;"
                                           title="${escapeAttr((g.patterns || []).join('\n') || 'no patterns')}">
                                        <input type="checkbox" class="new-user-group" value="${g.id}"> ${escapeHtml(g.name)}
                                    </label>
                                `).join('')}
                        </div>
                    </div>
                    <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 8px; margin-bottom: 8px;">
                        <div>
                            <label style="font-size: 12px; color: #8b949e;">Extra allowed apps (one regex per line):</label>
                            <textarea id="new-allowed-apps" class="scope-textarea" rows="3"></textarea>
                        </div>
                        <div>
                            <label style="font-size: 12px; color: #8b949e;">Denied apps (wins over allow):</label>
                            <textarea id="new-denied-apps" class="scope-textarea" rows="3"></textarea>
                        </div>
                    </div>
                    <div style="display: flex; gap: 8px;">
                        <button type="submit" class="btn-small">Create</button>
                        <button type="button" class="btn-small secondary" onclick="document.getElementById('create-user-form').style.display='none'">Cancel</button>
                    </div>
                </form>
            </div>
            ${adminUsers.length === 0 ? '<div class="empty">No users found</div>' : `
                <table class="admin-table">
                    <thead>
                        <tr>
                            <th>Username</th>
                            <th>Role</th>
                            <th>Allowed Envs</th>
                            <th>Apps</th>
                            <th>Last Login</th>
                            <th>Actions</th>
                        </tr>
                    </thead>
                    <tbody>
                        ${adminUsers.map(u => `
                            <tr>
                                <td>${escapeHtml(u.username)}</td>
                                <td><span class="role-badge role-${escapeHtml(u.role || 'admin')}">${escapeHtml(u.role || 'admin')}</span></td>
                                <td>${u.allowed_environments && u.allowed_environments.length > 0 ? u.allowed_environments.map(e => `<span class="repo-env-tag">${escapeHtml(e)}</span>`).join(' ') : '<span style="color: #6e7681;">all</span>'}</td>
                                <td>${renderUserScopeSummary(u)}</td>
                                <td>${u.last_login_at ? dateSpan(u.last_login_at) : '<span style="color: #6e7681;">never</span>'}</td>
                                <td>
                                    <select onchange="updateUserRole(${u.id}, this.value)" style="padding: 2px 6px; font-size: 12px;">
                                        ${roleOptions.map(r => `<option value="${r}" ${(u.role || 'admin') === r ? 'selected' : ''}>${r}</option>`).join('')}
                                    </select>
                                    <button class="btn-small secondary" style="margin-left: 4px; font-size: 11px; padding: 2px 8px;" onclick="toggleUserEditor(${u.id})">${state.editingUserId === u.id ? 'Close' : 'Scope'}</button>
                                    ${u.id !== state.user?.id ? `<button class="btn-small secondary" style="margin-left: 4px; font-size: 11px; padding: 2px 8px;" onclick="deleteAdminUser(${u.id}, '${escapeHtml(u.username)}')">Delete</button>` : ''}
                                </td>
                            </tr>
                            ${state.editingUserId === u.id ? `<tr><td colspan="6" style="padding: 0;">${renderUserScopeEditor(u, envOptions)}</td></tr>` : ''}
                        `).join('')}
                    </tbody>
                </table>
            `}
        </div>

        ${renderAppGroupsCard()}

        <!-- System Info Section -->
        <div class="card" style="margin-bottom: 16px;">
            <div class="card-header">
                <div class="card-title">System Information</div>
                <button class="btn-small secondary" onclick="refreshSystemInfo()">Refresh</button>
            </div>
            <div style="padding: 16px;">
                <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 16px;">
                    <div>
                        <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">Version</div>
                        <div style="font-family: monospace;">${renderValue(info.version)}</div>
                    </div>
                    <div>
                        <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">LLM Enabled</div>
                        <div>${info.llm_enabled ? '<span style="color: #3fb950;">✓ Yes</span>' : '<span style="color: #f85149;">✗ No</span>'}</div>
                    </div>
                    <div>
                        <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">Database Path</div>
                        <div style="font-family: monospace; font-size: 13px;">${renderValue(info.database_path)}</div>
                    </div>
                    <div>
                        <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">Kustomize Binary</div>
                        <div style="font-family: monospace; font-size: 13px;">${renderValue(info.kustomize_binary)}</div>
                    </div>
                    <div>
                        <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">Git Author Name</div>
                        <div>${renderValue(info.git_author?.name)}</div>
                    </div>
                    <div>
                        <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">Git Author Email</div>
                        <div style="font-family: monospace; font-size: 13px;">${renderValue(info.git_author?.email)}</div>
                    </div>
                </div>
            </div>
        </div>

        <!-- LLM Config Section -->
        <div class="card" style="margin-bottom: 16px;">
            <div class="card-header">
                <div class="card-title">LLM Configuration</div>
                <button class="btn-small secondary" onclick="refreshAdminConfig()">Refresh</button>
            </div>
            <div style="padding: 16px;">
                <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 16px; margin-bottom: 16px;">
                    <div>
                        <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">Provider Type</div>
                        <div><span class="role-badge role-deployer">${renderValue(llm.type || 'disabled')}</span></div>
                    </div>
                    <div>
                        <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">Model</div>
                        <div style="font-family: monospace;">${renderValue(llm.model)}</div>
                    </div>
                    <div>
                        <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">Base URL</div>
                        <div style="font-family: monospace; font-size: 13px;">${renderValue(llm.base_url)}</div>
                    </div>
                    <div>
                        <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">Custom Prompts</div>
                        <div>
                            ${llm.prompts?.has_analysis_prompt ? '<span class="repo-env-tag">Analysis</span>' : ''}
                            ${llm.prompts?.has_changelog_prompt ? '<span class="repo-env-tag">Changelog</span>' : ''}
                            ${!llm.prompts?.has_analysis_prompt && !llm.prompts?.has_changelog_prompt ? '<span style="color: #6e7681;">Using defaults</span>' : ''}
                        </div>
                    </div>
                </div>

                <div style="margin-top: 16px; padding-top: 16px; border-top: 1px solid #30363d;">
                    <div style="font-size: 12px; color: #8b949e; margin-bottom: 8px;">Connectivity Test</div>
                    <div style="display: flex; gap: 8px; align-items: center;">
                        <input type="text" id="llm-test-message" placeholder="Optional message (default: 'Say hello in one short sentence.')" style="flex: 1; padding: 6px 10px;" ${!info.llm_enabled ? 'disabled' : ''}>
                        <button class="btn-small" onclick="testLLM()" ${!info.llm_enabled || state.llmTestRunning ? 'disabled' : ''}>${state.llmTestRunning ? 'Sending…' : 'Send'}</button>
                    </div>
                    ${!info.llm_enabled ? '<div style="margin-top: 8px; color: #8b949e; font-size: 12px;">LLM is disabled. Configure a provider in <code>config.yaml</code> to enable testing.</div>' : ''}
                    ${state.llmTestError ? `
                        <div style="margin-top: 12px; padding: 10px; background: #2d1214; border: 1px solid #6e1419; border-radius: 6px; color: #ffa198; font-size: 13px;">
                            <strong>Failed:</strong> ${escapeHtml(state.llmTestError)}
                        </div>
                    ` : ''}
                    ${state.llmTestResult ? `
                        <div style="margin-top: 12px; padding: 10px; background: #0f2419; border: 1px solid #1f6f3e; border-radius: 6px;">
                            <div style="font-size: 12px; color: #8b949e; margin-bottom: 6px;">
                                <span style="color: #3fb950;">✓ Response received</span>
                                · ${escapeHtml(state.llmTestResult.provider || 'unknown')}
                                · <span style="font-family: monospace;">${escapeHtml(state.llmTestResult.model || '')}</span>
                                · ${state.llmTestResult.elapsed_ms}ms
                                ${state.llmTestResult.tokens ? `· ${state.llmTestResult.tokens} tokens` : ''}
                            </div>
                            <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">Prompt:</div>
                            <pre style="background: #161b22; padding: 8px; border-radius: 4px; font-size: 12px; white-space: pre-wrap; word-break: break-word; margin-bottom: 8px;">${escapeHtml(state.llmTestResult.prompt || '')}</pre>
                            <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">Response:</div>
                            <pre style="background: #161b22; padding: 8px; border-radius: 4px; font-size: 12px; white-space: pre-wrap; word-break: break-word; max-height: 300px; overflow-y: auto;">${escapeHtml(state.llmTestResult.response || '')}</pre>
                        </div>
                    ` : ''}
                </div>

                ${cfgLLM.prompts?.analysis_system ? `
                <div style="margin-top: 16px; padding-top: 16px; border-top: 1px solid #30363d;">
                    <div style="font-size: 12px; color: #8b949e; margin-bottom: 8px;">Analysis System Prompt</div>
                    <pre style="background: #161b22; padding: 12px; border-radius: 6px; font-size: 12px; overflow-x: auto; white-space: pre-wrap; word-break: break-word; max-height: 200px; overflow-y: auto;">${escapeHtml(cfgLLM.prompts.analysis_system)}</pre>
                </div>
                ` : ''}
                
                ${cfgLLM.prompts?.changelog_system ? `
                <div style="margin-top: 16px; padding-top: 16px; border-top: 1px solid #30363d;">
                    <div style="font-size: 12px; color: #8b949e; margin-bottom: 8px;">Changelog System Prompt</div>
                    <pre style="background: #161b22; padding: 12px; border-radius: 6px; font-size: 12px; overflow-x: auto; white-space: pre-wrap; word-break: break-word; max-height: 200px; overflow-y: auto;">${escapeHtml(cfgLLM.prompts.changelog_system)}</pre>
                </div>
                ` : ''}
            </div>
        </div>

        <!-- ArgoCD Discovery Section -->
        ${renderDiscoveryCard()}

        <!-- Webhooks Section -->
        <div class="card">
            <div class="card-header">
                <div class="card-title">Webhook Deliveries</div>
                <button class="btn-small secondary" onclick="refreshWebhooks()">Refresh</button>
            </div>
            ${webhooks.length === 0 ? '<div class="empty">No webhook deliveries found</div>' : `
                <table class="admin-table">
                    <thead>
                        <tr>
                            <th>ID</th>
                            <th>Event Type</th>
                            <th>URL</th>
                            <th>Status</th>
                            <th>Attempts</th>
                            <th>Created</th>
                            <th>Actions</th>
                        </tr>
                    </thead>
                    <tbody>
                        ${webhooks.map(w => `
                            <tr>
                                <td>#${w.id}</td>
                                <td><span class="repo-env-tag">${escapeHtml(w.event_type)}</span></td>
                                <td style="font-family: monospace; font-size: 12px; max-width: 200px; overflow: hidden; text-overflow: ellipsis;" title="${escapeHtml(w.url)}">${escapeHtml(w.url)}</td>
                                <td>${renderWebhookStatus(w.status)}${w.last_error ? `<div style="font-size: 11px; color: #f85149; margin-top: 2px; max-width: 240px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;" title="${escapeHtml(w.last_error)}">${escapeHtml(w.last_error)}</div>` : ''}</td>
                                <td>${w.attempts}${w.max_attempts ? '/' + w.max_attempts : ''}</td>
                                <td>${dateSpan(w.created_at)}</td>
                                <td>${(w.status === 'failed' || w.status === 'succeeded') ? `<button class="btn-small secondary" style="font-size: 11px; padding: 2px 8px;" onclick="retryWebhook(${w.id})" ${state.webhookRetrying?.has(w.id) ? 'disabled' : ''}>${state.webhookRetrying?.has(w.id) ? 'Retrying…' : 'Retry'}</button>` : ''}</td>
                            </tr>
                        `).join('')}
                    </tbody>
                </table>
            `}
        </div>
    `;
}

function renderDiscoveryCard() {
    const apps = state.discovered.apps || [];
    const skipped = state.discovered.skipped || [];
    const lastScannedAt = state.discovered.lastScannedAt;
    const scanning = state.discoverScanning;
    const error = state.discoverError;

    const lastScannedDisplay = lastScannedAt
        ? dateSpan(lastScannedAt)
        : '<span style="color: #6e7681; font-style: italic;">never scanned</span>';

    const scanButtonLabel = scanning
        ? '<span class="spinner" style="display: inline-block; vertical-align: middle; margin-right: 6px;"></span>Scanning…'
        : 'Refresh now';

    const errorBanner = error
        ? `<div style="margin-bottom: 12px; padding: 10px 12px; background: #3a2818; border: 1px solid #6c4426; border-radius: 6px; color: #f0b27a; font-size: 13px;">⚠️ ${escapeHtml(error)}</div>`
        : '';

    const activeRows = apps.length === 0
        ? '<div class="empty">No apps discovered. Click "Refresh now" to scan ArgoCD repos.</div>'
        : `<table class="admin-table">
            <thead>
                <tr>
                    <th>Name</th>
                    <th>Environment</th>
                    <th>ArgoCD Repo</th>
                    <th>Manifest</th>
                    <th>Config</th>
                </tr>
            </thead>
            <tbody>
                ${apps.map(a => renderDiscoveredAppRow(a)).join('')}
            </tbody>
        </table>`;

    const skippedSection = skipped.length === 0
        ? ''
        : renderSkippedSection(skipped);

    return `
        <div class="card" style="margin-bottom: 16px;">
            <div class="card-header">
                <div class="card-title">
                    ArgoCD Discovery
                    <span style="font-weight: normal; font-size: 12px; color: #8b949e; margin-left: 8px;">
                        ${apps.length} active${skipped.length > 0 ? `, ${skipped.length} skipped` : ''}
                        &middot; Last scanned: ${lastScannedDisplay}
                    </span>
                </div>
                <button class="btn-small ${scanning ? '' : 'secondary'}" onclick="triggerDiscoveryScan()" ${scanning ? 'disabled' : ''}>
                    ${scanButtonLabel}
                </button>
            </div>
            ${errorBanner}
            ${activeRows}
            ${skippedSection}
        </div>
    `;
}

function renderDiscoveredAppRow(app) {
    const cfg = app.config || {};
    const overlayCount = (cfg.Overlays && cfg.Overlays.length) || 0;
    const overlayInfo = cfg.OverlayPattern
        ? `pattern <code>${escapeHtml(cfg.OverlayPattern)}</code>`
        : overlayCount > 0
            ? `${overlayCount} explicit overlay${overlayCount === 1 ? '' : 's'}`
            : 'single overlay';

    const sourceTooltip = app.source_url ? ` title="${escapeHtml(app.source_url)}"` : '';
    const detailsId = `discovery-cfg-${escapeHtml(app.name)}-${escapeHtml(app.environment)}`;
    const registeredBadge = (state.apps || []).some(x => x.name === app.name)
        ? `<span class="risk-low" style="margin-left: 6px; padding: 1px 6px; border-radius: 8px; font-size: 10px;" title="This app already exists in the apps table — at least one build has been registered.">✓ Registered</span>`
        : '';

    return `
        <tr>
            <td>
                <span class="repo-env-tag" style="font-family: monospace;">${escapeHtml(app.name)}</span>${registeredBadge}
            </td>
            <td><span class="env-badge env-${escapeHtml(app.environment)}">${escapeHtml(app.environment)}</span></td>
            <td style="font-family: monospace; font-size: 12px;"${sourceTooltip}>${escapeHtml(app.argo_repo_id)}</td>
            <td style="font-family: monospace; font-size: 12px; color: #8b949e;">${escapeHtml(app.manifest)}</td>
            <td>
                <details id="${detailsId}">
                    <summary style="cursor: pointer; color: #58a6ff; font-size: 12px;">${cfg.Strategy || 'kustomize'} &middot; ${overlayInfo}</summary>
                    <div style="margin-top: 6px; padding: 8px; background: #161b22; border-radius: 4px; font-family: monospace; font-size: 12px;">
                        <div><span style="color: #8b949e;">base_path:</span> ${escapeHtml(cfg.BasePath || '')}</div>
                        ${cfg.OverlayPattern ? `<div><span style="color: #8b949e;">overlay_pattern:</span> ${escapeHtml(cfg.OverlayPattern)}</div>` : ''}
                        ${overlayCount > 0 ? `
                            <div style="margin-top: 4px;"><span style="color: #8b949e;">overlays:</span></div>
                            <ul style="margin: 4px 0 0 16px; padding: 0;">
                                ${cfg.Overlays.map(o => `<li>${escapeHtml(o.Name)} → <code>${escapeHtml(o.Path)}</code></li>`).join('')}
                            </ul>
                        ` : ''}
                    </div>
                </details>
            </td>
        </tr>
    `;
}

function renderSkippedSection(skipped) {
    // Group by category for color-coded display.
    const groups = { config: [], policy: [], structural: [] };
    for (const s of skipped) {
        const cat = categorizeSkipReason(s.skip_reason);
        groups[cat].push(s);
    }

    const expanded = state.showSkipped;
    const labels = {
        config: { title: 'Config issues (operator can fix)', class: 'risk-medium' },
        policy: { title: 'Policy / intentional', class: 'risk-low' },
        structural: { title: 'Structural (helm, multi-source — out of scope)', class: 'risk-unknown' },
    };

    return `
        <div style="margin-top: 16px; padding-top: 12px; border-top: 1px solid #30363d;">
            <div style="cursor: pointer; user-select: none; font-size: 13px; color: #8b949e;" onclick="toggleSkipped()">
                ${expanded ? '▼' : '▶'} Skipped (${skipped.length}) — click to ${expanded ? 'collapse' : 'expand'}
            </div>
            ${expanded ? Object.keys(groups).filter(k => groups[k].length > 0).map(k => `
                <div style="margin-top: 12px;">
                    <div style="font-size: 12px; color: #8b949e; margin-bottom: 6px;">
                        <span class="${labels[k].class}" style="padding: 1px 6px; border-radius: 8px; font-size: 10px; margin-right: 6px;">${groups[k].length}</span>
                        ${labels[k].title}
                    </div>
                    <table class="admin-table">
                        <tbody>
                            ${groups[k].map(s => {
                                const truncated = truncateReason(s.skip_reason, 80);
                                const titleAttr = s.skip_reason && s.skip_reason.length > 80 ? ` title="${escapeHtml(s.skip_reason)}"` : '';
                                return `
                                    <tr>
                                        <td style="width: 25%;"><span class="repo-env-tag" style="font-family: monospace;">${escapeHtml(s.name)}</span></td>
                                        <td style="width: 15%;"><span class="env-badge env-${escapeHtml(s.environment)}">${escapeHtml(s.environment)}</span></td>
                                        <td style="font-family: monospace; font-size: 12px; color: #8b949e;"${titleAttr}>${escapeHtml(truncated)}</td>
                                    </tr>
                                `;
                            }).join('')}
                        </tbody>
                    </table>
                </div>
            `).join('') : ''}
        </div>
    `;
}

function renderWebhookStatus(status) {
    const statusClasses = {
        'pending': 'risk-medium',
        'delivering': 'risk-medium',
        'succeeded': 'risk-low',
        'failed': 'risk-critical'
    };
    const className = statusClasses[status] || 'risk-unknown';
    return `<span class="${className}" style="padding: 2px 8px; border-radius: 12px; font-size: 11px; text-transform: uppercase;">${escapeHtml(status)}</span>`;
}

async function refreshSystemInfo() {
    await fetchSystemInfo();
    renderApp();
}

async function refreshAdminConfig() {
    await fetchAdminConfig();
    renderApp();
}

async function refreshWebhooks() {
    await fetchWebhooks();
    renderApp();
}

async function retryWebhook(id) {
    if (state.webhookRetrying.has(id)) return;
    state.webhookRetrying.add(id);
    renderApp();
    try {
        await api(`/admin/webhooks/${id}/retry`, { method: 'POST' });
        await fetchWebhooks();
    } catch (err) {
        state.error = err.message || 'Webhook retry failed';
    } finally {
        state.webhookRetrying.delete(id);
        renderApp();
    }
}

// --- App scope (per-user deploy permissions by app name) ---

function linesOf(text) {
    return (text || '').split('\n').map(l => l.trim()).filter(Boolean);
}

function renderUserScopeSummary(u) {
    if ((u.role || 'admin') === 'admin') return '<span style="color: #6e7681;">all</span>';
    if ((u.role || 'admin') === 'viewer') return '<span style="color: #6e7681;">none</span>';

    const parts = (u.groups || []).map(g =>
        `<span class="repo-env-tag" title="${escapeAttr((g.patterns || []).join('\n') || 'no patterns')}">${escapeHtml(g.name)}</span>`);
    const extra = (u.allowed_apps || []).length;
    if (extra > 0) parts.push(`<span class="repo-env-tag">+${extra} pattern${extra > 1 ? 's' : ''}</span>`);
    const denied = (u.denied_apps || []).length;
    if (denied > 0) {
        parts.push(`<span class="scope-deny-tag" title="${escapeAttr((u.denied_apps || []).join('\n'))}">&minus;${denied} denied</span>`);
    }
    if (parts.length === 0) return '<span style="color: #6e7681;">all</span>';
    return parts.join(' ');
}

function renderUserScopeEditor(u, envOptions) {
    const groupIds = new Set((u.groups || []).map(g => g.id));
    return `
        <div class="scope-editor" id="user-scope-${u.id}">
            <div class="scope-grid">
                <div>
                    <label class="scope-label">Allowed environments <span class="scope-hint">(none checked = all)</span></label>
                    <div class="scope-checks">
                        ${envOptions.map(e => `
                            <label><input type="checkbox" class="user-env-cb" value="${escapeAttr(e)}"
                                ${(u.allowed_environments || []).includes(e) ? 'checked' : ''}> ${escapeHtml(e)}</label>
                        `).join('')}
                    </div>
                </div>
                <div>
                    <label class="scope-label">Groups <span class="scope-hint">(each contributes its patterns)</span></label>
                    <div class="scope-checks">
                        ${state.appGroups.length === 0
                            ? '<span class="scope-hint">no groups defined yet</span>'
                            : state.appGroups.map(g => `
                                <label title="${escapeAttr((g.patterns || []).join('\n') || 'no patterns')}">
                                    <input type="checkbox" class="user-group-cb" value="${g.id}"
                                        ${groupIds.has(g.id) ? 'checked' : ''}
                                        onchange="updateScopePreview(${u.id})"> ${escapeHtml(g.name)}</label>
                            `).join('')}
                    </div>
                </div>
                <div>
                    <label class="scope-label">Extra allowed apps <span class="scope-hint">(one regex per line, full match)</span></label>
                    <textarea class="user-allow-apps scope-textarea" rows="4"
                        oninput="updateScopePreview(${u.id})"
                        placeholder="dashboard-.*">${escapeHtml((u.allowed_apps || []).join('\n'))}</textarea>
                </div>
                <div>
                    <label class="scope-label">Denied apps <span class="scope-hint">(wins over every allow)</span></label>
                    <textarea class="user-deny-apps scope-textarea" rows="4"
                        oninput="updateScopePreview(${u.id})"
                        placeholder="core-api">${escapeHtml((u.denied_apps || []).join('\n'))}</textarea>
                </div>
            </div>
            <div class="scope-preview" id="user-scope-preview-${u.id}">${renderScopePreview(userAppScope(u))}</div>
            <div style="display: flex; gap: 8px; margin-top: 10px;">
                <button class="btn-small" onclick="saveUserScope(${u.id})">Save scope</button>
                <button class="btn-small secondary" onclick="toggleUserEditor(${u.id})">Cancel</button>
            </div>
        </div>
    `;
}

function scopeFromEditor(userId) {
    const root = document.getElementById(`user-scope-${userId}`);
    if (!root) return { allow: [], deny: [] };
    const allow = [];
    root.querySelectorAll('.user-group-cb:checked').forEach(cb => {
        const g = state.appGroups.find(x => String(x.id) === cb.value);
        if (g) allow.push(...(g.patterns || []));
    });
    allow.push(...linesOf(root.querySelector('.user-allow-apps').value));
    return { allow, deny: linesOf(root.querySelector('.user-deny-apps').value) };
}

function renderScopePreview(scope) {
    const names = state.apps.map(a => a.name);
    if (names.length === 0) return '<span class="scope-hint">no apps loaded to preview against</span>';
    const matched = names.filter(n => scopePermits(scope, n));
    if (matched.length === names.length) {
        return `<span class="scope-hint">matches all ${names.length} known apps</span>`;
    }
    if (matched.length === 0) {
        return '<span class="scope-none">matches no known app &mdash; this user cannot deploy anything</span>';
    }
    const shown = matched.slice(0, 40);
    return `<span class="scope-hint">matches ${matched.length} of ${names.length} apps:</span> `
        + shown.map(n => `<span class="repo-env-tag">${escapeHtml(n)}</span>`).join(' ')
        + (matched.length > shown.length ? ` <span class="scope-hint">+${matched.length - shown.length} more</span>` : '');
}

function updateScopePreview(userId) {
    const el = document.getElementById(`user-scope-preview-${userId}`);
    if (el) el.innerHTML = renderScopePreview(scopeFromEditor(userId));
}

function toggleUserEditor(userId) {
    state.editingUserId = state.editingUserId === userId ? null : userId;
    renderApp();
}

async function saveUserScope(userId) {
    const root = document.getElementById(`user-scope-${userId}`);
    if (!root) return;

    const envs = Array.from(root.querySelectorAll('.user-env-cb:checked')).map(cb => cb.value);
    const groupIds = Array.from(root.querySelectorAll('.user-group-cb:checked')).map(cb => parseInt(cb.value, 10));

    try {
        await api(`/auth/users/${userId}`, {
            method: 'PUT',
            body: JSON.stringify({
                allowed_environments: envs,
                allowed_apps: linesOf(root.querySelector('.user-allow-apps').value),
                denied_apps: linesOf(root.querySelector('.user-deny-apps').value),
                group_ids: groupIds
            })
        });
        state.editingUserId = null;
        await fetchAdminUsers();
        renderApp();
    } catch (err) {
        alert('Error saving scope: ' + err.message);
    }
}

// --- App groups ---

function renderAppGroupsCard() {
    return `
        <div class="card" style="margin-bottom: 16px;">
            <div class="card-header">
                <div class="card-title">App Groups</div>
                <button class="btn-small" onclick="toggleGroupForm()">${state.creatingGroup ? 'Cancel' : '+ New Group'}</button>
            </div>
            <div style="padding: 0 16px 8px; color: #8b949e; font-size: 12px;">
                A group is a named list of app-name regexes. Attach it to a deployer and they may
                deploy exactly those apps. Patterns match the whole name, so <code>core-api</code>
                does not also grant <code>core-api-worker</code>; write <code>core-api.*</code> for that.
            </div>
            ${state.creatingGroup ? `
                <div style="margin: 0 16px 16px; padding: 16px; background: #161b22; border-radius: 6px;">
                    <form onsubmit="createGroup(event)">
                        <div style="display: grid; grid-template-columns: 1fr 2fr; gap: 8px; margin-bottom: 8px;">
                            <input type="text" id="new-group-name" placeholder="backend" required style="padding: 6px 10px;">
                            <input type="text" id="new-group-desc" placeholder="Description (optional)" style="padding: 6px 10px;">
                        </div>
                        <textarea id="new-group-patterns" class="scope-textarea" rows="4"
                            placeholder="One regex per line, e.g.&#10;core-api&#10;.*-worker"></textarea>
                        <div style="display: flex; gap: 8px; margin-top: 8px;">
                            <button type="submit" class="btn-small">Create</button>
                        </div>
                    </form>
                </div>
            ` : ''}
            ${state.appGroups.length === 0 ? '<div class="empty">No groups defined</div>' : `
                <table class="admin-table">
                    <thead>
                        <tr>
                            <th>Name</th>
                            <th>Patterns</th>
                            <th>Matches</th>
                            <th>Members</th>
                            <th>Actions</th>
                        </tr>
                    </thead>
                    <tbody>
                        ${state.appGroups.map(g => {
                            const matched = state.apps.filter(a => appPatternsMatchAny(a.name, g.patterns));
                            return `
                            <tr>
                                <td>
                                    <div>${escapeHtml(g.name)}</div>
                                    ${g.description ? `<div style="color: #8b949e; font-size: 12px;">${escapeHtml(g.description)}</div>` : ''}
                                </td>
                                <td>${(g.patterns || []).length === 0
                                    ? '<span style="color: #6e7681;">none</span>'
                                    : (g.patterns || []).map(pat => `<code class="scope-pattern">${escapeHtml(pat)}</code>`).join(' ')}</td>
                                <td title="${escapeAttr(matched.map(a => a.name).join('\n'))}">${matched.length} app${matched.length === 1 ? '' : 's'}</td>
                                <td>${g.member_count || 0}</td>
                                <td>
                                    <button class="btn-small secondary" style="font-size: 11px; padding: 2px 8px;" onclick="toggleGroupEditor(${g.id})">${state.editingGroupId === g.id ? 'Close' : 'Edit'}</button>
                                    <button class="btn-small secondary" style="margin-left: 4px; font-size: 11px; padding: 2px 8px;" onclick="deleteAppGroup(${g.id}, '${escapeHtml(g.name)}')">Delete</button>
                                </td>
                            </tr>
                            ${state.editingGroupId === g.id ? `
                                <tr><td colspan="5" style="padding: 0;">
                                    <div class="scope-editor" id="group-editor-${g.id}">
                                        <div style="display: grid; grid-template-columns: 1fr 2fr; gap: 8px; margin-bottom: 8px;">
                                            <input type="text" class="group-name" value="${escapeAttr(g.name)}" style="padding: 6px 10px;">
                                            <input type="text" class="group-desc" value="${escapeAttr(g.description || '')}" placeholder="Description" style="padding: 6px 10px;">
                                        </div>
                                        <label class="scope-label">Patterns <span class="scope-hint">(one regex per line, full match)</span></label>
                                        <textarea class="group-patterns scope-textarea" rows="5"
                                            oninput="updateGroupPreview(${g.id})">${escapeHtml((g.patterns || []).join('\n'))}</textarea>
                                        <div class="scope-preview" id="group-preview-${g.id}">${renderScopePreview({ allow: g.patterns || [], deny: [] })}</div>
                                        <div style="display: flex; gap: 8px; margin-top: 10px;">
                                            <button class="btn-small" onclick="saveAppGroup(${g.id})">Save group</button>
                                            <button class="btn-small secondary" onclick="toggleGroupEditor(${g.id})">Cancel</button>
                                        </div>
                                    </div>
                                </td></tr>
                            ` : ''}
                        `; }).join('')}
                    </tbody>
                </table>
            `}
        </div>
    `;
}

function appPatternsMatchAny(appName, patterns) {
    return (patterns || []).some(pat => appPatternMatches(appName, pat));
}

function toggleGroupForm() {
    state.creatingGroup = !state.creatingGroup;
    renderApp();
}

function toggleGroupEditor(groupId) {
    state.editingGroupId = state.editingGroupId === groupId ? null : groupId;
    renderApp();
}

function updateGroupPreview(groupId) {
    const root = document.getElementById(`group-editor-${groupId}`);
    const el = document.getElementById(`group-preview-${groupId}`);
    if (!root || !el) return;
    el.innerHTML = renderScopePreview({ allow: linesOf(root.querySelector('.group-patterns').value), deny: [] });
}

async function createGroup(event) {
    event.preventDefault();
    try {
        await api('/auth/groups', {
            method: 'POST',
            body: JSON.stringify({
                name: document.getElementById('new-group-name').value,
                description: document.getElementById('new-group-desc').value,
                patterns: linesOf(document.getElementById('new-group-patterns').value)
            })
        });
        state.creatingGroup = false;
        await fetchAppGroups();
        renderApp();
    } catch (err) {
        alert('Error creating group: ' + err.message);
    }
}

async function saveAppGroup(groupId) {
    const root = document.getElementById(`group-editor-${groupId}`);
    if (!root) return;
    try {
        await api(`/auth/groups/${groupId}`, {
            method: 'PUT',
            body: JSON.stringify({
                name: root.querySelector('.group-name').value,
                description: root.querySelector('.group-desc').value,
                patterns: linesOf(root.querySelector('.group-patterns').value)
            })
        });
        state.editingGroupId = null;
        await Promise.all([fetchAppGroups(), fetchAdminUsers()]);
        renderApp();
    } catch (err) {
        alert('Error saving group: ' + err.message);
    }
}

async function deleteAppGroup(groupId, name) {
    if (!confirm(`Delete group "${name}"?`)) return;
    try {
        await api(`/auth/groups/${groupId}`, { method: 'DELETE' });
        await Promise.all([fetchAppGroups(), fetchAdminUsers()]);
        renderApp();
    } catch (err) {
        alert('Error deleting group: ' + err.message);
    }
}

function showCreateUserForm() {
    document.getElementById('create-user-form').style.display = 'block';
}

async function createUser(event) {
    event.preventDefault();
    const username = document.getElementById('new-username').value;
    const password = document.getElementById('new-password').value;
    const role = document.getElementById('new-role').value;
    const envCheckboxes = document.querySelectorAll('.new-user-env:checked');
    const allowedEnvs = Array.from(envCheckboxes).map(cb => cb.value);
    const groupIds = Array.from(document.querySelectorAll('.new-user-group:checked')).map(cb => parseInt(cb.value, 10));
    const allowedApps = linesOf(document.getElementById('new-allowed-apps').value);
    const deniedApps = linesOf(document.getElementById('new-denied-apps').value);

    try {
        await api('/auth/users', {
            method: 'POST',
            body: JSON.stringify({
                username,
                password,
                role,
                allowed_environments: allowedEnvs.length > 0 ? allowedEnvs : undefined,
                group_ids: groupIds,
                allowed_apps: allowedApps,
                denied_apps: deniedApps
            })
        });
        document.getElementById('create-user-form').style.display = 'none';
        await fetchAdminUsers();
        renderApp();
    } catch (err) {
        alert('Error creating user: ' + err.message);
    }
}

async function updateUserRole(userId, newRole) {
    try {
        await api(`/auth/users/${userId}`, {
            method: 'PUT',
            body: JSON.stringify({ role: newRole })
        });
        await fetchAdminUsers();
        renderApp();
    } catch (err) {
        alert('Error updating user: ' + err.message);
    }
}

async function deleteAdminUser(userId, username) {
    if (!confirm(`Delete user "${username}"? This cannot be undone.`)) return;

    try {
        await api(`/auth/users/${userId}`, { method: 'DELETE' });
        await fetchAdminUsers();
        renderApp();
    } catch (err) {
        alert('Error deleting user: ' + err.message);
    }
}

// Utilities
function escapeHtml(text) {
    if (!text) return '';
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}

// escapeHtml leaves quotes alone; attribute values need them escaped too.
function escapeAttr(text) {
    return escapeHtml(text).replace(/"/g, '&quot;');
}

function formatDate(dateStr) {
    if (!dateStr) return 'unknown';
    const date = new Date(dateStr);
    const now = new Date();
    const diff = (now - date) / 1000;

    if (diff < 60) return 'just now';
    if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
    if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
    return date.toLocaleDateString();
}

// Returns a <span> with the relative date as text and full ISO datetime as tooltip
function dateSpan(dateStr, className) {
    if (!dateStr) return 'unknown';
    let iso = dateStr;
    try { iso = new Date(dateStr).toISOString(); } catch (e) {}
    const classAttr = className ? ` class="${className}"` : '';
    return `<span${classAttr} title="${escapeHtml(iso)}">${formatDate(dateStr)}</span>`;
}

// Ultra-short relative age for dense tables ("2h", "3d", "5w"). The full
// timestamp goes in the surrounding element's title.
function formatAge(dateStr) {
    if (!dateStr) return '?';
    const diff = (Date.now() - new Date(dateStr)) / 1000;
    if (!isFinite(diff)) return '?';
    if (diff < 60) return 'now';
    if (diff < 3600) return `${Math.floor(diff / 60)}m`;
    if (diff < 86400) return `${Math.floor(diff / 3600)}h`;
    if (diff < 604800) return `${Math.floor(diff / 86400)}d`;
    if (diff < 2592000) return `${Math.floor(diff / 604800)}w`;
    if (diff < 31536000) return `${Math.floor(diff / 2592000)}mo`;
    return `${Math.floor(diff / 31536000)}y`;
}

// SHA hover tooltip
function renderShaLink(sha, repoID) {
    if (!sha) return '';
    const short = sha.substring(0, 7);
    return `<code class="commit-sha commit-sha-hover" data-sha="${escapeHtml(sha)}" data-repo="${escapeHtml(repoID || '')}">${short}</code>`;
}

const commitInfoCache = {};

async function fetchCommitInfo(repoID, sha) {
    const key = `${repoID}:${sha}`;
    if (commitInfoCache[key]) return commitInfoCache[key];

    try {
        const resp = await api(`/commits/${encodeURIComponent(repoID)}/${encodeURIComponent(sha)}`);
        commitInfoCache[key] = resp.data;
        return resp.data;
    } catch {
        return null;
    }
}

// Tooltip element
const shaTooltip = document.createElement('div');
shaTooltip.id = 'sha-tooltip';
shaTooltip.style.display = 'none';
document.body.appendChild(shaTooltip);

let tooltipTimeout = null;

document.body.addEventListener('mouseenter', async (e) => {
    const el = e.target.closest('.commit-sha-hover');
    if (!el) return;

    const sha = el.dataset.sha;
    const repo = el.dataset.repo;
    if (!sha) return;

    // Clear any pending hide
    if (tooltipTimeout) { clearTimeout(tooltipTimeout); tooltipTimeout = null; }

    // Position tooltip near element
    const rect = el.getBoundingClientRect();
    shaTooltip.style.left = rect.left + 'px';
    shaTooltip.style.top = (rect.bottom + 6 + window.scrollY) + 'px';
    shaTooltip.innerHTML = '<span class="sha-tooltip-loading">Loading...</span>';
    shaTooltip.style.display = 'block';

    if (!repo) {
        // No repo context — show SHA only
        shaTooltip.innerHTML = `<div class="sha-tooltip-sha">${escapeHtml(sha)}</div>`;
        return;
    }

    const info = await fetchCommitInfo(repo, sha);
    // Check if tooltip is still showing for this element
    if (shaTooltip.style.display === 'none') return;

    if (!info) {
        shaTooltip.innerHTML = `<div class="sha-tooltip-sha">${escapeHtml(sha)}</div>`;
        return;
    }

    const firstLine = (info.message || '').split('\n')[0].substring(0, 100);
    const author = info.author || '';
    const dateStr = info.timestamp ? new Date(info.timestamp).toLocaleString() : '';
    const files = info.files_changed || [];

    // Build files list (max 8 files, then +N more)
    let filesHtml = '';
    if (files.length > 0) {
        const displayFiles = files.slice(0, 8);
        const moreCount = files.length - displayFiles.length;
        filesHtml = `
            <div class="sha-tooltip-files">
                ${displayFiles.map(f => `<div class="sha-tooltip-file">${escapeHtml(f)}</div>`).join('')}
                ${moreCount > 0 ? `<div class="sha-tooltip-file-more">+${moreCount} more files</div>` : ''}
            </div>
        `;
    }

    shaTooltip.innerHTML = `
        <div class="sha-tooltip-sha">${escapeHtml(info.sha || sha)}</div>
        <div class="sha-tooltip-message">${escapeHtml(firstLine)}</div>
        <div class="sha-tooltip-meta">${escapeHtml(author)}${dateStr ? ' \u00b7 ' + dateStr : ''}</div>
        ${filesHtml}
    `;
}, true);

document.body.addEventListener('mouseleave', (e) => {
    const el = e.target.closest('.commit-sha-hover');
    if (!el) return;

    tooltipTimeout = setTimeout(() => {
        shaTooltip.style.display = 'none';
    }, 100);
}, true);

// Keep tooltip visible when hovering the tooltip itself
shaTooltip.addEventListener('mouseenter', () => {
    if (tooltipTimeout) { clearTimeout(tooltipTimeout); tooltipTimeout = null; }
});
shaTooltip.addEventListener('mouseleave', () => {
    shaTooltip.style.display = 'none';
});


// ---------------------------------------------------------------------------
// Memory tab
//
// Repo memory is the free-text project context handed to the LLM on every
// analysis (see loadMemoryForBuild on the server). It is keyed by repo id, not
// app name, because one repo can build several apps.
// ---------------------------------------------------------------------------

async function fetchMemories() {
    try {
        const res = await api('/memory');
        state.memories = res.data || [];
    } catch (err) {
        state.memories = [];
        state.memoryError = err.message;
    }
}

// Repos that can hold memory: the application repos, plus any repo id that
// already has memory but is no longer in the config.
function memoryRepoIds() {
    const ids = state.repos
        .filter(r => r.repo_type === 'application')
        .map(r => r.id);
    for (const m of state.memories) {
        if (!ids.includes(m.repo_id)) ids.push(m.repo_id);
    }
    return ids.sort();
}

function defaultMemoryRepo() {
    const withMemory = state.memories.find(m => m.content);
    return withMemory ? withMemory.repo_id : memoryRepoIds()[0] || null;
}

function memoryEntry(repoId) {
    return state.memories.find(m => m.repo_id === repoId) || null;
}

function memoryIsDirty() {
    return state.memoryDraft !== state.memorySaved;
}

async function selectMemoryRepo(repoId, skipRender) {
    if (!repoId) return;
    if (repoId !== state.memoryRepo && memoryIsDirty()
        && !confirm('You have unsaved memory changes. Discard them?')) {
        return;
    }

    state.memoryRepo = repoId;
    state.memorySummary = '';
    state.memoryError = null;
    state.memoryOpenVersion = null;
    state.memoryLoading = true;
    if (!skipRender) renderApp();

    try {
        const [mem, versions] = await Promise.all([
            api(`/memory/${encodeURIComponent(repoId)}`),
            api(`/memory/${encodeURIComponent(repoId)}/versions`)
        ]);
        state.memoryDraft = mem.data?.content || '';
        state.memorySaved = state.memoryDraft;
        state.memoryVersions = versions.data || [];
    } catch (err) {
        state.memoryError = err.message;
        state.memoryVersions = [];
    } finally {
        state.memoryLoading = false;
        if (!skipRender) renderApp();
    }
}

// The textarea must survive typing, and renderApp() replaces the whole DOM, so
// input updates state and touches only the two labels that depend on it.
function onMemoryInput(el) {
    state.memoryDraft = el.value;
    const dirty = document.getElementById('mem-dirty');
    if (dirty) dirty.textContent = memoryIsDirty() ? '● unsaved' : '';
    const count = document.getElementById('mem-count');
    if (count) count.textContent = `${state.memoryDraft.length} chars`;
    const save = document.getElementById('mem-save');
    if (save) save.disabled = !memoryIsDirty();
}

async function saveMemory() {
    if (!state.memoryRepo || state.memorySaving) return;
    const summary = state.memorySummary.trim();

    state.memorySaving = true;
    state.memoryError = null;
    renderApp();

    try {
        await api(`/memory/${encodeURIComponent(state.memoryRepo)}`, {
            method: 'POST',
            body: JSON.stringify({ content: state.memoryDraft, change_summary: summary })
        });
        state.memorySaved = state.memoryDraft;
        state.memorySummary = '';
        await fetchMemories();
        await refreshMemoryVersions();
    } catch (err) {
        state.memoryError = err.message;
    } finally {
        state.memorySaving = false;
        renderApp();
    }
}

async function refreshMemoryVersions() {
    if (!state.memoryRepo) return;
    try {
        const res = await api(`/memory/${encodeURIComponent(state.memoryRepo)}/versions`);
        state.memoryVersions = res.data || [];
    } catch (err) {
        state.memoryError = err.message;
    }
}

function revertMemoryDraft() {
    state.memoryDraft = state.memorySaved;
    renderApp();
}

async function clearMemory() {
    if (!state.memoryRepo) return;
    if (!confirm(`Clear the memory for ${state.memoryRepo}? History is kept, so you can restore an earlier version.`)) return;

    state.memorySaving = true;
    renderApp();
    try {
        await api(`/memory/${encodeURIComponent(state.memoryRepo)}`, { method: 'DELETE' });
        state.memoryDraft = '';
        state.memorySaved = '';
        await fetchMemories();
    } catch (err) {
        state.memoryError = err.message;
    } finally {
        state.memorySaving = false;
        renderApp();
    }
}

async function rollbackMemory(version) {
    if (!state.memoryRepo || state.memorySaving) return;
    if (!confirm(`Restore version ${version}? It is appended as a new version, nothing is lost.`)) return;

    state.memorySaving = true;
    state.memoryError = null;
    renderApp();

    try {
        const res = await api(`/memory/${encodeURIComponent(state.memoryRepo)}/versions/${version}/rollback`, {
            method: 'POST'
        });
        state.memoryDraft = res.data?.content || '';
        state.memorySaved = state.memoryDraft;
        state.memoryOpenVersion = null;
        await Promise.all([fetchMemories(), refreshMemoryVersions()]);
    } catch (err) {
        state.memoryError = err.message;
    } finally {
        state.memorySaving = false;
        renderApp();
    }
}

function toggleMemoryVersion(version) {
    state.memoryOpenVersion = state.memoryOpenVersion === version ? null : version;
    renderApp();
}


// --- Memory suggestions: LLM proposals awaiting review ------------------------
//
// The analysis call returns memory_updates alongside the risk verdict. Nothing
// is applied automatically: each proposal lands here, and accepting one writes
// an ordinary memory version so it can be rolled back like a hand edit.

async function fetchMemorySuggestions() {
    try {
        const res = await api('/memory-suggestions');
        state.memorySuggestions = res.data || [];
        state.memoryPendingByRepo = res.pending_by_repo || {};
    } catch (err) {
        state.memorySuggestions = [];
        state.memoryPendingByRepo = {};
        state.memoryError = err.message;
    }
}

function memoryPendingTotal() {
    return Object.values(state.memoryPendingByRepo || {}).reduce((a, b) => a + b, 0);
}

function suggestionsForRepo(repoId) {
    return state.memorySuggestions.filter(sg => sg.repo_id === repoId);
}

function toggleSuggestionEditor(id) {
    state.memoryEditingSuggestion = state.memoryEditingSuggestion === id ? null : id;
    renderApp();
}

async function resolveSuggestion(id, action) {
    if (state.memoryBusySuggestion) return;

    let body = null;
    if (action === 'accept' && state.memoryEditingSuggestion === id) {
        const el = document.getElementById(`sugg-edit-${id}`);
        if (el) body = { content: el.value };
    }

    state.memoryBusySuggestion = id;
    state.memoryError = null;
    renderApp();

    try {
        await api(`/memory-suggestions/${id}/${action}`, {
            method: 'POST',
            body: JSON.stringify(body || {})
        });
        state.memoryEditingSuggestion = null;

        // Accepting rewrote the memory, so the editor must not keep showing the
        // pre-accept text as if it were unsaved work.
        await Promise.all([fetchMemories(), fetchMemorySuggestions()]);
        if (action === 'accept') {
            const mem = await api(`/memory/${encodeURIComponent(state.memoryRepo)}`);
            state.memoryDraft = mem.data?.content || '';
            state.memorySaved = state.memoryDraft;
            await refreshMemoryVersions();
        }
    } catch (err) {
        state.memoryError = err.message;
    } finally {
        state.memoryBusySuggestion = null;
        renderApp();
    }
}

function renderMemorySuggestions() {
    const items = suggestionsForRepo(state.memoryRepo);
    if (items.length === 0) return '';

    const canWrite = userCanWrite();

    const rows = items.map(sg => {
        const busy = state.memoryBusySuggestion === sg.id;
        const editing = state.memoryEditingSuggestion === sg.id;
        const build = sg.build_id ? `build #${sg.build_id}` : 'manual';

        return `
            <div class="sugg-item">
                <div class="sugg-head">
                    <span class="sugg-action sugg-${escapeAttr(sg.action)}">${escapeHtml(sg.action)}</span>
                    ${sg.section ? `<span class="sugg-section">${escapeHtml(sg.section)}</span>` : ''}
                    <span class="scope-hint">${escapeHtml(build)} · ${dateSpan(sg.created_at)}</span>
                </div>
                <div class="sugg-content">${escapeHtml(sg.content) || '<span class="scope-hint">(whole section)</span>'}</div>
                ${sg.reason ? `<div class="sugg-reason">${escapeHtml(sg.reason)}</div>` : ''}
                ${editing ? `
                    <textarea class="mem-textarea sugg-edit" id="sugg-edit-${sg.id}" rows="4"
                              spellcheck="false">${escapeHtml(sg.content)}</textarea>
                ` : ''}
                ${canWrite ? `
                    <div class="sugg-actions">
                        <button class="btn-small" onclick="resolveSuggestion(${sg.id}, 'accept')" ${busy ? 'disabled' : ''}>
                            ${busy ? 'Working…' : (editing ? 'Save & accept' : 'Accept')}
                        </button>
                        <button class="btn-small secondary" onclick="toggleSuggestionEditor(${sg.id})" ${busy ? 'disabled' : ''}>
                            ${editing ? 'Cancel edit' : 'Edit'}
                        </button>
                        <button class="btn-small secondary" onclick="resolveSuggestion(${sg.id}, 'reject')" ${busy ? 'disabled' : ''}>Reject</button>
                    </div>
                ` : '<div class="scope-hint">read-only: your role cannot apply suggestions</div>'}
            </div>`;
    }).join('');

    return `
        <div class="sugg-panel">
            <div class="sugg-panel-head">
                ⚡ ${items.length} suggestion${items.length === 1 ? '' : 's'} from build analysis
                <span class="scope-hint">proposed by the model, applied only when you accept</span>
            </div>
            ${rows}
        </div>
    `;
}

function renderMemory() {
    const repoIds = memoryRepoIds();

    if (repoIds.length === 0) {
        return '<div class="empty">No application repositories configured, so there is nothing to write memory for.</div>';
    }

    const listHtml = repoIds.map(id => {
        const mem = memoryEntry(id);
        const has = !!(mem && mem.content);
        const pending = state.memoryPendingByRepo[id] || 0;
        const meta = has
            ? `v${mem.version} · ${mem.content.length} chars · ${formatAge(mem.updated_at)}`
            : 'no memory yet';
        const pendingTag = pending > 0 ? `<span class="sugg-count">${pending}</span>` : '';
        return `
            <button class="mem-repo ${state.memoryRepo === id ? 'active' : ''}"
                    onclick="selectMemoryRepo('${escapeHtml(id)}')"
                    title="${escapeAttr(has ? `last updated by ${mem.updated_by || 'unknown'}` : 'never written')}">
                <span class="mem-repo-dot ${has ? 'set' : 'unset'}"></span>${escapeHtml(id)}${pendingTag}
                <span class="mem-repo-meta">${escapeHtml(meta)}</span>
            </button>`;
    }).join('');

    return `
        <div style="font-size: 13px; color: #8b949e; margin-bottom: 12px;">
            Free-text project context prepended to every LLM risk analysis and changelog for that repository.
            Conventions, risky areas, what a migration means here &mdash; anything the model should know before it reads a diff.
        </div>
        <div class="mem-layout">
            <div class="mem-list">${listHtml}</div>
            ${renderMemoryEditor()}
        </div>
    `;
}

function renderMemoryEditor() {
    if (!state.memoryRepo) {
        return '<div class="empty">Pick a repository to edit its memory.</div>';
    }
    if (state.memoryLoading) {
        return '<div class="empty">Loading memory…</div>';
    }

    const canWrite = userCanWrite();
    const mem = memoryEntry(state.memoryRepo);
    const dirty = memoryIsDirty();

    const errorHtml = state.memoryError
        ? `<div class="empty" style="color: #f85149; margin-bottom: 10px;">${escapeHtml(state.memoryError)}</div>`
        : '';

    let currentMeta;
    if (mem && mem.content) {
        currentMeta = `v${mem.version} · saved ${dateSpan(mem.updated_at)}${mem.updated_by ? ` by ${escapeHtml(mem.updated_by)}` : ''}`;
    } else if (state.memoryVersions.length > 0) {
        currentMeta = 'cleared · restore a version below to bring it back';
    } else {
        currentMeta = 'never saved';
    }

    const writeControls = canWrite ? `
        <input id="mem-summary" class="mem-summary" type="text" maxlength="200"
               value="${escapeAttr(state.memorySummary)}"
               oninput="state.memorySummary = this.value"
               placeholder="what changed (optional)">
        <button class="btn-small secondary" onclick="revertMemoryDraft()" ${dirty ? '' : 'disabled'}>Revert</button>
        <button class="btn-small" id="mem-save" onclick="saveMemory()"
                ${dirty && !state.memorySaving ? '' : 'disabled'}>
            ${state.memorySaving ? 'Saving…' : 'Save'}
        </button>
        ${mem && mem.content ? `<button class="btn-small secondary" onclick="clearMemory()" title="Clear the current memory (history is kept)">Clear</button>` : ''}
    ` : '<span class="scope-hint">read-only: your role cannot edit memory</span>';

    return `
        <div class="mem-editor">
            ${errorHtml}
            ${renderMemorySuggestions()}
            <div class="mem-toolbar">
                <strong style="font-size: 14px;">${escapeHtml(state.memoryRepo)}</strong>
                <span class="scope-hint">${currentMeta}</span>
                <span class="mem-dirty" id="mem-dirty">${dirty ? '● unsaved' : ''}</span>
                <span class="spacer"></span>
                ${writeControls}
            </div>
            <textarea class="mem-textarea" id="mem-content" spellcheck="false"
                      oninput="onMemoryInput(this)" ${canWrite ? '' : 'readonly'}
                      placeholder="## Conventions&#10;- migrations under db/migrations are applied by hand in prod&#10;- anything touching billing/ is high risk">${escapeHtml(state.memoryDraft)}</textarea>
            <div style="display: flex; gap: 12px; margin-top: 6px;">
                <span class="mem-count" id="mem-count">${state.memoryDraft.length} chars</span>
                <span class="scope-hint">Plain text or markdown &mdash; it is passed to the model verbatim.</span>
            </div>
            ${renderMemoryHistory()}
        </div>
    `;
}

function renderMemoryHistory() {
    if (state.memoryVersions.length === 0) {
        return '<div style="margin-top: 16px;" class="scope-hint">No history yet.</div>';
    }

    const rows = state.memoryVersions.map(v => {
        const open = state.memoryOpenVersion === v.version;
        return `
            <div class="mem-version">
                <div class="mem-version-head" onclick="toggleMemoryVersion(${v.version})">
                    <span>${open ? '▾' : '▸'}</span>
                    <span class="mem-version-num">v${v.version}</span>
                    <span style="color: #c9d1d9;">${escapeHtml(v.change_summary || '')}</span>
                    <span class="scope-hint">${escapeHtml(v.updated_by || 'unknown')} · ${dateSpan(v.updated_at)} · ${v.content.length} chars</span>
                    <span class="spacer" style="flex: 1;"></span>
                    ${userCanWrite() ? `<button class="btn-small secondary" onclick="event.stopPropagation(); rollbackMemory(${v.version})">Restore</button>` : ''}
                </div>
                ${open ? `<div class="mem-version-body">${escapeHtml(v.content) || '<span class="scope-hint">(empty)</span>'}</div>` : ''}
            </div>`;
    }).join('');

    return `
        <div style="margin-top: 18px;">
            <div style="font-size: 12px; color: #8b949e; margin-bottom: 4px;">
                History (${state.memoryVersions.length}) &mdash; append-only, a restore adds a new version
            </div>
            ${rows}
        </div>
    `;
}

// Start
init();
