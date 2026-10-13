#requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [ValidateSet('windows', 'linux', 'darwin')]
    [string]$OperatingSystem,
    [Parameter(Mandatory)]
    [ValidateSet('amd64', 'arm64')]
    [string]$Architecture,
    [Parameter(Mandatory)]
    [string]$Destination
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$version = '8.0.27'
# Pinned to the official v8.0.27 release and checked against its checksums.txt.
$checksums = @{
    'windows-amd64' = 'ac6867818696aefe299c8bac1730f831cbb127f0854acca1b1aebb0110a5dc53'
    'linux-amd64' = '82d38c89a62a2787f91e244595e0e76e52d6caaf2aa3d1a19b617e554c72b449'
    'linux-arm64' = '5ef174b13320d2061dc33cb0ee3db31dbbf2cd947ab2e02edd5b16ac035e8b2a'
    'darwin-amd64' = '8ca6317fe7560a0452d2835f296a6360de8bc88099670c68635fd1cf4132c8d5'
    'darwin-arm64' = 'd9bf58db6e07d0f587a248abc4a1099178fbec1d659d874c961d8caf3495a09e'
}
$platform = "$OperatingSystem-$Architecture"
if (-not $checksums.ContainsKey($platform)) { throw "Unsupported core platform: $platform" }
$releaseArchitecture = if ($Architecture -eq 'arm64') { 'aarch64' } else { $Architecture }
$extension = if ($OperatingSystem -eq 'windows') { 'zip' } else { 'tar.gz' }
$archiveName = "CLIProxyAPI_${version}_${OperatingSystem}_${releaseArchitecture}.$extension"
$destinationDirectory = [IO.Path]::GetFullPath($Destination)
$null = New-Item -ItemType Directory -Path $destinationDirectory -Force
$archivePath = Join-Path $destinationDirectory $archiveName
$downloadURL = "https://github.com/router-for-me/CLIProxyAPI/releases/download/v$version/$archiveName"
Invoke-WebRequest -Uri $downloadURL -OutFile $archivePath
$actualChecksum = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actualChecksum -cne $checksums[$platform]) {
    throw "The SHA256 checksum does not match the pinned core release: $archiveName"
}
if ($OperatingSystem -eq 'windows') {
    Expand-Archive -LiteralPath $archivePath -DestinationPath $destinationDirectory -Force
}
else {
    & tar -xzf $archivePath -C $destinationDirectory
    if ($LASTEXITCODE -ne 0) { throw 'Could not extract the verified core archive.' }
}
$executableName = if ($OperatingSystem -eq 'windows') { 'cli-proxy-api.exe' } else { 'cli-proxy-api' }
$executablePath = Join-Path $destinationDirectory $executableName
if (-not (Test-Path -LiteralPath $executablePath -PathType Leaf)) {
    throw "The core archive does not contain $executableName."
}
Write-Output $executablePath
