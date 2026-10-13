"use strict";

(() => {
  const match = /^\/v0\/resource\/plugins\/cliproxy-account-warmup\/launch\/([1-9][0-9]{0,4})\/([a-f0-9]{64})$/.exec(
    window.location.pathname,
  );
  if (!match || Number(match[1]) > 65535 || window.location.search || window.location.hash) {
    document.querySelector("#message").textContent =
      "Reopen Account Warmup from Plugins in EasyCLIProxyAPI.";
    return;
  }
  window.location.replace(
    `http://127.0.0.1:${match[1]}/#bootstrap=${match[2]}`,
  );
})();
