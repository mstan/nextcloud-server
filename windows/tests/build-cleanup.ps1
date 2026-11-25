# SPDX-FileCopyrightText: 2026 Matthew Stanley
# SPDX-License-Identifier: AGPL-3.0-or-later
[CmdletBinding()]
param([Parameter(Mandatory)] [string]$Root)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '..\build\disposable.ps1')
New-Item -ItemType Directory -Path $Root -Force | Out-Null
$fixture = Join-Path $Root ([IO.Path]::GetRandomFileName())
$owned = Join-Path $fixture 'owned'
$outside = Join-Path $fixture 'owned-other'
New-Item -ItemType Directory -Path $owned, $outside | Out-Null
Set-Content -LiteralPath (Join-Path $outside 'sentinel') -Value 'preserve'
function Refuses([string]$Path, [string]$Owner) {
    $refused = $false
    try { Remove-NcwinBuildPath -Path $Path -Root $Owner } catch { $refused = $true }
    if (-not $refused) { throw "unsafe cleanup was accepted: $Path" }
}
try {
    Refuses $outside $owned
    Refuses $owned $owned
    if (-not (Test-Path -LiteralPath $owned)) { throw 'owning directory was removed' }
    if ((Get-Content -LiteralPath (Join-Path $outside 'sentinel')) -ne 'preserve') { throw 'outside data changed' }

    $repository = Join-Path $owned 'repository'
    New-Item -ItemType Directory -Path $repository | Out-Null
    $marker = Join-Path $repository '.git'
    Set-Content -LiteralPath $marker -Value 'synthetic build-cleanup test marker'
    Set-Content -LiteralPath (Join-Path $repository 'sentinel') -Value 'preserve'
    Refuses $repository $owned
    if (-not (Test-Path -LiteralPath $marker) -or (Get-Content -LiteralPath (Join-Path $repository 'sentinel')) -ne 'preserve') { throw 'repository contents changed' }
    [IO.File]::Delete($marker)

    $link = Join-Path $owned 'linked'
    New-Item -ItemType Junction -Path $link -Target $outside | Out-Null
    try {
        Refuses $owned $fixture
        if ((Get-Content -LiteralPath (Join-Path $outside 'sentinel')) -ne 'preserve') { throw 'junction target changed' }
    } finally { [IO.Directory]::Delete($link) }

    $literal = Join-Path $owned 'a[1]'
    $other = Join-Path $owned 'a1'
    New-Item -ItemType Directory -Path $literal, $other | Out-Null
    Set-Content -LiteralPath (Join-Path $literal 'file') -Value 'disposable'
    Remove-NcwinBuildPath -Path $literal -Root $owned
    if ((Test-Path -LiteralPath $literal) -or -not (Test-Path -LiteralPath $other)) { throw 'literal path cleanup failed' }
    Remove-NcwinBuildPath -Path (Join-Path $owned 'absent') -Root $owned
    Remove-NcwinBuildPath -Path $owned -Root $fixture
    if (Test-Path -LiteralPath $owned) { throw 'owned directory was retained' }
    'build cleanup: passed containment, sibling-prefix, repository, junction, literal-path and missing-path cases'
} finally { Remove-NcwinBuildPath -Path $fixture -Root $Root }
