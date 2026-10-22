#Requires -Version 5.1
<#
.SYNOPSIS
Install the remote latexmk CLI for the current PowerShell user, without admin rights.
.DESCRIPTION
Downloads a release and verifies SHA-256, or copies an existing exe with -Local.
Registers LATEXMK_CLI and PATH in the current user's all-hosts profile. Use -Rc
for other profiles (including another PowerShell edition). -Uninstall removes
only the managed profile blocks; installed binaries are retained.
#>
[CmdletBinding()]
param(
    [string] $Local,
    [string] $Release = 'latest',
    [string] $Prefix,
    [string[]] $Rc,
    [switch] $Uninstall
)

function ConvertTo-LatexmkLiteral([string] $Value) {
    # PowerShell recognizes typographic single quotes as string delimiters too.
    return "'" + [regex]::Replace($Value, "['\u2018\u2019\u201a\u201b]", '$0$0') + "'"
}

function Get-LatexmkMachineArchitecture {
    # Machine-scoped environment comes from the registry, not the emulated
    # process environment. This API is available in .NET Framework / PS 5.1.
    try { return [Environment]::GetEnvironmentVariable('PROCESSOR_ARCHITECTURE', 'Machine') }
    catch { return $null }
}

function Get-LatexmkArchitecture {
    # An x64 shell on ARM64 can report AMD64 with no ARCHITEW6432 variable.
    $architecture = Get-LatexmkMachineArchitecture
    if (-not $architecture) { $architecture = $env:PROCESSOR_ARCHITEW6432 }
    if (-not $architecture) { $architecture = $env:PROCESSOR_ARCHITECTURE }
    switch ($architecture) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        default { throw "Unsupported Windows architecture: $architecture" }
    }
}

function Get-LatexmkLatestRelease {
    $result = Invoke-RestMethod -Uri 'https://api.github.com/repos/billstark001/latexmk/releases/latest' -UseBasicParsing -TimeoutSec 30
    return $result.tag_name
}

function Get-LatexmkDownload([string] $Uri, [string] $Destination) {
    Invoke-WebRequest -Uri $Uri -OutFile $Destination -UseBasicParsing -TimeoutSec 180 | Out-Null
}

function Remove-LatexmkBlock([string] $Text) {
    $inside = $false
    $result = New-Object System.Text.StringBuilder
    # Retain exact line endings and any user text without a final newline.
    foreach ($match in [regex]::Matches($Text, '[^\r\n]*(?:\r\n|\n|\r|$)')) {
        $line = $match.Value.TrimEnd([char[]] "`r`n")
        if ($line -ceq '# >>> latexmk CLI >>>') {
            if ($inside) { throw 'Malformed latexmk CLI profile markers' }
            $inside = $true
        } elseif ($line -ceq '# <<< latexmk CLI <<<') {
            if (-not $inside) { throw 'Malformed latexmk CLI profile markers' }
            $inside = $false
        } elseif (-not $inside) {
            [void] $result.Append($match.Value)
        }
    }
    if ($inside) { throw 'Malformed latexmk CLI profile markers' }
    return $result.ToString()
}

function Read-LatexmkProfile([string] $Name) {
    $full = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Name)
    $text = ''
    $bytes = [byte[]] @()
    # UTF-8 BOM also works in Windows PowerShell 5.1 for non-ASCII paths.
    $encoding = New-Object System.Text.UTF8Encoding($true)
    if (Test-Path -LiteralPath $full) {
        $item = Get-Item -LiteralPath $full -Force
        if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw "Profile must be a regular file (symlinks are not replaced): $full"
        }
        $bytes = [IO.File]::ReadAllBytes($full)
        $reader = New-Object IO.StreamReader($full, [Text.Encoding]::Default, $true)
        try {
            $text = $reader.ReadToEnd()
            $encoding = $reader.CurrentEncoding
        } finally { $reader.Dispose() }
        # Preserve whether the original encoding had a BOM.
        if ($encoding.CodePage -eq 65001) {
            $hasBom = $bytes.Length -ge 3 -and $bytes[0] -eq 239 -and $bytes[1] -eq 187 -and $bytes[2] -eq 191
            $encoding = New-Object Text.UTF8Encoding($hasBom)
        }
    }
    return @{ Path = $full; Text = $text; Bytes = $bytes; Encoding = $encoding; Clean = (Remove-LatexmkBlock $text) }
}

