# SPDX-FileCopyrightText: 2026 Matthew Stanley
# SPDX-License-Identifier: AGPL-3.0-or-later

function Remove-NcwinBuildPath {
    [CmdletBinding()]
    param([Parameter(Mandatory)] [string]$Path, [Parameter(Mandatory)] [string]$Root)

    $base = [IO.Path]::GetFullPath($Root).TrimEnd('\', '/')
    $target = [IO.Path]::GetFullPath($Path).TrimEnd('\', '/')
    if ($target -eq $base -or -not $target.StartsWith($base + '\', [StringComparison]::OrdinalIgnoreCase)) {
        throw "cleanup path is outside its owning directory: $target"
    }
    if (-not (Test-Path -LiteralPath $target)) { return }

    $pending = New-Object 'System.Collections.Generic.Stack[string]'
    $pending.Push($target)
    while ($pending.Count) {
        $entry = $pending.Pop()
        $attributes = [IO.File]::GetAttributes($entry)
        if ([IO.Path]::GetFileName($entry) -eq '.git' -or ($attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw "cleanup contains a repository or reparse point: $entry"
        }
        if ($attributes -band [IO.FileAttributes]::Directory) {
            foreach ($child in [IO.Directory]::EnumerateFileSystemEntries($entry)) { $pending.Push($child) }
        }
    }
    Remove-Item -LiteralPath $target -Recurse -ErrorAction Stop
}
