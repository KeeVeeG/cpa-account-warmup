/* No credentials or account data are written to browser storage. */
"use strict";

(() => {
  let bootstrapCapability = new URLSearchParams(window.location.hash.slice(1)).get(
    "bootstrap",
  );
  if (window.location.hash) {
    window.history.replaceState(
      null,
      "",
      window.location.pathname + window.location.search,
    );
  }
  const API_BASE = "/api";
  const OPEN_FROM_PLUGINS =
    "Open Account Warmup from Plugins in EasyCLIProxyAPI.";
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
  const notice = $("#notice");
  const cards = Object.fromEntries(
    PROVIDERS.map((provider) => [provider, $(`[data-provider="${provider}"]`)]),
  );
  const tabs = Object.fromEntries(
    PROVIDERS.map((provider) => [
      provider,
      $(`[data-provider-tab="${provider}"]`),
    ]),
  );
  const state = {
    sessionToken: "",
    connected: false,
    loading: false,
    provider: "claude",
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
          Authorization: `Bearer ${state.sessionToken}`,
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
          : "Unable to connect to the proxy. Reopen Account Warmup from Plugins.",
      );
    } finally {
      clearTimeout(timer);
    }
  }

  function invalidateConnection(message) {
    state.session += 1;
    state.sessionToken = "";
    state.connected = false;
    state.loading = false;
    for (const provider of PROVIDERS) {
      if (state.runs[provider]?.active) state.runs[provider].stop = true;
    }
    showNotice(message);
    render();
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
      $("#last-refreshed").textContent =
        `Updated ${new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
    } catch (error) {
      if (session !== state.session) return;
      if (error.status === 401 || error.status === 403)
        invalidateConnection(
          "Session expired. Reopen Account Warmup from Plugins.",
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
    if (account.disabled)
      return {
        ...result,
        status: "disabled",
        message: account.reason || "This account is disabled in CLIProxyAPI.",
      };
    if (account.unavailable)
      return {
        ...result,
        status: "unavailable",
        message: account.reason || "This account is currently unavailable.",
      };
    if (!isEligible(account)) {
      return {
        ...result,
        status: "skipped",
        message: account.reason || "This account is not eligible for warmup.",
      };
    }
    return result || { status: "ready", message: "No request sent" };
  }

  function selectProvider(provider) {
    state.provider = provider;
    for (const name of PROVIDERS) {
      const selected = name === provider;
      tabs[name].setAttribute("aria-selected", String(selected));
      tabs[name].tabIndex = selected ? 0 : -1;
      cards[name].hidden = !selected;
    }
  }

  function renderCard(provider) {
    const card = cards[provider];
    const accounts = accountList(provider);
    const selected = selectedAccounts(provider);
    const run = state.runs[provider];
    const busy = run?.active === true;
    const enabled = state.connected && !state.loading && !busy;
    $(".account-count", tabs[provider]).textContent = String(accounts.length);
    tabs[provider].dataset.running = String(busy);
    tabs[provider].setAttribute(
      "aria-label",
      `${providerName(provider)}, ${accounts.length} account${accounts.length === 1 ? "" : "s"}${busy ? ", warmup running" : ""}`,
    );
    $(".model-value", card).textContent = state.config[`${provider}_model`];
    $(".selection-count", card).textContent = state.loading
      ? "Loading accounts…"
      : state.connected
        ? `${selected.length} of ${accounts.filter(isEligible).length} eligible selected`
        : "No accounts loaded";
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
          ? `No ${providerName(provider)} accounts found.`
          : "No accounts loaded.";
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
        const label =
          typeof account.label === "string" && account.label
            ? account.label
            : account.id;
        $(".account-label", row).textContent = label;
        $(".account-label", row).title = label;
        $(".account-provider", row).textContent = providerName(provider);
        $(".account-icon", row).append(
          $(".provider-icon", tabs[provider]).cloneNode(true),
        );
        checkbox.setAttribute("aria-label", `Select ${label}`);
        const details = [];
        if (result.message && result.status !== "success") {
          details.push(result.message);
        }
        if (
          Number.isFinite(result.input_tokens) &&
          Number.isFinite(result.output_tokens)
        ) {
          const tokens = [
            `${result.input_tokens} input`,
            `${result.output_tokens} output`,
          ];
          if (Number.isFinite(result.reasoning_tokens)) {
            tokens.push(`${result.reasoning_tokens} reasoning`);
          }
          details.push(tokens.join(" · "));
        }
        const detail = $(".account-detail", row);
        for (const [index, text] of details.entries()) {
          if (index > 0) detail.append(document.createElement("br"));
          detail.append(document.createTextNode(text));
        }
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
    $("#account-summary").textContent = state.connected
      ? `${state.accounts.length} account${state.accounts.length === 1 ? "" : "s"}`
      : "Account Warmup";
    $("#connection-dot").classList.toggle("connected", state.connected);
    $("#connection-label").textContent = state.loading
      ? "Loading…"
      : state.connected
        ? "Connected"
        : "Not connected";
    $("#refresh-button").disabled =
      !state.sessionToken || state.loading || isActive();
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
          message: `Sending a short request to ${state.config[`${provider}_model`]}`,
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
              "Session expired. Reopen Account Warmup from Plugins.",
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

  $("#refresh-button").addEventListener("click", () => {
    void loadState();
  });
  for (const provider of PROVIDERS) {
    const card = cards[provider];
    tabs[provider].addEventListener("click", () => selectProvider(provider));
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
  $(".provider-tabs").addEventListener("keydown", (event) => {
    let provider;
    if (event.key === "Home") provider = PROVIDERS[0];
    else if (event.key === "End") provider = PROVIDERS[PROVIDERS.length - 1];
    else if (event.key === "ArrowLeft" || event.key === "ArrowRight")
      provider = PROVIDERS.find((name) => name !== state.provider);
    else return;
    event.preventDefault();
    selectProvider(provider);
    tabs[provider].focus();
  });
  window.addEventListener("beforeunload", (event) => {
    if (isActive()) {
      event.preventDefault();
      event.returnValue = "";
    }
  });
  async function connect() {
    if (!bootstrapCapability || !/^[a-f0-9]{64}$/.test(bootstrapCapability)) {
      bootstrapCapability = "";
      showNotice(OPEN_FROM_PLUGINS);
      render();
      return;
    }
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 15_000);
    try {
      const request = fetch(API_BASE + "/session", {
        method: "POST",
        credentials: "omit",
        cache: "no-store",
        redirect: "error",
        signal: controller.signal,
        headers: {
          Accept: "application/json",
          Authorization: `Bearer ${bootstrapCapability}`,
        },
      });
      bootstrapCapability = "";
      const response = await request;
      if (!response.ok) throw new Error("Session exchange failed");
      const data = await response.json();
      if (typeof data?.session_token !== "string" || !/^[a-f0-9]{64}$/.test(data.session_token)) {
        throw new Error("Invalid session");
      }
      state.sessionToken = data.session_token;
      await loadState();
    } catch {
      bootstrapCapability = "";
      showNotice(OPEN_FROM_PLUGINS);
      render();
    } finally {
      clearTimeout(timer);
    }
  }
  void connect();
})();
