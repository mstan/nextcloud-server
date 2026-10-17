<#
.SYNOPSIS
  Build, check and (with -Publish) release ncwin by hand, with the same gates
  CI runs.
.DESCRIPTION
  1. Preflight: clean work tree on a win/NN branch; the release tag
     vX.Y.Z-win.N is new or already points at HEAD.
  2. Fast gates: go vet + go test (supervisor), go vet (integration tests),
     the Windows lint, php -l of the patch set and the windows_integration app.
  3. assemble.ps1 -Clean -WithMariaDB -WithPostgres, signed with
     <WorkDir>\signing, stamped with the release tag.
  4. Upstream PHPUnit suites (tests/phpunit-windows.sh + junit_check.py)
     with the assembled PHP, against a test instance in the repo (unless
     -SkipUpstreamTests).
  5. Integration suite (tests/run-integration.ps1) on the assembled tree with
     bundled PostgreSQL.
  6. MSI (build-msi.ps1), release zip and .sha256 files in
     <WorkDir>\release\<tag>.
  7. -Publish: tag, push win/NN (and master, which mirrors it) and the tag,
     and create the GitHub release (marked latest), which installed servers'
     daily update task then picks up.
.EXAMPLE
  powershell -File windows\build\release.ps1 -WinBuild 4            # build and check only
  powershell -File windows\build\release.ps1 -WinBuild 4 -Publish   # and release
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [ValidateRange(1, 99)] [int]$WinBuild,
    # Default: nextcloud-build next to the repository.
    [string]$WorkDir,
    [switch]$Publish,
    [switch]$SkipUpstreamTests,
    [int]$HttpPort = 38080,
    [int]$HttpsPort = 38443
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if (-not $WorkDir) { $WorkDir = Join-Path (Split-Path -Parent $repo) 'nextcloud-build' }
$lock = Get-Content -Raw (Join-Path $PSScriptRoot 'deps.lock.json') | ConvertFrom-Json
$tag = "v$($lock.nextcloud.version)-win.$WinBuild"
$out = Join-Path $WorkDir 'out\ncwin'
$rel = Join-Path $WorkDir "release\$tag"
function Step([string]$name) { Write-Host "`n==== $name" -ForegroundColor Cyan }
function Native([string]$what) { if ($LASTEXITCODE -ne 0) { throw "$what failed (exit $LASTEXITCODE)" } }
function RepoGit { & git.exe -C $repo @args; Native "git $($args -join ' ')" }
# Native stderr redirected under ErrorActionPreference=Stop is fatal in Windows
# PowerShell; probes whose failure is an answer run with Continue.
function Probe([scriptblock]$b) { $ErrorActionPreference = 'Continue'; & $b }
# The Windows launcher, when present: the first `python` on PATH can be an
# MSYS build that mangles Windows paths.
function Python { if (Get-Command py -ErrorAction SilentlyContinue) { & py -3 @args } else { & python @args } }

Step "preflight $tag"
$branch = (RepoGit rev-parse --abbrev-ref HEAD)
if ($branch -notmatch '^win/\d+$') { throw "check out a win/NN branch (on $branch)" }
if (RepoGit status --porcelain) { throw 'the work tree has uncommitted changes' }
$head = RepoGit rev-parse HEAD
$tagged = Probe { & git.exe -C $repo rev-parse -q --verify "refs/tags/$tag^{commit}" 2>$null }
if ($tagged -and $tagged -ne $head) { throw "$tag already exists at $tagged, not HEAD $head; use another -WinBuild" }
if ($Publish) {
    if (-not (Probe { & gh auth status 2>$null | Out-Null; $LASTEXITCODE -eq 0 })) { throw 'gh is not logged in' }
    if (Probe { & gh release view $tag -R mstan/nextcloud-server 2>$null | Out-Null; $LASTEXITCODE -eq 0 }) { throw "release $tag already exists on GitHub" }
}

