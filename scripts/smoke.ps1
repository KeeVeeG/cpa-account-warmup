#requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$CorePath,
    [string]$LibraryPath,
    [string]$Compiler,
    [switch]$RecordUsage,
    [switch]$StopHelper,
    [string]$WorkDirectory = (Join-Path $PSScriptRoot '..\work\smoke')
)

# This test starts a separate core with no accounts and never sends a model request.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repositoryRoot = Split-Path -Parent $PSScriptRoot
$coreExecutable = (Get-Item -LiteralPath $CorePath -ErrorAction Stop).FullName
$runDirectory = Join-Path ([IO.Path]::GetFullPath($WorkDirectory)) ([Guid]::NewGuid().ToString('N'))
$authDirectory = Join-Path $runDirectory 'auth'
$pluginDirectory = Join-Path $runDirectory 'plugins'
$temporaryDirectory = Join-Path $runDirectory 'tmp'
$null = New-Item -ItemType Directory -Path $authDirectory, $pluginDirectory, $temporaryDirectory -Force

if ($LibraryPath) {
    $library = Get-Item -LiteralPath $LibraryPath -ErrorAction Stop
    if ($library.PSIsContainer -or $library.Name -notmatch '^cpa-account-warmup\.(dll|so|dylib)$') {
        throw 'LibraryPath must point to a built Account Warmup plugin library.'
    }
}
else {
    $buildArguments = @{ OutputDirectory = (Join-Path $runDirectory 'build') }
    if ($Compiler) { $buildArguments.Compiler = $Compiler }
    & (Join-Path $PSScriptRoot 'build.ps1') @buildArguments
    $libraryFiles = @(Get-ChildItem -LiteralPath $buildArguments.OutputDirectory -Recurse -File |
        Where-Object { $_.Name -match '^cpa-account-warmup\.(dll|so|dylib)$' })
    if ($libraryFiles.Count -ne 1) { throw 'The build did not produce exactly one plugin library.' }
    $library = $libraryFiles[0]
}
Copy-Item -LiteralPath $library.FullName -Destination $pluginDirectory

$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
$listener.Start()
try { $port = $listener.LocalEndpoint.Port }
finally { $listener.Stop() }
$baseURL = "http://127.0.0.1:$port"
$managementKey = [Guid]::NewGuid().ToString('N') + [Guid]::NewGuid().ToString('N')
$inferenceKey = [Guid]::NewGuid().ToString('N')
$apiKeys = if ($RecordUsage) { "'$inferenceKey'" } else { '' }
$configPath = Join-Path $runDirectory 'config.yaml'
$config = @"
config-version: 8
server:
  host: '127.0.0.1'
  port: $port
  discovery:
    enabled: false
management:
  allow-remote: false
  secret-key: '$managementKey'
  disable-control-panel: true
  disable-auto-update-panel: true
access:
  api-keys: [$apiKeys]
requests:
  proxy-url: '$baseURL'
oauth:
  auth-dir: $($authDirectory | ConvertTo-Json -Compress)
plugins:
  enabled: true
  dir: $($pluginDirectory | ConvertTo-Json -Compress)
  configs:
    cpa-account-warmup:
      enabled: true
      record_usage: $($RecordUsage.IsPresent.ToString().ToLowerInvariant())
      core_url: '$baseURL'
"@
[IO.File]::WriteAllText($configPath, $config, [Text.UTF8Encoding]::new($false))