function Set-LatexmkFile([string] $Staged, [string] $Destination) {
    if ([IO.File]::Exists($Destination)) {
        # Replace preserves the old file on failure, including a running exe.
        [IO.File]::Replace($Staged, $Destination, [System.Management.Automation.Language.NullString]::Value)
    } else {
        [IO.File]::Move($Staged, $Destination)
    }
}

function Get-LatexmkProfileEncoding([Text.Encoding] $Encoding, [string] $Content) {
    # ANSI profiles cannot always represent the installed path. BOM-less UTF-8
    # is also read as ANSI by Windows PowerShell 5.1, even when written by PS 7.
    $roundTrip = $Encoding.GetString($Encoding.GetBytes($Content))
    if ($roundTrip -cne $Content -or ($Encoding.CodePage -eq 65001 -and $Content -match '[^\x00-\x7f]')) {
        return New-Object Text.UTF8Encoding($true)
    }
    return $Encoding
}

function Invoke-LatexmkInstall {
    [CmdletBinding()]
    param(
        [string] $Local,
        [string] $Release = 'latest',
        [string] $Prefix,
        [string[]] $Rc,
        [switch] $Uninstall
    )
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'
    if ($Local -and $PSBoundParameters.ContainsKey('Release')) { throw '-Local and -Release cannot be combined' }
    if ($Uninstall -and ($Local -or $PSBoundParameters.ContainsKey('Release'))) {
        throw '-Uninstall cannot be combined with -Local or -Release'
    }
    if (-not $Rc) { $Rc = @($PROFILE.CurrentUserAllHosts) }
    if (-not $Prefix) {
        if (-not $env:LOCALAPPDATA) { throw 'LOCALAPPDATA is unavailable; specify -Prefix' }
        $Prefix = Join-Path $env:LOCALAPPDATA 'latexmk'
    }
    foreach ($name in @($Prefix) + $Rc + @($Local)) {
        if ($name -match '[\r\n\x00]') { throw 'Paths must not contain newlines or NUL' }
    }
    $Prefix = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Prefix)
    if ($Prefix.Contains(';')) { throw 'Prefix must not contain the PATH separator (;)' }
    # Validate every profile before replacing any binary or registration.
    $seenProfiles = @{}
    $profiles = @(foreach ($name in $Rc) {
        $profileFile = Read-LatexmkProfile $name
        # Resolve before deduplicating: relative aliases and case variants can
        # refer to one Windows profile and must not look like concurrent edits.
        if (-not $seenProfiles.ContainsKey($profileFile.Path)) {
            $seenProfiles[$profileFile.Path] = $true
            $profileFile
        }
    })
    $bin = Join-Path $Prefix 'bin'
    $command = Join-Path $bin 'latexmk.exe'
    $block = ''
    if (-not $Uninstall) {
        if ($env:OS -ne 'Windows_NT') { throw 'This installer requires Windows' }
        [void] [IO.Directory]::CreateDirectory($bin)
        if (Test-Path -LiteralPath $command) {
            $item = Get-Item -LiteralPath $command -Force
            if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
                throw "Managed command must be a regular file: $command"
            }
        }
        $staged = Join-Path $bin ('.latexmk-install-' + [guid]::NewGuid().ToString('N'))
        $checksums = $staged + '.sha256'
        $oldTls = [Net.ServicePointManager]::SecurityProtocol
        try {
            if ($Local) {
                $source = (Get-Item -LiteralPath $Local -Force).FullName
                if ([string]::Equals($source, $command, [StringComparison]::OrdinalIgnoreCase)) {
                    throw 'Local source must differ from the managed command'
                }
                [IO.File]::Copy($source, $staged)
            } else {
                $architecture = Get-LatexmkArchitecture
                [Net.ServicePointManager]::SecurityProtocol = $oldTls -bor [Net.SecurityProtocolType]::Tls12
                if ($Release -eq 'latest') { $Release = Get-LatexmkLatestRelease }
                if (-not $Release -or $Release -notmatch '^[a-zA-Z0-9._-]+$') { throw 'Invalid release tag' }
                $asset = "latexmk_windows_$architecture.exe"
                $base = "https://github.com/billstark001/latexmk/releases/download/$Release"
                Get-LatexmkDownload "$base/SHA256SUMS" $checksums
                Get-LatexmkDownload "$base/$asset" $staged
                $entries = @([IO.File]::ReadAllLines($checksums) | Where-Object { $_ -match ('^([a-fA-F0-9]{64})  ' + [regex]::Escape($asset) + '$') })
                if ($entries.Count -ne 1) { throw "Expected one SHA-256 checksum for $asset" }
                $expected = $entries[0].Substring(0, 64)
                $actual = (Get-FileHash -LiteralPath $staged -Algorithm SHA256).Hash
                if ($actual -ne $expected) { throw 'Download checksum mismatch; existing CLI was not replaced' }
            }
            Set-LatexmkFile $staged $command
        } finally {
            [Net.ServicePointManager]::SecurityProtocol = $oldTls
            foreach ($temp in @($staged, $checksums)) {
                if ([IO.File]::Exists($temp)) { [IO.File]::Delete($temp) }
            }
        }
        $literalCommand = ConvertTo-LatexmkLiteral $command
        $literalBin = ConvertTo-LatexmkLiteral $bin
        $block = @(
            '# >>> latexmk CLI >>>'
            ('$env:LATEXMK_CLI = ' + $literalCommand)
            # Prepend our directory, removing any previous copies. Machine PATH
            # can contain TeX Live's latexmk, which must not shadow this command.
            ('$env:PATH = ' + $literalBin + ' + '';'' + ((@($env:PATH -split '';'' | Where-Object { $_ -and $_.TrimEnd(''\'') -ine ' + $literalBin + '.TrimEnd(''\'') })) -join '';'')')
            '# <<< latexmk CLI <<<'
        ) -join "`r`n"
        $block += "`r`n"
    }
    foreach ($profileFile in $profiles) {
        $content = $profileFile.Clean
        if (-not $Uninstall) {
            if ($content.Length -gt 0 -and -not $content.EndsWith("`n") -and -not $content.EndsWith("`r")) {
                $content += "`r`n"
            }
            $content += $block
        }
        if ($content -ceq $profileFile.Text) { continue }
        $parent = Split-Path -Parent $profileFile.Path
        [void] [IO.Directory]::CreateDirectory($parent)
        $temp = Join-Path $parent ('.latexmk-profile-' + [guid]::NewGuid().ToString('N'))
        try {
            $writeEncoding = Get-LatexmkProfileEncoding $profileFile.Encoding $content
            [IO.File]::WriteAllText($temp, $content, $writeEncoding)
            $now = Read-LatexmkProfile $profileFile.Path
            if ([Convert]::ToBase64String($now.Bytes) -cne [Convert]::ToBase64String($profileFile.Bytes)) {
                throw "Profile changed during installation; rerun: $($profileFile.Path)"
            }
            Set-LatexmkFile $temp $profileFile.Path
        } finally {
            if ([IO.File]::Exists($temp)) { [IO.File]::Delete($temp) }
        }
        Write-Host "Updated $($profileFile.Path)"
    }
    if ($Uninstall) {
        Write-Host 'Registration removed. Installed binaries were retained. Open a new PowerShell session.'
    } else {
        Write-Host "Registered $command. Open a new PowerShell session, then run: latexmk version"
    }
}

# Dot-sourcing loads the functions for isolated installer tests.
if ($MyInvocation.InvocationName -ne '.') { Invoke-LatexmkInstall @PSBoundParameters }
