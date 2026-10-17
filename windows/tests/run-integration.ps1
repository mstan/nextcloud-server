<#
.SYNOPSIS
  Run the Windows integration suite (windows/tests, Go) against a fresh
  instance of an assembled ncwin tree, installed the way the MSI does it.
.DESCRIPTION
  configure (with the installer's marker-dot paths) -> setup -> `ncwin run`
  in the foreground -> go test -> integrity check and selftest -> shutdown.
  The instance lives in a throwaway state directory; nothing is installed as
  a service. Exits with the test suite's status.
.EXAMPLE
  powershell -File windows\tests\run-integration.ps1 -Build F:\Projects\nextcloud-build\out\ncwin -Db pgsql
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$Build,
    [ValidateSet('sqlite', 'pgsql', 'mysql')] [string]$Db = 'pgsql',
    [int]$HttpPort = 38080,
    [int]$HttpsPort = 38443,
    [string]$StateRoot = (Join-Path $env:TEMP 'ncwin-integration'),
    # Extra `go test` arguments, e.g. '-run','TestDeleteThenRecreate'.
    [string[]]$GoTestArgs = @()
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

foreach ($p in $HttpPort, $HttpsPort) {
    if (Get-NetTCPConnection -State Listen -LocalPort $p -ErrorAction SilentlyContinue) { throw "port $p is in use; pass -HttpPort/-HttpsPort" }
}
$exe = Join-Path $Build 'ncwin.exe'
if (-not (Test-Path $exe)) { throw "$Build is not an assembled ncwin tree" }
$t = Join-Path $StateRoot $Db
if (Test-Path $t) { Remove-Item -Recurse -Force $t }
New-Item -ItemType Directory -Force $t | Out-Null
$lanHost = 'ncwin-integration'

& $exe configure --config "$t\state\ncwin.toml" --install-dir "$Build\." --state-dir "$t\state" --data-dir "$t\data." `
    --db $Db --http-port $HttpPort --https-port $HttpsPort "--hostnames=$lanHost" '--public-domain=' '--acme-email='
if ($LASTEXITCODE) { throw 'configure failed' }
# Every ncwin command below (and the tests' own `ncwin occ`) addresses this
# instance, never an installed service on the same machine.
$env:NCWIN_CONFIG = "$t\state\ncwin.toml"
$env:NCWIN_ADMIN_PASS = 'Integration-Admin-Pass-1'
& $exe setup --config "$t\state\ncwin.toml" --no-acl --if-not-installed *> "$t\setup-out.txt"
if ($LASTEXITCODE) { Get-Content "$t\state\log\setup.log" -Tail 10; throw 'setup failed' }

$p = Start-Process $exe -ArgumentList @('run', '--config', "$t\state\ncwin.toml") -PassThru -NoNewWindow `
    -RedirectStandardOutput "$t\run.out" -RedirectStandardError "$t\run.err"
try {
    # Before the control pipe exists `ncwin events` fails; that is not an error here.
    function WebReady { $ErrorActionPreference = 'Continue'; ((& $exe events --kind proc.ready 2>&1 | Out-String) -match ' web ') }
    $deadline = (Get-Date).AddMinutes(10)
    while (-not (WebReady)) {
        if ($p.HasExited) { throw "ncwin run exited ($($p.ExitCode)); see $t\run.err" }
        if ((Get-Date) -gt $deadline) { throw 'web never became ready' }
        Start-Sleep 2
    }
    $env:NCWIN_URL = "https://localhost:$HttpsPort"; $env:NCWIN_USER = 'admin'; $env:NCWIN_PASS = $env:NCWIN_ADMIN_PASS
    $env:NCWIN_DATA = "$t\data"; $env:NCWIN_EXE = $exe; $env:NCWIN_LAN_HOST = $lanHost
    Push-Location $PSScriptRoot
    try {
        & go test ./... -count=1 -v -timeout 40m @GoTestArgs
        $code = $LASTEXITCODE
    } finally { Pop-Location }
    & $exe occ integrity:check-core
    if ($LASTEXITCODE) { Write-Warning 'integrity check failed'; $code = 1 }
    & $exe occ windows:selftest
    if ($LASTEXITCODE) { Write-Warning 'selftest failed'; $code = 1 }
} finally {
    $ErrorActionPreference = 'Continue'
    & $exe shutdown 2>&1 | Out-Null
    if (-not $p.WaitForExit(900000)) { Stop-Process -Id $p.Id -Force }
}
"integration suite exit code $code (events journal: $t\state\log\events.jsonl)"
exit $code
