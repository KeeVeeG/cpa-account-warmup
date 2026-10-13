/* No credentials or account data are written to browser storage. */
"use strict";

(() => {
  const API_BASE = "/v0/management/plugins/cliproxy-account-warmup";
  const PROVIDERS = ["claude", "codex"];
  const RESULT_STATUSES = new Set(["success", "error", "skipped", "uncertain"]);
  const STATUS_LABELS = {
    ready: "Ready",
    disabled: "Disabled",
    unavailable: "Unavailable",
    queued: "Queued",
    running: "Sending",
    success: "Succeeded",
    error: "Failed",
    skipped: "Skipped",
    uncertain: "Unconfirmed",
    stopped: "Not sent",
  };
  const $ = (selector, root = document) => root.querySelector(selector);
  const keyInput = $("#management-key");
  const notice = $("#notice");
  const cards = Object.fromEntries(
    PROVIDERS.map((provider) => [provider, $(`[data-provider="${provider}"]`)]),
  );
  const state = {
    key: "",
    connected: false,
    loading: false,
    session: 0,
    accounts: [],
    selections: new Map(),
    results: new Map(),
    config: { claude_model: "claude-haiku-5-5", codex_model: "gpt-6-luna" },
    runs: { claude: null, codex: null },
  };

  class APIError extends Error {
    constructor(message, status = 0) {
      super(message);
      this.status = status;
    }
  }

  function accountKey(provider, id) {
    return JSON.stringify([provider, id]);
  }
  function isActive() {
    return PROVIDERS.some((provider) => state.runs[provider]?.active);
  }
  function showNotice(message = "") {
    notice.textContent = message;
    notice.hidden = !message;
  }
  function isEligible(account) {
    return (
      account.eligible === true && !account.disabled && !account.unavailable
    );
  }
  function providerName(provider) {
    return provider === "claude" ? "Claude" : "Codex";
  }
  function accountList(provider) {
    return state.accounts.filter((account) => account.provider === provider);
  }
  function selectedAccounts(provider) {
    return accountList(provider).filter(
      (account) =>
        isEligible(account) &&
        state.selections.get(accountKey(provider, account.id)),
    );
  }

  async function api(path, body) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), body ? 120_000 : 15_000);
    try {
      const response = await fetch(API_BASE + path, {
        method: body ? "POST" : "GET",
        credentials: "omit",
        cache: "no-store",
        redirect: "error",
        signal: controller.signal,
        headers: {
          Accept: "application/json",
          Authorization: `Bearer ${state.key}`,
          ...(body ? { "Content-Type": "application/json" } : {}),
        },
        ...(body ? { body: JSON.stringify(body) } : {}),
      });
      let data;
      try {
        data = await response.json();
      } catch (error) {
        if (controller.signal.aborted) throw error;
        throw new APIError(
          "The proxy returned an unreadable response.",
          response.status,
        );
      }
      if (!response.ok) {
        // A structured warmup result is authoritative even when its HTTP status is an error.
        if (
          body &&
          response.status !== 401 &&
          response.status !== 403 &&
          data?.auth_id === body.auth_id &&
          RESULT_STATUSES.has(data.status)
        )
          return data;
        const detail =
          typeof data?.error === "string"
            ? data.error
            : typeof data?.message === "string"
              ? data.message
              : `Request failed (HTTP ${response.status}).`;
        throw new APIError(detail, response.status);
      }
      return data;
    } catch (error) {
      if (error instanceof APIError) throw error;
      if (controller.signal.aborted) {
        throw new APIError(
          body
            ? "The warmup timed out. Its outcome is unconfirmed; the plugin will not retry it."
            : "Loading accounts timed out. Check that CLIProxyAPI is running and try again.",
        );
      }
      throw new APIError(
        body
          ? "The connection was interrupted. The request may have reached the provider; the plugin will not retry it automatically."
          : "Unable to connect to the proxy. Check that CLIProxyAPI is running, then try connecting again.",
      );
    } finally {
      clearTimeout(timer);
    }
  }

  function invalidateConnection(message) {
    state.session += 1;
    state.key = "";
    state.connected = false;
    state.loading = false;
    keyInput.value = "";
    for (const provider of PROVIDERS) {
      if (state.runs[provider]?.active) state.runs[provider].stop = true;
    }
    showNotice(message);
    render();
    keyInput.focus();
  }

  function normalizeAccounts(accounts) {
    const seen = new Set();
    return accounts.filter((account) => {
      if (
        !account ||
        typeof account.id !== "string" ||
        !account.id ||
        !PROVIDERS.includes(account.provider)
      )
        return false;
      const key = accountKey(account.provider, account.id);
      if (seen.has(key)) return false;
      seen.add(key);
      if (!state.selections.has(key))
        state.selections.set(key, isEligible(account));
      return true;
    });
  }

  async function loadState() {
    if (state.loading || isActive()) return;
    const session = state.session;
    state.loading = true;
    showNotice();
    render();
    try {
      const data = await api("/state");
      if (session !== state.session) return;
      if (!data || !Array.isArray(data.accounts))
        throw new APIError("The proxy returned an invalid account list.");
      state.accounts = normalizeAccounts(data.accounts);
      for (const provider of PROVIDERS) {
        const model = data.config?.[`${provider}_model`];
        if (typeof model === "string" && model.trim())
          state.config[`${provider}_model`] = model;
      }
      // History is newest first; apply oldest first so each account retains its latest result.
      const history = Array.isArray(data.results)
        ? data.results.slice().reverse()
        : [];
      for (const result of history) {
        if (
          PROVIDERS.includes(result?.provider) &&
          typeof result.auth_id === "string" &&
          RESULT_STATUSES.has(result.status)
        ) {
          state.results.set(
            accountKey(result.provider, result.auth_id),
            result,
          );
        }
      }
      state.connected = true;
      keyInput.value = "";
      $("#last-refreshed").textContent =
        `Updated ${new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
    } catch (error) {
      if (session !== state.session) return;
      if (error.status === 401 || error.status === 403)
        invalidateConnection(
          "The management key was rejected. Enter a valid key to reconnect.",
        );
      else showNotice(error.message || "Unable to load accounts.");
    } finally {
      if (session === state.session) {
        state.loading = false;
        render();
      }
    }
  }

  function displayResult(account) {
    const result = state.results.get(accountKey(account.provider, account.id));
    if (result) return result;
    if (account.disabled)
      return {
        status: "disabled",
        message: account.reason || "This account is disabled in CLIProxyAPI.",
      };
    if (account.unavailable)
      return {
        status: "unavailable",
        message: account.reason || "This account is currently unavailable.",
      };
    if (!isEligible(account))
      return {
        status: "skipped",
        message: account.reason || "This account is not eligible for warmup.",
      };
    return { status: "ready", message: "Ready for one minimal request" };
  }

  function renderCard(provider) {
    const card = cards[provider];
    const accounts = accountList(provider);
    const selected = selectedAccounts(provider);
    const run = state.runs[provider];
    const busy = run?.active === true;
    const enabled = state.connected && !state.loading && !busy;
    $(".account-count", card).textContent = state.connected
      ? `${accounts.length} account${accounts.length === 1 ? "" : "s"}`
      : "— accounts";
    $(".model-value", card).textContent = state.config[`${provider}_model`];
    $(".selection-count", card).textContent = state.loading
      ? "Loading accounts…"
      : state.connected
        ? `${selected.length} of ${accounts.filter(isEligible).length} eligible selected`
        : "Connect to load accounts";
    $(".select-all", card).disabled =
      !enabled || accounts.every((account) => !isEligible(account));
    $(".clear-selection", card).disabled = !enabled || selected.length === 0;
    $(".warm-button", card).disabled = !enabled || selected.length === 0;
    $(".stop-button", card).hidden = !busy;
    $(".stop-button", card).disabled = run?.stop === true;
    $(".stop-button", card).textContent = run?.stop ? "Stopping…" : "Stop";

    const list = $(".account-list", card);
    const scrollTop = list.scrollTop;
    const focusedKey = list.contains(document.activeElement)
      ? document.activeElement.dataset.accountKey
      : null;
    let restoreFocus = null;
    list.replaceChildren();
    if (!state.connected || accounts.length === 0) {
      const empty = document.createElement("p");
      empty.className = "empty-state";
      empty.textContent = state.loading
        ? "Loading accounts from your proxy…"
        : state.connected
          ? `No ${providerName(provider)} accounts found. Add an account in CLIProxyAPI, then refresh.`
          : `Your ${providerName(provider)} accounts will appear here.`;
      list.append(empty);
    } else {
      for (const account of accounts) {
        const fragment = $("#account-template").content.cloneNode(true);
        const row = $(".account-row", fragment);
        const checkbox = $(".account-checkbox", row);
        const key = accountKey(provider, account.id);
        const result = displayResult(account);
        row.dataset.disabled = String(!isEligible(account));
        checkbox.dataset.accountKey = key;
        checkbox.checked = state.selections.get(key) === true;
        checkbox.disabled = !enabled || !isEligible(account);
        if (key === focusedKey && !checkbox.disabled) restoreFocus = checkbox;
        checkbox.addEventListener("change", () => {
          state.selections.set(key, checkbox.checked);
          renderCard(provider);
        });
        $(".account-label", row).textContent =
          typeof account.label === "string" && account.label
            ? account.label
            : account.id;
        $(".account-detail", row).textContent =
          typeof result.message === "string" ? result.message : "";
        const badge = $(".account-status", row);
        badge.dataset.status = result.status;
        badge.textContent = STATUS_LABELS[result.status] || "Unknown";
        list.append(fragment);
      }
    }
    list.scrollTop = scrollTop;
    restoreFocus?.focus({ preventScroll: true });
    $(".run-progress", card).hidden = !run;
    if (run) {
      const progress = $("progress", card);
      progress.max = Math.max(1, run.total);
      progress.value = run.completed;
      $(".progress-count", card).textContent =
        `${run.completed} / ${run.total}`;
      $(".progress-message", card).textContent = busy
        ? run.stop
          ? "Stopping after the current request…"
          : "Warming up selected accounts…"
        : run.summary;
    }
  }

  function render() {
    $("#login-panel").hidden = state.connected;
    $("#connect-button").disabled = state.loading || isActive();
    keyInput.disabled = state.loading || isActive();
    $("#connection-dot").classList.toggle("connected", state.connected);
    $("#connection-label").textContent = state.loading
      ? "Connecting…"
      : state.connected
        ? "Connected to proxy"
        : "Not connected";
    $("#lock-button").hidden = !state.connected;
    $("#lock-button").disabled = isActive();
    $("#refresh-button").disabled =
      !state.connected || state.loading || isActive();
    for (const provider of PROVIDERS) renderCard(provider);
  }

  function requestID() {
    if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
    // Localhost is a secure context. This fallback also supports a remote HTTP proxy.
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    bytes[6] = (bytes[6] & 15) | 64;
    bytes[8] = (bytes[8] & 63) | 128;
    const hex = Array.from(bytes, (byte) =>
      byte.toString(16).padStart(2, "0"),
    ).join("");
    return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
  }

  async function warmup(provider) {
    if (!state.connected || state.loading || state.runs[provider]?.active)
      return;
    const accounts = selectedAccounts(provider);
    if (!accounts.length) return;
    const session = state.session;
    const run = {
      active: true,
      stop: false,
      total: accounts.length,
      completed: 0,
      success: 0,
      error: 0,
      skipped: 0,
      uncertain: 0,
      summary: "",
    };
    state.runs[provider] = run;
    for (const account of accounts)
      state.results.set(accountKey(provider, account.id), {
        status: "queued",
        message: "Waiting for this provider's current request",
      });
    showNotice();
    render();
    try {
      for (const account of accounts) {
        if (run.stop || !state.connected || session !== state.session) break;
        const key = accountKey(provider, account.id);
        state.results.set(key, {
          status: "running",
          message: `Sending one minimal request to ${state.config[`${provider}_model`]}`,
        });
        renderCard(provider);
        let result;
        try {
          result = await api("/warmup", {
            provider,
            auth_id: account.id,
            request_id: requestID(),
          });
          if (
            result?.auth_id !== account.id ||
            result?.provider !== provider ||
            !RESULT_STATUSES.has(result?.status)
          )
            throw new APIError(
              "The proxy returned an unexpected result. The request may have completed; check before trying again.",
            );
        } catch (error) {
          const authFailure = error.status === 401 || error.status === 403;
          const rejected = error.status >= 400 && error.status < 500;
          result = {
            auth_id: account.id,
            provider,
            status: rejected ? "error" : "uncertain",
            message:
              error.message ||
              "No result was received. Check the account before trying again.",
          };
          run.stop = true;
          if (authFailure)
            invalidateConnection(
              "The management key was rejected. Pending warmups have stopped. Enter a valid key to reconnect.",
            );
          else
            showNotice(
              `${providerName(provider)} warmup stopped: ${result.message}`,
            );
        }
        state.results.set(key, result);
        run.completed += 1;
        run[result.status] += 1;
        renderCard(provider);
      }
    } finally {
      for (const account of accounts) {
        const key = accountKey(provider, account.id);
        if (state.results.get(key)?.status === "queued")
          state.results.set(key, {
            status: "stopped",
            message: "Stopped before sending a request",
          });
      }
      run.active = false;
      const counts = [
        `${run.success} succeeded`,
        ...(run.error ? [`${run.error} failed`] : []),
        ...(run.skipped ? [`${run.skipped} skipped`] : []),
        ...(run.uncertain ? [`${run.uncertain} unconfirmed`] : []),
      ];
      run.summary = `${run.completed < run.total ? "Stopped" : "Finished"} · ${counts.join(" · ")}`;
      render();
    }
  }

  $("#login-form").addEventListener("submit", (event) => {
    event.preventDefault();
    if (state.loading || isActive()) return;
    const key = keyInput.value.trim();
    if (!key) return;
    state.key = key;
    state.session += 1;
    void loadState();
  });
  $("#refresh-button").addEventListener("click", () => {
    void loadState();
  });
  $("#lock-button").addEventListener("click", () => {
    if (isActive()) return;
    state.accounts = [];
    state.results.clear();
    state.selections.clear();
    state.runs = { claude: null, codex: null };
    $("#last-refreshed").textContent = "";
    invalidateConnection("");
  });
  for (const provider of PROVIDERS) {
    const card = cards[provider];
    $(".select-all", card).addEventListener("click", () => {
      for (const account of accountList(provider))
        if (isEligible(account))
          state.selections.set(accountKey(provider, account.id), true);
      renderCard(provider);
    });
    $(".clear-selection", card).addEventListener("click", () => {
      for (const account of accountList(provider))
        state.selections.set(accountKey(provider, account.id), false);
      renderCard(provider);
    });
    $(".warm-button", card).addEventListener("click", () => {
      void warmup(provider);
    });
    $(".stop-button", card).addEventListener("click", () => {
      if (state.runs[provider]?.active) {
        state.runs[provider].stop = true;
        renderCard(provider);
      }
    });
  }
  window.addEventListener("beforeunload", (event) => {
    if (isActive()) {
      event.preventDefault();
      event.returnValue = "";
    }
  });
  render();
})();