$startInfo = [Diagnostics.ProcessStartInfo]::new()
$startInfo.FileName = $coreExecutable
$startInfo.WorkingDirectory = $runDirectory
$startInfo.UseShellExecute = $false
$startInfo.CreateNoWindow = $true
$startInfo.RedirectStandardOutput = $true
$startInfo.RedirectStandardError = $true
foreach ($argument in @('-config', $configPath, '-local-model', '-password', $managementKey)) {
    $startInfo.ArgumentList.Add($argument)
}
# Do not inherit store, Home, OAuth, proxy, or other service credentials.
$startInfo.Environment.Clear()
foreach ($variableName in @('SystemRoot', 'WINDIR', 'PATH')) {
    $value = [Environment]::GetEnvironmentVariable($variableName, 'Process')
    if ($null -ne $value) { $startInfo.Environment[$variableName] = $value }
}
if ($IsWindows) {
    # A plugin must not depend on DLLs available only in the build toolchain.
    $windowsDirectory = [Environment]::GetEnvironmentVariable('SystemRoot', 'Process')
    $startInfo.Environment['PATH'] = (Join-Path $windowsDirectory 'System32') + [IO.Path]::PathSeparator + $windowsDirectory
}
foreach ($variableName in @('USERPROFILE', 'HOME', 'APPDATA', 'LOCALAPPDATA')) {
    $startInfo.Environment[$variableName] = $runDirectory
}
$startInfo.Environment['TEMP'] = $temporaryDirectory
$startInfo.Environment['TMP'] = $temporaryDirectory
$startInfo.Environment['TMPDIR'] = $temporaryDirectory
# The core also checks unrelated client versions in the background. Route those
# requests back into this isolated listener, which does not implement a proxy.
foreach ($variableName in @('HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY')) {
    $startInfo.Environment[$variableName] = $baseURL
}
$startInfo.Environment['NO_PROXY'] = '127.0.0.1,localhost,::1'

$handler = [Net.Http.HttpClientHandler]::new()
$handler.UseProxy = $false
$handler.AllowAutoRedirect = $false
$client = [Net.Http.HttpClient]::new($handler)
$client.Timeout = [TimeSpan]::FromSeconds(3)
$process = [Diagnostics.Process]::new()
$process.StartInfo = $startInfo
$outputTask = $null
$errorTask = $null
$checks = [Collections.Generic.List[string]]::new()
$helperPath = $null
$helperProcessID = $null

function Get-HelperProcessIDs {
    param([string]$Executable)
    $processLines = & /bin/ps -axo 'pid=,command='
    if ($LASTEXITCODE -ne 0) { throw 'Could not inspect the smoke helper process.' }
    foreach ($line in $processLines) {
        if ($line -match '^\s*([0-9]+)\s+(.+)$' -and $Matches[2] -ceq $Executable) {
            [int]$Matches[1]
        }
    }
}

function Invoke-SmokeRequest {
    param(
        [string]$Path,
        [string]$Method = 'GET',
        [switch]$Authenticated,
        [string]$Body,
        [string]$RequestBase = $baseURL,
        [string]$Token,
        [string]$Origin,
        [hashtable]$ExtraHeaders = @{}
    )
    $request = [Net.Http.HttpRequestMessage]::new([Net.Http.HttpMethod]::new($Method), "$RequestBase$Path")
    try {
        if ($Authenticated) {
            $request.Headers.Authorization = [Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $managementKey)
        }
        if ($Token) {
            $request.Headers.Authorization = [Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $Token)
        }
        if ($Origin) { $null = $request.Headers.TryAddWithoutValidation('Origin', $Origin) }
        foreach ($name in $ExtraHeaders.Keys) {
            $null = $request.Headers.TryAddWithoutValidation($name, [string]$ExtraHeaders[$name])
        }
        if ($PSBoundParameters.ContainsKey('Body')) {
            $request.Content = [Net.Http.StringContent]::new($Body, [Text.Encoding]::UTF8, 'application/json')
        }
        $response = $client.SendAsync($request).GetAwaiter().GetResult()
        try {
            return [pscustomobject]@{
                Status = [int]$response.StatusCode
                Body = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
                ContentType = [string]$response.Content.Headers.ContentType
                Location = [string]$response.Headers.Location
                CacheControl = [string]$response.Headers.CacheControl
            }
        }
        finally { $response.Dispose() }
    }
    finally { $request.Dispose() }
}

function Assert-Status {
    param($Response, [int]$Expected, [string]$Name)
    if ($Response.Status -ne $Expected) {
        throw "$Name returned HTTP $($Response.Status), expected $Expected."
    }
    $checks.Add($Name)
}