Step 'go vet + go test'
Push-Location (Join-Path $repo 'windows\supervisor')
try { & go vet ./...; Native 'go vet'; & go test ./... -count=1; Native 'go test' } finally { Pop-Location }
Push-Location (Join-Path $repo 'windows\tests')
try { & go vet ./...; Native 'go vet (integration tests)' } finally { Pop-Location }

Step 'Windows lint'
Python (Join-Path $repo 'windows\build\lint_windows.py'); Native 'lint_windows.py'

Step 'assemble (signed)'
& powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'assemble.ps1') `
    -WorkDir $WorkDir -Clean -WithMariaDB -WithPostgres -Version $tag
Native 'assemble.ps1'
$php = Join-Path $out 'php\php.exe'

Step 'php -l (patch set and windows_integration)'
$files = @(& git.exe -C $repo diff --name-only --diff-filter=AM "v$($lock.nextcloud.version)" HEAD -- '*.php' 'occ') |
    ForEach-Object { Join-Path $repo $_ }
$files += Get-ChildItem (Join-Path $repo 'windows\app') -Recurse -Filter *.php | ForEach-Object FullName
$bad = 0
foreach ($f in $files) { & $php -n -l $f | Out-Null; if ($LASTEXITCODE -ne 0) { & $php -n -l $f; $bad++ } }
if ($bad) { throw "$bad file(s) fail php -l" }
"linted $($files.Count) files"

if (-not $SkipUpstreamTests) {
    Step 'upstream PHPUnit suites'
    # The suites need the repo itself installed as an instance, like CI's
    # "Fresh test instance": an existing install is reused.
    $ini = Join-Path $WorkDir 'phpunit\php.ini'
    New-Item -ItemType Directory -Force (Split-Path $ini) | Out-Null
    $ext = (Join-Path (Split-Path $php) 'ext') -replace '\\', '/'
    @('[PHP]', "extension_dir = `"$ext`"", 'memory_limit = 4G',
        'extension = curl', 'extension = fileinfo', 'extension = gd', 'extension = gmp', 'extension = intl',
        'extension = mbstring', 'extension = exif', 'extension = openssl', 'extension = sodium', 'extension = zip',
        'extension = pdo_sqlite', 'extension = apcu', 'apc.enable_cli = 1', 'opcache.enable_cli = 0') |
        Set-Content -Encoding ASCII $ini
    $env:PHPRC = Split-Path $ini
    $env:OPENSSL_CONF = Join-Path (Split-Path $php) 'extras\ssl\openssl.cnf'
    Push-Location $repo
    try {
        if (-not (Test-Path 'vendor-bin\phpunit\vendor\bin\phpunit')) {
            $composer = Join-Path $WorkDir 'phpunit\composer.phar'
            if (-not (Test-Path $composer)) { Invoke-WebRequest https://getcomposer.org/download/2.8.12/composer.phar -OutFile $composer }
            & $php $composer install -d vendor-bin/phpunit --no-interaction --no-progress; Native 'composer install'
        }
        $cfg = Join-Path $repo 'config\config.php'
        if (-not ((Test-Path $cfg) -and (Select-String -Quiet -Path $cfg -Pattern "'installed' => true"))) {
            $data = (Join-Path $WorkDir 'phpunit\ncdata') -replace '\\', '/'
            & $php occ maintenance:install --database=sqlite --admin-user=admin --admin-pass=admin "--data-dir=$data"; Native 'test instance install'
            & $php tests/enable_all.php; Native 'enable_all'
        }
        $junit = Join-Path $WorkDir 'phpunit\junit'
        if (Test-Path $junit) { Remove-Item -Recurse -Force $junit }
        New-Item -ItemType Directory $junit | Out-Null
        $sh = Join-Path $env:ProgramFiles 'Git\bin\sh.exe'
        & $sh windows/tests/phpunit-windows.sh ($php -replace '\\', '/') ($junit -replace '\\', '/')
        Python windows/tests/junit_check.py $junit; Native 'upstream PHPUnit (junit_check)'
    } finally {
        Pop-Location
        Remove-Item Env:PHPRC, Env:OPENSSL_CONF -ErrorAction SilentlyContinue
    }
}

