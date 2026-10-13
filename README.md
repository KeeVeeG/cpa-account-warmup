# Account Warmup

A CLIProxyAPI plugin that sends a small request to selected Claude or Codex accounts. Uses accounts already connected to EasyCLIProxyAPI, with separate **Warm up Claude** and **Warm up Codex** buttons.

Defaults: `claude-haiku-5-5` and `gpt-6-luna`, with thinking disabled. Models can be changed in the plugin settings.

## Build

Requires Go 1.26+ and a C compiler (MinGW on Windows, GCC or Clang on Linux/macOS).

```powershell
# Windows
.\scripts\build.ps1
# Optional: -Compiler 'C:\path\to\gcc.exe'
```

```sh
# Linux / macOS
bash scripts/build.sh
```

The library and ZIP package are written to `dist/`.

## Install

Requires CLIProxyAPI **8.0.27+** with native plugin support. Tested against 8.0.27.

1. Stop the core in EasyCLIProxyAPI.
2. Copy the library into the core's plugin directory. On Windows x64: `cpa-core/plugins/windows/amd64/cliproxy-account-warmup.dll`. Keep this filename without a version suffix.
3. Merge this entry into your existing configuration:

   ```yaml
   plugins:
     enabled: true
     configs:
       cliproxy-account-warmup:
         enabled: true
         claude_model: claude-haiku-5-5
         codex_model: gpt-6-luna
         cooldown_seconds: 300
   ```

4. Start the core and open **Plugins → Account Warmup**.
5. Select accounts and press the provider's button. Opening the page from Plugins signs it in automatically; no Management Key is needed.

The interface uses a temporary loopback session limited to this plugin. Reopen it from Plugins if the session expires. EasyCLIProxyAPI itself does not need to be modified.

## Behavior

- Requests are pinned to each selected account. Disabled and unavailable accounts are skipped.
- Claude has a one-token output limit. Codex requests no reasoning and disables the proxy's automatic image tool; this backend has no hard output cap.
- The plugin does not retry or switch models. The proxy's own retry settings still apply. Exclude warmup models from third-party rules that change models, providers, or account selection.
- A local five-minute guard prevents duplicate clicks. It clears when the plugin restarts. **Stop** skips pending accounts; the current request may still finish.
- Success means generation completed. It does not confirm that a five-hour quota window started; check the account's quota panel for that.
- Core 8.0.27 omits `request_id` for internal calls, so EasyCLIProxyAPI may reject their Usage history entries. The plugin shows the returned token counts.

## Tests

With CGO enabled and a C compiler available:

```sh
go test -race ./...
go vet ./...
```

Tests use fake host callbacks and do not send requests to real accounts. CI builds native libraries on Windows, Linux, and macOS.

MIT license.
