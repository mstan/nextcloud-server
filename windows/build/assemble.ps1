<#
.SYNOPSIS
  Assemble an ncwin install tree: official Nextcloud release + this fork's
  core patches + the Windows runtime (FrankenPHP/PHP, PECL, optional DBs) +
  ncwin.exe.

.DESCRIPTION
  Output layout (= paths.install_dir on the target machine):
    <Out>\nextcloud\   patched Nextcloud web root (+ apps\windows_integration)
    <Out>\php\         FrankenPHP + PHP (TS) + extensions
    <Out>\mariadb\     optional, -WithMariaDB
    <Out>\pgsql\       optional, -WithPostgres
    <Out>\ncwin.exe

  Every download is pinned in deps.lock.json and verified by SHA-256.
  The patch applied is `git diff v<version> <PatchRef>` over the upstream
  tree, excluding windows/ and .github/ (fork-only material).

.EXAMPLE
  powershell -File windows\build\assemble.ps1 -WorkDir F:\Projects\nextcloud-build
#>
[CmdletBinding()]
param(
    [string]$WorkDir = (Join-Path (Split-Path -Parent (Split-Path -Parent (Split-Path -Parent $PSScriptRoot))) 'nextcloud-build'),
    [string]$Out,
    [string]$PatchRef = 'HEAD',
    [switch]$WithMariaDB,
    [switch]$WithPostgres,
    [switch]$Clean,
    # Directory holding core.key/.crt and windows_integration.key/.crt, issued by
    # resources/codesigning/downstream-root.crt. CI passes these from secrets.
    [string]$SigningDir,
    # Developer builds only: keep upstream's signature (the integrity check will flag patched files).
    [switch]$NoSign
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$lock = Get-Content -Raw (Join-Path $PSScriptRoot 'deps.lock.json') | ConvertFrom-Json
$cache = Join-Path $WorkDir 'cache'
if (-not $Out) { $Out = Join-Path $WorkDir 'out\ncwin' }
New-Item -ItemType Directory -Force $cache | Out-Null

function Get-Pinned([string]$Url, [string]$Sha256) {
    $name = [IO.Path]::GetFileName(([Uri]$Url).AbsolutePath)
    $dest = Join-Path $cache $name
    if (Test-Path $dest) {
        if ((Get-FileHash -Algorithm SHA256 $dest).Hash -eq $Sha256.ToUpper()) { return $dest }
        Write-Warning "cached $name has wrong hash; re-downloading"
        Remove-Item $dest
    }
    Write-Host "download $Url"
    $tmp = "$dest.download"
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $wc = New-Object Net.WebClient
    $wc.DownloadFile($Url, $tmp)
    $actual = (Get-FileHash -Algorithm SHA256 $tmp).Hash
    if ($actual -ne $Sha256.ToUpper()) {
        Remove-Item $tmp
        throw "SHA-256 mismatch for $Url`n  expected $Sha256`n  actual   $actual"
    }
    Move-Item $tmp $dest
    return $dest
}

function Expand-Zip([string]$Zip, [string]$Dest) {
    New-Item -ItemType Directory -Force $Dest | Out-Null
    # bsdtar (built into Windows 10+) is far faster than Expand-Archive. Use the
    # System32 copy explicitly: a Git/MSYS tar.exe earlier on PATH cannot read zips.
    & (Join-Path $env:SystemRoot 'System32\tar.exe') -xf $Zip -C $Dest
    if ($LASTEXITCODE -ne 0) { throw "tar failed extracting $Zip" }
}

if ($Clean -and (Test-Path $Out)) { Remove-Item -Recurse -Force $Out }
if (Test-Path $Out) { throw "$Out exists; pass -Clean to rebuild it" }
$stage = "$Out.staging"
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Force $stage | Out-Null

# 1. Nextcloud release + fork patch set.
$nc = $lock.nextcloud
$ncZip = Get-Pinned $nc.url $nc.sha256
Write-Host "extract nextcloud $($nc.version)"
Expand-Zip $ncZip $stage   # zip contains a top-level nextcloud\ directory
$tag = "v$($nc.version)"
$patch = Join-Path $WorkDir "ncwin-$($nc.version).patch"
# No "^{commit}" peel syntax: MSYS2's git brace-expands its arguments.
& git -C $repo rev-parse --verify --quiet "refs/tags/$tag" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "upstream tag $tag not found in $repo; run: git fetch upstream --tags" }
# --output, not a pipe: PowerShell 5.1 re-encodes native-command pipeline text.
# Release zips contain no tests or build tooling; never ship those parts of the patch set.
& git -C $repo diff --binary --full-index "--output=$patch" $tag $PatchRef -- . ':(exclude)windows' ':(exclude).github' ':(exclude)tests' ':(exclude)build' ':(exclude,glob)apps/*/tests/**'
if ($LASTEXITCODE -ne 0) { throw 'git diff failed' }
$patchSize = (Get-Item $patch).Length
if ($patchSize -gt 0) {
    Write-Host "apply fork patch ($patchSize bytes, $tag..$PatchRef)"
    & git -C (Join-Path $stage 'nextcloud') apply --whitespace=nowarn $patch
    if ($LASTEXITCODE -ne 0) { throw "fork patch does not apply cleanly to the $($nc.version) release zip" }
} else {
    Write-Host "no fork patches between $tag and $PatchRef"
}
Copy-Item -Recurse (Join-Path $repo 'windows\app\windows_integration') (Join-Path $stage 'nextcloud\apps\windows_integration')