Step 'integration suite (bundled PostgreSQL)'
& powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $repo 'windows\tests\run-integration.ps1') `
    -Build $out -Database pgsql -HttpPort $HttpPort -HttpsPort $HttpsPort -StateRoot (Join-Path $WorkDir 'integration')
Native 'integration suite'

Step 'MSI, zip, checksums'
if (Test-Path $rel) { Remove-Item -Recurse -Force $rel }
New-Item -ItemType Directory -Force $rel | Out-Null
$base = "ncwin-$($tag.TrimStart('v'))"
& powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'build-msi.ps1') `
    -Payload $out -WinBuild $WinBuild -Out (Join-Path $rel "$base.msi")
Native 'build-msi.ps1'
# Entry names use '/' (the zip format's separator; .NET Framework's
# CreateFromDirectory writes '\'), and hidden files such as .htaccess are
# included (Compress-Archive skips them).
Add-Type -AssemblyName System.IO.Compression, System.IO.Compression.FileSystem
$zipPath = Join-Path $rel "$base.zip"
$all = Get-ChildItem -Recurse -Force -File $out
$zip = [IO.Compression.ZipFile]::Open($zipPath, [IO.Compression.ZipArchiveMode]::Create)
try {
    foreach ($f in $all) {
        $name = $f.FullName.Substring($out.Length + 1).Replace('\', '/')
        [void][IO.Compression.ZipFileExtensions]::CreateEntryFromFile($zip, $f.FullName, $name, [IO.Compression.CompressionLevel]::Optimal)
    }
} finally { $zip.Dispose() }
$check = [IO.Compression.ZipFile]::OpenRead($zipPath)
try {
    $names = $check.Entries | ForEach-Object FullName
    if ($names.Count -ne $all.Count -or ($names -match '\\') -or ($names -notcontains 'nextcloud/version.php') -or ($names -notcontains 'nextcloud/.htaccess')) {
        throw "release zip is incomplete or malformed ($($names.Count) of $($all.Count) entries)"
    }
} finally { $check.Dispose() }
foreach ($f in "$base.msi", "$base.zip") {
    $h = (Get-FileHash -Algorithm SHA256 (Join-Path $rel $f)).Hash.ToLower()
    Set-Content -Encoding ASCII -Path (Join-Path $rel "$f.sha256") -Value "$h  $f"
}
Get-ChildItem $rel | Format-Table Name, Length -AutoSize

if (-not $Publish) {
    "built and checked $tag in $rel (not published; rerun with -Publish)"
    exit 0
}

Step "publish $tag"
$prev = Probe { & git.exe -C $repo describe --tags --abbrev=0 --match 'v*-win.*' "$head^" 2>$null }
if (-not $tagged) { RepoGit tag $tag $head }
RepoGit push origin $branch
RepoGit push --force origin "${branch}:master"
RepoGit push origin $tag
$notes = Join-Path $rel 'notes.md'
$log = if ($prev) { & git.exe -C $repo log --format='- %s' "$prev..$tag" } else { @() }
@("Nextcloud $($lock.nextcloud.version) for Windows.", '',
    'Install or upgrade: run the .msi (elevated). Installed servers update themselves with their daily update task.', '',
    "Changes since $(if ($prev) { $prev } else { 'the start' }):") + $log | Set-Content -Encoding UTF8 $notes
& gh release create $tag (Get-ChildItem $rel -Exclude notes.md, *.wixpdb | ForEach-Object FullName) -R mstan/nextcloud-server `
    --verify-tag --latest --title "ncwin $($tag.TrimStart('v'))" --notes-file $notes
Native 'gh release create'
"published $tag"
