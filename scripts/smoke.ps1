#requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$CorePath,
    [string]$Compiler,
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

$buildArguments = @{ OutputDirectory = (Join-Path $runDirectory 'build') }
if ($Compiler) { $buildArguments.Compiler = $Compiler }
& (Join-Path $PSScriptRoot 'build.ps1') @buildArguments
$libraryFiles = @(Get-ChildItem -LiteralPath $buildArguments.OutputDirectory -Recurse -File |
    Where-Object { $_.Name -match '^cliproxy-account-warmup\.(dll|so|dylib)$' })
if ($libraryFiles.Count -ne 1) { throw 'The build did not produce exactly one plugin library.' }
Copy-Item -LiteralPath $libraryFiles[0].FullName -Destination $pluginDirectory

$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
$listener.Start()
try { $port = $listener.LocalEndpoint.Port }
finally { $listener.Stop() }
$baseURL = "http://127.0.0.1:$port"
$managementKey = [Guid]::NewGuid().ToString('N') + [Guid]::NewGuid().ToString('N')
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
  api-keys: []
requests:
  proxy-url: '$baseURL'
oauth:
  auth-dir: $($authDirectory | ConvertTo-Json -Compress)
plugins:
  enabled: true
  dir: $($pluginDirectory | ConvertTo-Json -Compress)
  configs:
    cliproxy-account-warmup:
      enabled: true
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
foreach ($variableName in @('USERPROFILE', 'HOME', 'APPDATA', 'LOCALAPPDATA')) {
    $startInfo.Environment[$variableName] = $runDirectory
}
$startInfo.Environment['TEMP'] = $temporaryDirectory
$startInfo.Environment['TMP'] = $temporaryDirectory
# The core also checks unrelated client versions in the background. Route those
# requests back into this isolated listener, which does not implement a proxy.
foreach ($variableName in @('HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY')) {
    $startInfo.Environment[$variableName] = $baseURL
}
$startInfo.Environment['NO_PROXY'] = '127.0.0.1,localhost,::1'

$handler = [Net.Http.HttpClientHandler]::new()
$handler.UseProxy = $false
$client = [Net.Http.HttpClient]::new($handler)
$client.Timeout = [TimeSpan]::FromSeconds(3)
$process = [Diagnostics.Process]::new()
$process.StartInfo = $startInfo
$outputTask = $null
$errorTask = $null
$checks = [Collections.Generic.List[string]]::new()

function Invoke-SmokeRequest {
    param([string]$Path, [string]$Method = 'GET', [switch]$Authenticated, [string]$Body)
    $request = [Net.Http.HttpRequestMessage]::new([Net.Http.HttpMethod]::new($Method), "$baseURL$Path")
    try {
        if ($Authenticated) {
            $request.Headers.Authorization = [Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $managementKey)
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
    $registered = @(($listing.Body | ConvertFrom-Json).plugins | Where-Object { $_.id -eq 'cliproxy-account-warmup' })
    if ($registered.Count -ne 1 -or -not $registered[0].registered -or -not $registered[0].effective_enabled) {
        throw 'The plugin was not registered and enabled by the native host.'
    }
    if ($registered[0].metadata.name -ne 'Account Warmup' -or @($registered[0].menus).Count -ne 1) {
        throw 'The registered plugin metadata or menu is incorrect.'
    }
    $checks.Add('Native ABI registration and metadata')

    $resourceBase = '/v0/resource/plugins/cliproxy-account-warmup'
    foreach ($asset in @(
        @{ Route = 'status'; File = 'index.html'; Type = 'text/html' },
        @{ Route = 'app.js'; File = 'app.js'; Type = 'text/javascript' },
        @{ Route = 'styles.css'; File = 'styles.css'; Type = 'text/css' }
    )) {
        $response = Invoke-SmokeRequest -Path "$resourceBase/$($asset.Route)"
        Assert-Status $response 200 "Public $($asset.File) resource"
        $source = [IO.File]::ReadAllText((Join-Path $repositoryRoot "internal/plugin/web/$($asset.File)"))
        if ($response.Body -cne $source -or -not $response.ContentType.StartsWith($asset.Type)) {
            throw "The host altered $($asset.File) or served it with an incorrect content type."
        }
        $checks.Add("Unescaped $($asset.File) content")
    }

    $apiBase = '/v0/management/plugins/cliproxy-account-warmup'
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

    # -password enables a 10-second keep-alive idle timeout and graceful shutdown.
    # Stop sending heartbeats and allow the host to invoke plugin shutdown itself.
    if (-not $process.WaitForExit(20000)) { throw 'The core did not shut down after the keep-alive timeout.' }
    if ($process.ExitCode -ne 0) { throw "The core exited with code $($process.ExitCode)." }
    $checks.Add('Graceful native host shutdown')
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