# 2. PHP runtime.
$fp = $lock.frankenphp
Write-Host "extract frankenphp $($fp.version) (PHP $($fp.php))"
Expand-Zip (Get-Pinned $fp.url $fp.sha256) (Join-Path $stage 'php')
foreach ($ext in $lock.pecl.PSObject.Properties) {
    $zip = Get-Pinned $ext.Value.url $ext.Value.sha256
    $tmp = Join-Path $WorkDir "pecl-$($ext.Name)"
    if (Test-Path $tmp) { Remove-Item -Recurse -Force $tmp }
    Expand-Zip $zip $tmp
    Copy-Item (Join-Path $tmp "php_$($ext.Name).dll") (Join-Path $stage 'php\ext')
    # Dependency DLLs (e.g. ImageMagick CORE_RL_*.dll) must sit beside php.exe.
    Get-ChildItem $tmp -Filter *.dll | Where-Object { $_.Name -notlike 'php_*' } | Copy-Item -Destination (Join-Path $stage 'php')
    Remove-Item -Recurse -Force $tmp
}

# 2b. Re-sign the patched core and windows_integration with the downstream key,
# so Nextcloud's code-integrity check keeps working (see Checker::loadDownstreamCertificate).
if ($NoSign) {
    Write-Warning 'skipping code signing (-NoSign): the integrity check will report modified core files'
} else {
    if (-not $SigningDir) { $SigningDir = Join-Path $WorkDir 'signing' }
    foreach ($f in 'core.key', 'core.crt', 'windows_integration.key', 'windows_integration.crt') {
        if (-not (Test-Path (Join-Path $SigningDir $f))) { throw "missing $f in $SigningDir (or pass -NoSign for a developer build)" }
    }
    $signTmp = Join-Path $WorkDir 'sign-tmp'
    if (Test-Path $signTmp) { Remove-Item -Recurse -Force $signTmp }
    New-Item -ItemType Directory -Force (Join-Path $signTmp 'config') | Out-Null
    $ext = (Join-Path $stage 'php\ext') -replace '\\', '/'
    Set-Content -Encoding ASCII -Path (Join-Path $signTmp 'php.ini') -Value @(
        '[PHP]', "extension_dir = `"$ext`"", 'memory_limit = 1G',
        'extension = openssl', 'extension = mbstring', 'extension = intl', 'extension = fileinfo',
        'extension = gd', 'extension = zip', 'extension = curl', 'extension = sodium', 'extension = pdo_sqlite')
    $env:PHPRC = $signTmp
    $env:NEXTCLOUD_CONFIG_DIR = (Join-Path $signTmp 'config') + '\'
    $env:OPENSSL_CONF = Join-Path $stage 'php\extras\ssl\openssl.cnf'
    $php = Join-Path $stage 'php\php.exe'
    $nc = Join-Path $stage 'nextcloud'
    Push-Location $nc
    try {
        Write-Host 'sign core'
        & $php -f occ -- --no-warnings integrity:sign-core "--privateKey=$(Join-Path $SigningDir 'core.key')" "--certificate=$(Join-Path $SigningDir 'core.crt')" "--path=$nc"
        if ($LASTEXITCODE -ne 0) { throw 'integrity:sign-core failed' }
        Write-Host 'sign windows_integration'
        & $php -f occ -- --no-warnings integrity:sign-app "--privateKey=$(Join-Path $SigningDir 'windows_integration.key')" "--certificate=$(Join-Path $SigningDir 'windows_integration.crt')" "--path=$(Join-Path $nc 'apps\windows_integration')"
        if ($LASTEXITCODE -ne 0) { throw 'integrity:sign-app failed' }
    } finally {
        Pop-Location
        Remove-Item Env:PHPRC, Env:NEXTCLOUD_CONFIG_DIR, Env:OPENSSL_CONF -ErrorAction SilentlyContinue
        Remove-Item -Recurse -Force $signTmp -ErrorAction SilentlyContinue
    }
}

# 3. Optional bundled databases.
foreach ($db in @(@{On = $WithMariaDB; Key = 'mariadb'}, @{On = $WithPostgres; Key = 'pgsql'})) {
    if (-not $db.On) { continue }
    $d = $lock.($db.Key)
    if (-not $d) { throw "deps.lock.json has no '$($db.Key)' entry" }
    $tmp = Join-Path $WorkDir "db-$($db.Key)"
    if (Test-Path $tmp) { Remove-Item -Recurse -Force $tmp }
    Expand-Zip (Get-Pinned $d.url $d.sha256) $tmp
    $top = Get-ChildItem $tmp -Directory | Select-Object -First 1
    Move-Item $top.FullName (Join-Path $stage $db.Key)
    # Optional "strip" list: bundled extras ncwin never runs (pgAdmin, StackBuilder, docs).
    if ($d.PSObject.Properties.Name -contains 'strip') {
        foreach ($s in $d.strip) {
            $p = Join-Path (Join-Path $stage $db.Key) $s
            if (Test-Path $p) { Remove-Item -Recurse -Force $p }
        }
    }
    Remove-Item -Recurse -Force $tmp
}

# 4. ncwin.exe
Write-Host 'build ncwin.exe'
Push-Location (Join-Path $repo 'windows\supervisor')
try {
    $ver = (& git -C $repo describe --tags --always $PatchRef)
    & go build -trimpath -ldflags "-s -w -X main.version=$ver" -o (Join-Path $stage 'ncwin.exe') ./cmd/ncwin
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
} finally { Pop-Location }

Copy-Item (Join-Path $repo 'windows\supervisor\ncwin.toml.example') $stage

Move-Item $stage $Out
Write-Host "assembled $Out"
