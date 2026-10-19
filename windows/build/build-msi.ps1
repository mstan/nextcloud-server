<#
.SYNOPSIS
  Build the ncwin MSI from an assembled install tree (see assemble.ps1).

.DESCRIPTION
  MSI versions compare only major.minor.build, and fork rebuilds of one
  Nextcloud release (vX.Y.Z-win.N) must still upgrade each other, so the
  MSI version is X.Y.(Z*100+N).

.EXAMPLE
  powershell -File windows\build\build-msi.ps1 -Payload F:\Projects\nextcloud-build\out\ncwin -WinBuild 1
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$Payload,
    [int]$WinBuild = 1,
    [string]$Out
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$lock = Get-Content -Raw (Join-Path $PSScriptRoot 'deps.lock.json') | ConvertFrom-Json
$v = [version]$lock.nextcloud.version
if ($WinBuild -lt 1 -or $WinBuild -gt 99) { throw 'WinBuild must be 1..99' }
$msiVersion = '{0}.{1}.{2}' -f $v.Major, $v.Minor, ($v.Build * 100 + $WinBuild)
if (-not $Out) { $Out = Join-Path (Split-Path -Parent $Payload) "ncwin-$($lock.nextcloud.version)-win.$WinBuild.msi" }
if (-not (Test-Path (Join-Path $Payload 'ncwin.exe'))) { throw "$Payload is not an assembled ncwin tree" }

# The license page: AGPL text from COPYING as minimal RTF.
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("ncwin-msi-" + [guid]::NewGuid())
New-Item -ItemType Directory $tmp | Out-Null
try {
    $text = Get-Content -Raw (Join-Path $repo 'COPYING')
    $escaped = $text.Replace('\', '\\').Replace('{', '\{').Replace('}', '\}') -replace "`r?`n", "\par`n"
    Set-Content -Encoding ASCII -Path (Join-Path $tmp 'license.rtf') -Value ("{\rtf1\ansi\deff0{\fonttbl{\f0 Consolas;}}\fs16 " + $escaped + "}")

    $wix = Join-Path $env:USERPROFILE '.dotnet\tools\wix.exe'
    if (-not (Test-Path $wix)) { $wix = 'wix' }
    Write-Host "wix build $msiVersion -> $Out"
    & $wix build -arch x64 `
        -ext WixToolset.UI.wixext -ext WixToolset.Util.wixext -ext WixToolset.Firewall.wixext `
        -bindpath "payload=$Payload" -bindpath "installer=$tmp" `
        -d "Version=$msiVersion" `
        (Join-Path $repo 'windows\installer\ncwin.wxs') (Join-Path $repo 'windows\installer\ui.wxs') `
        -o $Out
    if ($LASTEXITCODE -ne 0) { throw 'wix build failed' }
    $hash = (Get-FileHash -Algorithm SHA256 $Out).Hash.ToLower()
    Set-Content -Encoding ASCII -Path "$Out.sha256" -Value "$hash  $(Split-Path -Leaf $Out)"
    Write-Host "built $Out ($([math]::Round((Get-Item $Out).Length / 1MB)) MB, sha256 $hash)"
} finally {
    $resolvedTmp = [IO.Path]::GetFullPath($tmp)
    $tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
    if (-not $resolvedTmp.StartsWith($tempRoot, [StringComparison]::OrdinalIgnoreCase)) { throw 'MSI temporary directory is outside the temporary root' }
    Remove-Item -LiteralPath $resolvedTmp -Recurse -Force -ErrorAction SilentlyContinue
}