try {
    if (-not $process.Start()) { throw 'Could not start the isolated core.' }
    $outputTask = $process.StandardOutput.ReadToEndAsync()
    $errorTask = $process.StandardError.ReadToEndAsync()
    $ready = $false
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    while ([DateTime]::UtcNow -lt $deadline -and -not $process.HasExited) {
        try {
            $heartbeat = Invoke-SmokeRequest -Path '/keep-alive' -Authenticated
            if ($heartbeat.Status -eq 200) { $ready = $true; break }
        }
        catch { }
        Start-Sleep -Milliseconds 100
    }
    if (-not $ready) { throw "The isolated core did not become ready. See logs in $runDirectory" }

    $listing = Invoke-SmokeRequest -Path '/v8/management/plugins' -Authenticated
    Assert-Status $listing 200 'Authenticated plugin listing'
    $registered = @(($listing.Body | ConvertFrom-Json).plugins | Where-Object { $_.id -eq 'cpa-account-warmup' })
    if ($registered.Count -ne 1 -or -not $registered[0].registered -or -not $registered[0].effective_enabled) {
        throw 'The plugin was not registered and enabled by the native host.'
    }
    if ($registered[0].metadata.name -ne 'Account Warmup' -or @($registered[0].menus).Count -ne 1) {
        throw 'The registered plugin metadata or menu is incorrect.'
    }
    $checks.Add('Native ABI registration and metadata')
    if ($IsMacOS -and $env:GOARCH -eq 'amd64') {
        $helperDirectories = @(Get-ChildItem -LiteralPath $temporaryDirectory -Directory -Filter 'cpa-account-warmup.*')
        if ($helperDirectories.Count -ne 1) { throw 'Expected one private Intel Mac helper directory.' }
        $helperPath = Join-Path $helperDirectories[0].FullName 'helper'
        $ownerOnly = [IO.UnixFileMode]::UserRead -bor [IO.UnixFileMode]::UserWrite -bor [IO.UnixFileMode]::UserExecute
        if ([IO.File]::GetUnixFileMode($helperDirectories[0].FullName) -ne $ownerOnly -or
            [IO.File]::GetUnixFileMode($helperPath) -ne $ownerOnly) {
            throw 'The Intel Mac helper is not restricted to its owner.'
        }
        $helperProcesses = @(Get-HelperProcessIDs -Executable $helperPath)
        if ($helperProcesses.Count -ne 1) { throw 'Expected one running Intel Mac helper process.' }
        $helperProcessID = $helperProcesses[0]
        $checks.Add('Intel Mac helper runs from an owner-only temporary directory')
    }
    if ($StopHelper -and -not $helperPath) { throw 'StopHelper requires the Intel Mac helper build.' }

    if ($RecordUsage) {
        Assert-Status (Invoke-SmokeRequest -Path '/v1/models') 401 'Native API authentication is preserved'
        Assert-Status (Invoke-SmokeRequest -Path '/v1/models' -Token $inferenceKey) 200 'Ordinary API keys still work'
        $forgedTicket = 'cpa-warmup.' + ('a' * 64)
        $ticketHeaders = @{ 'X-CLIProxyAPI-Warmup-Ticket' = $forgedTicket }
        $reservedRequest = '{"model":"cpa-account-warmup-internal","input":"Reply OK","stream":false}'
        Assert-Status (Invoke-SmokeRequest -Path '/v1/responses' -Method POST -Token $forgedTicket -ExtraHeaders $ticketHeaders -Body $reservedRequest) 401 'Forged warmup authentication is rejected'
        Assert-Status (Invoke-SmokeRequest -Path '/v1/responses' -Method POST -Token $inferenceKey -ExtraHeaders $ticketHeaders -Body $reservedRequest) 403 'A regular API key cannot forge a warmup ticket'
    }
    else {
        Assert-Status (Invoke-SmokeRequest -Path '/v1/models') 200 'Anonymous API access is preserved without Usage recording'
    }

    $resourceBase = '/v0/resource/plugins/cpa-account-warmup'
    $launchPath = [string]$registered[0].menus[0].path
    if ($launchPath -cnotmatch ('^' + [regex]::Escape($resourceBase) + '/launch/([0-9]+)/([a-f0-9]{64})$')) {
        throw 'The plugin menu did not expose a valid launch capability.'
    }
    $localPort = [int]$Matches[1]
    $launchCapability = $Matches[2]
    if ($localPort -lt 1 -or $localPort -gt 65535 -or $localPort -eq $port) {
        throw 'The menu contains an invalid local interface port.'
    }
    $localBase = "http://127.0.0.1:$localPort"
    $checks.Add('Authenticated menu contains an unguessable launch capability')
    $launchResponse = Invoke-SmokeRequest -Path $launchPath
    Assert-Status $launchResponse 200 'Menu launch serves the static bootstrap page'
    $bootstrapSource = [IO.File]::ReadAllText((Join-Path $repositoryRoot 'internal/plugin/web/bootstrap.html'))
    if ($launchResponse.Body -cne $bootstrapSource -or -not $launchResponse.ContentType.StartsWith('text/html') -or
        $launchResponse.Location -or $launchResponse.Body.Contains($launchCapability)) {
        throw 'The launch page contains dynamic content, credentials, or a redirect.'
    }
    $checks.Add('The public launch page is static and contains no credentials')
    $sessionArguments = @{ RequestBase = $localBase; Origin = $localBase; ExtraHeaders = @{ 'Sec-Fetch-Site' = 'same-origin' } }
    Assert-Status (Invoke-SmokeRequest @sessionArguments -Path '/api/session' -Method POST -Body '') 401 'Local session requires a launch capability'
    Assert-Status (Invoke-SmokeRequest @sessionArguments -Path '/api/session' -Method POST -Token $managementKey -Body '') 401 'Local session rejects a management key'
    Assert-Status (Invoke-SmokeRequest -RequestBase $localBase -Origin 'https://invalid.example' -Path '/api/session' -Method POST -Token $launchCapability -Body '') 403 'Local session rejects a foreign origin'
    Assert-Status (Invoke-SmokeRequest -RequestBase $localBase -Origin $localBase -Path '/api/session' -Method POST -Token $launchCapability -Body '') 403 'Local session requires a same-origin fetch'
    Assert-Status (Invoke-SmokeRequest @sessionArguments -Path '/api/session' -Method POST -Token $launchCapability -Body '{}') 400 'Local session rejects a nonempty request body'
    $sessionResponse = Invoke-SmokeRequest @sessionArguments -Path '/api/session' -Method POST -Token $launchCapability -Body ''
    Assert-Status $sessionResponse 200 'Local launch capability creates a session'
    $sessionToken = [string](($sessionResponse.Body | ConvertFrom-Json).session_token)
    if ($sessionToken -cnotmatch '^[a-f0-9]{64}$' -or $sessionToken -ceq $launchCapability -or
        $sessionResponse.CacheControl -ne 'no-store') {
        throw 'The local session response is invalid or cacheable.'
    }
    $checks.Add('Local session token is distinct from the launch capability and is not cached')

    foreach ($asset in @(
        @{ Route = 'status'; File = 'index.html'; Type = 'text/html' },
        @{ Route = 'bootstrap.js'; File = 'bootstrap.js'; Type = 'text/javascript' },
        @{ Route = 'app.js'; File = 'app.js'; Type = 'text/javascript' },
        @{ Route = 'styles.css'; File = 'styles.css'; Type = 'text/css' },
        @{ Route = 'claude.svg'; File = 'claude.svg'; Type = 'image/svg+xml' },
        @{ Route = 'codex.svg'; File = 'codex.svg'; Type = 'image/svg+xml' }
    )) {
        $response = Invoke-SmokeRequest -Path "$resourceBase/$($asset.Route)"
        Assert-Status $response 200 "Public $($asset.File) resource"
        $source = [IO.File]::ReadAllText((Join-Path $repositoryRoot "internal/plugin/web/$($asset.File)"))
        if ($response.Body -cne $source -or -not $response.ContentType.StartsWith($asset.Type)) {
            throw "The host altered $($asset.File) or served it with an incorrect content type."
        }
        if ($response.Body.Contains($sessionToken) -or $response.Body.Contains($launchPath) -or $response.Location) {
            throw "The public $($asset.File) resource disclosed launch or session credentials."
        }
        $checks.Add("Unescaped $($asset.File) content")
    }
    $checks.Add('Core fallback resources do not disclose session or launch credentials')

    $localPage = Invoke-SmokeRequest -RequestBase $localBase -Path '/'
    Assert-Status $localPage 200 'Local interface is served without a management key'
    $indexSource = [IO.File]::ReadAllText((Join-Path $repositoryRoot 'internal/plugin/web/index.html'))
    if ($localPage.Body -cne $indexSource -or $localPage.Body.Contains($sessionToken) -or
        $localPage.Body.Contains($launchPath) -or $localPage.Body.Contains($managementKey) -or
        $localPage.Body -match '<input\b[^>]*\btype\s*=\s*["'']password["'']|id\s*=\s*["''](?:auth-form|management-key|api-key|login-form)["'']') {
        throw 'The local page contains credentials, a management key form, or altered content.'
    }
    $checks.Add('Local HTML contains no management key form or embedded credentials')
    foreach ($asset in @('app.js', 'styles.css', 'claude.svg', 'codex.svg')) {
        $localAsset = Invoke-SmokeRequest -RequestBase $localBase -Path "/$asset"
        Assert-Status $localAsset 200 "Local $asset resource"
        $source = [IO.File]::ReadAllText((Join-Path $repositoryRoot "internal/plugin/web/$asset"))
        if ($localAsset.Body -cne $source -or $localAsset.Body.Contains($sessionToken)) {
            throw "The local $asset resource is altered or contains a session credential."
        }
    }

    $localArguments = @{ RequestBase = $localBase; Origin = $localBase }
    Assert-Status (Invoke-SmokeRequest @localArguments -Path '/api/state') 401 'Local state requires a session'
    Assert-Status (Invoke-SmokeRequest @localArguments -Path '/api/state' -Token $managementKey) 401 'Local state rejects the management key as a session'
    Assert-Status (Invoke-SmokeRequest @localArguments -Path '/api/warmup' -Method POST -Body '{}') 401 'Local warmup requires a session'
    Assert-Status (Invoke-SmokeRequest -RequestBase $localBase -Origin 'https://invalid.example' -Token $sessionToken -Path '/api/state') 403 'Local state rejects a foreign origin'
    $localStateResponse = Invoke-SmokeRequest @localArguments -Path '/api/state' -Token $sessionToken
    Assert-Status $localStateResponse 200 'Local session can read state through the native host'
    $localState = $localStateResponse.Body | ConvertFrom-Json
    if (@($localState.accounts).Count -ne 0 -or @($localState.results).Count -ne 0) {
        throw 'The local session unexpectedly returned accounts or warmup results.'
    }
    $checks.Add('Local session reads the isolated empty account state')
    Assert-Status (Invoke-SmokeRequest @localArguments -Path '/api/warmup' -Method POST -Token $sessionToken -Body '{"provider":"all"}') 400 'Local invalid warmup is rejected without a model request'

    $apiBase = '/v0/management/plugins/cpa-account-warmup'
    Assert-Status (Invoke-SmokeRequest -Path "$apiBase/state") 401 'State requires authentication'
    Assert-Status (Invoke-SmokeRequest -Path "$apiBase/warmup" -Method POST -Body '{}') 401 'Warmup requires authentication'
    $stateResponse = Invoke-SmokeRequest -Path "$apiBase/state" -Authenticated
    Assert-Status $stateResponse 200 'Authenticated state'
    $state = $stateResponse.Body | ConvertFrom-Json
    if (@($state.accounts).Count -ne 0 -or @($state.results).Count -ne 0) {
        throw 'The isolated core unexpectedly returned accounts or warmup results.'
    }
    if ($state.config.claude_model -ne 'claude-haiku-5-5' -or $state.config.codex_model -ne 'gpt-6-luna') {
        throw 'The plugin reported unexpected default models.'
    }
    $checks.Add('Empty isolated account state and default models')
    Assert-Status (Invoke-SmokeRequest -Path "$apiBase/warmup" -Method POST -Authenticated -Body '{"provider":"all"}') 400 'Invalid provider rejected'
    foreach ($provider in @('claude', 'codex')) {
        $body = @{ provider = $provider; auth_id = 'nonexistent-smoke-account'; request_id = [Guid]::NewGuid().ToString() } | ConvertTo-Json -Compress
        $skippedResponse = Invoke-SmokeRequest -Path "$apiBase/warmup" -Method POST -Authenticated -Body $body
        Assert-Status $skippedResponse 200 "Unknown $provider account handled"
        $skipped = $skippedResponse.Body | ConvertFrom-Json
        if ($skipped.status -ne 'skipped' -or $skipped.provider -ne $provider -or
            @($skipped.PSObject.Properties.Name) -contains 'cooldown_until') {
            throw "The unknown $provider account was not safely skipped without a cooldown."
        }
        $checks.Add("Unknown $provider account skipped without a model request")
    }
    $after = Invoke-SmokeRequest -Path "$apiBase/state" -Authenticated
    Assert-Status $after 200 'State after rejected warmups'
    $recentResults = @(($after.Body | ConvertFrom-Json).results)
    if ($recentResults.Count -ne 2 -or @($recentResults | Where-Object { $_.status -ne 'skipped' }).Count -ne 0) {
        throw 'The result history does not contain exactly the two skipped requests.'
    }

    if ($StopHelper) {
        $currentHelperProcesses = @(Get-HelperProcessIDs -Executable $helperPath)
        if ($currentHelperProcesses.Count -ne 1 -or $currentHelperProcesses[0] -ne $helperProcessID) {
            throw 'The smoke helper process changed before the crash test.'
        }
        Stop-Process -Id $helperProcessID -Force
        $deadline = [DateTime]::UtcNow.AddSeconds(5)
        do {
            $failedState = Invoke-SmokeRequest -Path "$apiBase/state" -Authenticated
            if ($failedState.Status -ge 500 -and $failedState.Status -le 599) { break }
            Start-Sleep -Milliseconds 100
        } while ([DateTime]::UtcNow -lt $deadline)
        if ($failedState.Status -lt 500 -or $failedState.Status -gt 599) {
            throw 'The plugin did not fail closed after its helper exited.'
        }
        $checks.Add('Helper death fails pending plugin RPC without crashing the core')
        Assert-Status (Invoke-SmokeRequest -Path '/keep-alive' -Authenticated) 200 'The core remains responsive after helper death'
    }

    # -password enables a 10-second keep-alive idle timeout and graceful shutdown.
    # Stop sending heartbeats and allow the host to invoke plugin shutdown itself.
    if (-not $process.WaitForExit(20000)) { throw 'The core did not shut down after the keep-alive timeout.' }
    if ($process.ExitCode -ne 0) { throw "The core exited with code $($process.ExitCode)." }
    $checks.Add('Graceful native host shutdown')
    if ($helperPath) {
        if (@(Get-HelperProcessIDs -Executable $helperPath).Count -ne 0 -or
            @(Get-ChildItem -LiteralPath $temporaryDirectory -Directory -Filter 'cpa-account-warmup.*').Count -ne 0) {
            throw 'The Intel Mac helper process or its private directory survived shutdown.'
        }
        $checks.Add('Intel Mac helper process exits and its private files are removed')
    }
    $localStillRunning = $false
    try {
        $null = Invoke-SmokeRequest @localArguments -Path '/api/state' -Token $sessionToken
        $localStillRunning = $true
    }
    catch [Net.Http.HttpRequestException] { }
    if ($localStillRunning) { throw 'The local interface remained reachable after the core exited.' }
    $checks.Add('Local interface listener stops with the native host')
    $result = [ordered]@{ passed = $true; checks = $checks.ToArray(); directory = $runDirectory }
    [IO.File]::WriteAllText((Join-Path $runDirectory 'smoke-results.json'), ($result | ConvertTo-Json -Depth 4))
    Write-Host "Passed $($checks.Count) native smoke checks. Logs: $runDirectory"
}
finally {
    if ($outputTask) {
        if (-not $process.HasExited -and -not $process.WaitForExit(15000)) {
            # Kill only the child started by this test, never an existing core.
            $process.Kill($true)
            $process.WaitForExit()
        }
        [IO.File]::WriteAllText((Join-Path $runDirectory 'stdout.log'), $outputTask.GetAwaiter().GetResult())
        [IO.File]::WriteAllText((Join-Path $runDirectory 'stderr.log'), $errorTask.GetAwaiter().GetResult())
    }
    $process.Dispose()
    $client.Dispose()
}
