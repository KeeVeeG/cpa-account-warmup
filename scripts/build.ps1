[CmdletBinding()]
param(
    [string]$OutputDirectory = (Join-Path $PSScriptRoot '..\dist'),
    [string]$Compiler,
    [string]$RepositoryURL = $env:PLUGIN_REPOSITORY_URL
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repositoryRoot = Split-Path -Parent $PSScriptRoot
$versionSource = Get-Content -LiteralPath (Join-Path $repositoryRoot 'internal\plugin\server.go') -Raw
$versionMatch = [regex]::Match($versionSource, '(?m)^const Version = "([0-9]+\.[0-9]+\.[0-9]+)"\r?$')
if (-not $versionMatch.Success) {
    throw 'Could not read the plugin version.'
}
$version = $versionMatch.Groups[1].Value
if ($env:PLUGIN_RELEASE_TAG -and $env:PLUGIN_RELEASE_TAG -cne "v$version") {
    throw "Release tag must match the plugin version: v$version."
}
if ($RepositoryURL -and $RepositoryURL -cnotmatch '^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') {
    throw 'RepositoryURL must be https://github.com/owner/repository.'
}
$previousCGO = [Environment]::GetEnvironmentVariable('CGO_ENABLED', 'Process')
$previousCC = [Environment]::GetEnvironmentVariable('CC', 'Process')

function Invoke-Go {
    param([string[]]$Arguments)
    & go @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "go $($Arguments -join ' ') failed with exit code $LASTEXITCODE."
    }
}

try {
    $null = Get-Command go -ErrorAction Stop
    if ($Compiler) {
        $compilerCommand = Get-Command $Compiler -CommandType Application -ErrorAction Stop
        # Go parses CC as a command line, so executable paths with spaces need quotes.
        $env:CC = '"' + $compilerCommand.Source + '"'
    }
    $env:CGO_ENABLED = '1'
    $outputPath = [IO.Path]::GetFullPath($OutputDirectory)
    Push-Location -LiteralPath $repositoryRoot
    try {
        $targetOS = (Invoke-Go -Arguments @('env', 'GOOS')).Trim()
        $targetArch = (Invoke-Go -Arguments @('env', 'GOARCH')).Trim()
        $extension = switch ($targetOS) {
            'windows' { 'dll' }
            'linux' { 'so' }
            'darwin' { 'dylib' }
            default { throw "Unsupported target operating system: $targetOS" }
        }
        $platformDirectory = Join-Path $outputPath "$targetOS-$targetArch"
        $null = New-Item -ItemType Directory -Path $platformDirectory -Force
        $libraryName = "cpa-account-warmup.$extension"
        $libraryPath = Join-Path $platformDirectory $libraryName
        $buildArguments = @('build', '-trimpath', '-buildmode=c-shared')
        if ($RepositoryURL) {
            $buildArguments += @('-ldflags', "-X=cpa-account-warmup/internal/plugin.RepositoryURL=$RepositoryURL")
        }
        Invoke-Go -Arguments ($buildArguments + @('-o', $libraryPath, './cmd/cpa-account-warmup'))

        $libraryHash = (Get-FileHash -LiteralPath $libraryPath -Algorithm SHA256).Hash.ToLowerInvariant()
        $packageChecksums = Join-Path $platformDirectory 'checksums.txt'
        [IO.File]::WriteAllText($packageChecksums, "$libraryHash  $libraryName`n", [Text.Encoding]::ASCII)
        $archiveFiles = @($libraryPath, $packageChecksums)
        foreach ($document in @('README.md', 'LICENSE')) {
            $documentPath = Join-Path $repositoryRoot $document
            if (Test-Path -LiteralPath $documentPath -PathType Leaf) {
                $archiveFiles += $documentPath
            }
        }
        $archivePath = Join-Path $outputPath "cpa-account-warmup_${version}_${targetOS}_${targetArch}.zip"
        Compress-Archive -LiteralPath $archiveFiles -DestinationPath $archivePath -CompressionLevel Optimal -Force

        $archiveChecksumLines = @(Get-ChildItem -LiteralPath $outputPath -Filter "cpa-account-warmup_${version}_*.zip" -File |
            Sort-Object Name | ForEach-Object {
                $hash = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
                "$hash  $($_.Name)"
            })
        [IO.File]::WriteAllText((Join-Path $outputPath 'checksums.txt'), ($archiveChecksumLines -join "`n") + "`n", [Text.Encoding]::ASCII)
        Write-Host "Built $libraryPath"
        Write-Host "Packaged $archivePath"
    }
    finally {
        Pop-Location
    }
}
finally {
    if ($null -eq $previousCGO) {
        Remove-Item -LiteralPath Env:CGO_ENABLED -ErrorAction SilentlyContinue
    }
    else {
        $env:CGO_ENABLED = $previousCGO
    }
    if ($null -eq $previousCC) {
        Remove-Item -LiteralPath Env:CC -ErrorAction SilentlyContinue
    }
    else {
        $env:CC = $previousCC
    }
}
