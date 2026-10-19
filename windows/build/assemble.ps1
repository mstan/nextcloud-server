<#
.SYNOPSIS
  Assemble an ncwin install tree: official Nextcloud release + this fork's
  core patches + the Windows runtime (Apache/PHP, PECL, optional DBs) +
  ncwin.exe.

.DESCRIPTION
  Output layout (= paths.install_dir on the target machine):
    <Out>\nextcloud\   patched Nextcloud web root (+ apps\windows_integration)
    <Out>\apache\      Apache HTTP Server
    <Out>\php\         PHP (TS) + extensions
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
    # Default: nextcloud-build next to the repository.
    [string]$WorkDir,
    [string]$Out,
    [string]$PatchRef = 'HEAD',
    [switch]$WithMariaDB,
    [switch]$WithPostgres,
    [switch]$Clean,
    # Directory holding core.key/.crt and windows_integration.key/.crt, issued by
    # resources/codesigning/downstream-root.crt. CI passes these from secrets.
    [string]$SigningDir,
    # Developer builds only: keep upstream's signature (the integrity check will flag patched files).
    [switch]$NoSign,
    # Release tag stamped into ncwin.exe (vX.Y.Z-win.N); default: git describe.
    # Automatic updates compare it with the latest release, so releases need it.
    [string]$Version
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$git = Join-Path $env:ProgramFiles 'Git\mingw64\bin\git.exe'
if (-not (Test-Path -LiteralPath $git)) { $git = (Get-Command git -CommandType Application).Source }
$env:PATH = (Split-Path -Parent $git) + [IO.Path]::PathSeparator + $env:PATH
# (Computed here: Windows PowerShell 5.1 leaves $PSScriptRoot empty in param() defaults.)
if (-not $WorkDir) { $WorkDir = Join-Path (Split-Path -Parent $repo) 'nextcloud-build' }
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
    # Download hosts rate-limit shared CI addresses (HTTP 429) and have
    # transient outages; back off and retry, honouring Retry-After.
    for ($attempt = 1; ; $attempt++) {
        try {
            (New-Object Net.WebClient).DownloadFile($Url, $tmp)
            break
        } catch {
            $resp = $_.Exception.InnerException.Response
            $code = if ($resp) { [int]$resp.StatusCode } else { 0 }
            if ($attempt -ge 6 -or ($code -ne 0 -and $code -ne 429 -and $code -lt 500)) { throw }
            $wait = 15 * [math]::Pow(2, $attempt - 1)
            if ($resp -and $resp.Headers['Retry-After'] -match '^\d+$') { $wait = [math]::Max($wait, [int]$resp.Headers['Retry-After']) }
            Write-Warning "download $Url failed ($($_.Exception.Message)); retry $attempt in $wait s"
            Start-Sleep -Seconds $wait
        }
    }
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
& $git -C $repo rev-parse --verify --quiet "refs/tags/$tag" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "upstream tag $tag not found in $repo; run: git fetch upstream --tags" }
# --output, not a pipe: PowerShell 5.1 re-encodes native-command pipeline text.
# Release zips contain no tests or build tooling; never ship those parts of the patch set.
& $git -C $repo diff --ignore-submodules=all --binary --full-index "--output=$patch" $tag $PatchRef -- . ':(exclude)windows' ':(exclude).github' ':(exclude)tests' ':(exclude)build' ':(exclude,glob)apps/*/tests/**'
if ($LASTEXITCODE -ne 0) { throw 'git diff failed' }
$patchSize = (Get-Item $patch).Length
if ($patchSize -gt 0) {
    Write-Host "apply fork patch ($patchSize bytes, $tag..$PatchRef)"
    # Release ZIPs do not preserve the executable bit of occ.
    $savedPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        & $git -c core.filemode=false -C (Join-Path $stage 'nextcloud') apply --whitespace=nowarn $patch
        $patchExit = $LASTEXITCODE
    } finally { $ErrorActionPreference = $savedPreference }
    if ($patchExit -ne 0) { throw "fork patch does not apply cleanly to the $($nc.version) release zip" }
} else {
    Write-Host "no fork patches between $tag and $PatchRef"
}
Copy-Item -Recurse (Join-Path $repo 'windows\app\windows_integration') (Join-Path $stage 'nextcloud\apps\windows_integration')

# 2. Apache and PHP runtime.
$httpd = $lock.apache
Write-Host "extract Apache $($httpd.version)"
$httpdTmp = Join-Path $stage 'apache-unpack'
Expand-Zip (Get-Pinned $httpd.url $httpd.sha256) $httpdTmp
Move-Item -LiteralPath (Join-Path $httpdTmp 'Apache24') -Destination (Join-Path $stage 'apache')
$httpdResolved = [IO.Path]::GetFullPath($httpdTmp)
if (-not $httpdResolved.StartsWith([IO.Path]::GetFullPath($stage) + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { throw 'Apache unpack path is outside staging' }
Remove-Item -LiteralPath $httpdResolved -Recurse -Force
$phpRuntime = $lock.php
Write-Host "extract PHP $($phpRuntime.version)"
Expand-Zip (Get-Pinned $phpRuntime.url $phpRuntime.sha256) (Join-Path $stage 'php')
if (-not (Test-Path -LiteralPath (Join-Path $stage 'php\php8apache2_4.dll'))) { throw 'PHP runtime lacks the Apache module (requires thread-safe PHP)' }
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
    # Optional "strip" list (wildcards allowed): bundled extras ncwin never runs
    # (pgAdmin, StackBuilder, docs, C headers, link libraries, debug symbols).
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
    $ver = if ($Version) { $Version } else { (& $git -C $repo describe --tags --always $PatchRef) }
    & go build -trimpath -ldflags "-s -w -X main.version=$ver" -o (Join-Path $stage 'ncwin.exe') ./cmd/ncwin
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
} finally { Pop-Location }

Copy-Item (Join-Path $repo 'windows\supervisor\ncwin.toml.example') $stage

Move-Item $stage $Out
Write-Host "assembled $Out"
