# Account Warmup

A CLIProxyAPI plugin that sends a short arithmetic request to selected Claude or Codex accounts. Uses accounts already connected to EasyCLIProxyAPI, with separate **Warm up Claude** and **Warm up Codex** buttons.

Defaults: `claude-haiku-5-5` with thinking disabled and `gpt-6-luna` with low reasoning. Models can be changed in the plugin settings.

## Install

Requires a local CLIProxyAPI **8.0.27+** core with native plugin support. Tested against 8.0.27. Prebuilt packages require Windows 10+, macOS 15+, or Linux with glibc 2.34+.

1. Download the ZIP for your OS and architecture from [Releases](https://github.com/KeeVeeG/cpa-account-warmup/releases/latest), then stop the core in EasyCLIProxyAPI.
2. Copy the library into the core's plugin directory. On Windows x64: `cpa-core/plugins/windows/amd64/cpa-account-warmup.dll`. Keep this filename without a version suffix.
3. Merge this entry into your existing configuration:

   ```yaml
   plugins:
     enabled: true
     configs:
       cpa-account-warmup:
         enabled: true
         claude_model: claude-haiku-5-5
         codex_model: gpt-6-luna
   ```

4. Start the core and open **Plugins → Account Warmup**.
5. Select accounts and press the provider's button. Opening the page from Plugins signs it in automatically; no Management Key is needed.

The interface follows the app's light or dark theme and uses a temporary loopback session limited to this plugin. Reopen it from Plugins if the session expires. EasyCLIProxyAPI itself does not need to be modified.

To include warmups in Usage history, set `record_usage: true` only if the core requires an API Access key. It is off by default. The plugin uses one-use tokens and does not store an API key. If the core uses a different port, set `core_url` in the plugin settings (default `http://127.0.0.1:8317`).

## Behavior

- Requests are pinned to each selected account. Disabled and unavailable accounts are skipped.
- Each request randomly chooses an operation and operands from 1 to 9, with or without spaces. Division has an integer result.
- Claude has a one-token output limit. Codex uses low reasoning and disables the proxy's automatic image tool; this backend has no hard output cap.
- Host payload overrides still apply. Codex requests carry `X-CLIProxyAPI-Account-Warmup: true` so overrides can target warmup separately.
- The plugin does not retry or switch models. The proxy's own retry settings still apply. Exclude warmup models from third-party rules that change models, providers, or account selection.
- Accounts can be warmed up again as soon as their current request finishes. **Stop** skips pending accounts; the current request continues.
- Success means generation completed. It does not confirm that a five-hour quota window started; check the account's quota panel for that.

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

On Intel Macs, the plugin runs a bundled background helper and stops it when the plugin is unloaded.

## Tests

With CGO enabled and a C compiler available:

```sh
go test -race ./...
go vet ./...
```

Tests use fake host callbacks and do not send requests to real accounts. CI builds native libraries on Windows, Linux, and macOS.

MIT license.
