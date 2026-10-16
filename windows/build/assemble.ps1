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
    [switch]$Clean
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
& git -C $repo diff --binary --full-index "--output=$patch" $tag $PatchRef -- . ':(exclude)windows' ':(exclude).github'
if ($LASTEXITCODE -ne 0) { throw 'git diff failed' }
$patchSize = (Get-Item $patch).Length
if ($patchSize -gt 0) {
    Write-Host "apply fork patch ($patchSize bytes, $tag..$PatchRef)"
    # Release zips omit some upstream paths (tests, build tooling); patches
    # touching only those are skipped rather than failing the build.
    & git -C (Join-Path $stage 'nextcloud') apply --whitespace=nowarn --exclude='tests/*' --exclude='build/*' $patch
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

Move-Item $stage $Out
Write-Host "assembled $Out"
