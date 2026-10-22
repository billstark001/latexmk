#Requires -Version 5.1
# Standalone tests: no Pester installation, network, real profiles or registry writes.
param([string] $Binary)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'install-cli.ps1')
if ($Binary) { $Binary = (Get-Item -LiteralPath $Binary).FullName }
$script:failures = 0

# Keep architecture fixtures independent of the actual Windows registry.
function Get-LatexmkMachineArchitecture { return $script:machineArchitecture }

function Assert-True($Condition, [string] $Message) {
    if (-not $Condition) { throw $Message }
}

function Assert-Fails([scriptblock] $Action) {
    $failed = $false
    try { & $Action } catch { $failed = $true }
    Assert-True $failed 'Expected installation to fail'
}

function Test-Installer([string] $Name, [scriptblock] $Action) {
    $root = Join-Path ([IO.Path]::GetTempPath()) ('latexmk-installer-' + [guid]::NewGuid().ToString('N'))
    # Use characters requiring literal quoting; ASCII source also loads in PS 5.1.
    $homeDir = Join-Path $root ('home ''$(throw injected) [x] ' + [char]0x8bba + [char]0x6587)
    [void] [IO.Directory]::CreateDirectory($homeDir)
    $fixture = @{
        Root = $root
        Prefix = (Join-Path $homeDir 'managed')
        Rc = (Join-Path $homeDir 'profile.ps1')
        Local = (Join-Path $root 'source.exe')
    }
    [IO.File]::WriteAllText($fixture.Local, 'local CLI')
    $saved = @{}
    foreach ($key in @('OS', 'PROCESSOR_ARCHITECTURE', 'PROCESSOR_ARCHITEW6432', 'PATH', 'LATEXMK_CLI', 'LATEXMK_TEST_KEEP', 'LOCALAPPDATA')) {
        $saved[$key] = [Environment]::GetEnvironmentVariable($key, 'Process')
    }
    $env:OS = 'Windows_NT'
    $env:PROCESSOR_ARCHITECTURE = 'AMD64'
    $env:PROCESSOR_ARCHITEW6432 = ''
    $env:LOCALAPPDATA = $homeDir
    $script:machineArchitecture = ''
    try {
        & $Action $fixture
        Write-Host "PASS $Name"
    } catch {
        $script:failures++
        Write-Host "FAIL ${Name}: $_`n$($_.ScriptStackTrace)"
    } finally {
        foreach ($key in $saved.Keys) { [Environment]::SetEnvironmentVariable($key, $saved[$key], 'Process') }
        Remove-Item -LiteralPath $root -Recurse -Force
    }
}

Test-Installer 'local copy, quoting, PATH precedence, replacement and uninstall' {
    param($f)
    $original = '# user configuration' + "`r`n" + '$env:LATEXMK_TEST_KEEP = ''untouched''' + "`r`n"
    [IO.File]::WriteAllText($f.Rc, $original, (New-Object Text.UnicodeEncoding($false, $true)))
    $argsForInstall = @{ Local = $f.Local; Prefix = $f.Prefix; Rc = @($f.Rc) }
    Invoke-LatexmkInstall @argsForInstall
    $command = Join-Path (Join-Path $f.Prefix 'bin') 'latexmk.exe'
    Assert-True ([IO.File]::ReadAllText($command) -ceq 'local CLI') 'Local binary was not copied'
    $installed = [IO.File]::ReadAllBytes($f.Rc)
    Assert-True ($installed[0] -eq 255 -and $installed[1] -eq 254) 'UTF-16 profile encoding changed'
    Invoke-LatexmkInstall @argsForInstall
    Assert-True ([Convert]::ToBase64String([IO.File]::ReadAllBytes($f.Rc)) -ceq [Convert]::ToBase64String($installed)) 'Repeated install changed profile'
    $env:PATH = 'C:\TeXLive\bin;C:\Windows'
    . $f.Rc
    $firstPath = $env:PATH
    . $f.Rc
    Assert-True ($firstPath -ceq $env:PATH) 'Repeated sourcing duplicated PATH'
    Assert-True ($env:PATH.StartsWith((Join-Path $f.Prefix 'bin') + ';')) 'CLI directory does not precede TeX Live'
    Assert-True ($env:LATEXMK_CLI -ceq $command) 'Quoted CLI path did not round trip'
    Assert-True ($env:LATEXMK_TEST_KEEP -eq 'untouched') 'User profile was damaged'
    [IO.File]::WriteAllText($f.Local, 'replacement CLI')
    Invoke-LatexmkInstall @argsForInstall
    Assert-True ([IO.File]::ReadAllText($command) -ceq 'replacement CLI') 'Local update did not replace binary'
    Invoke-LatexmkInstall -Uninstall -Prefix $f.Prefix -Rc $f.Rc
    Invoke-LatexmkInstall -Uninstall -Prefix $f.Prefix -Rc $f.Rc
    Assert-True ([IO.File]::ReadAllText($f.Rc) -ceq $original) 'Uninstall damaged user profile'
    Assert-True ([IO.File]::Exists($command)) 'Uninstall deleted binary'
}

Test-Installer 'validate every profile and binary before replacement' {
    param($f)
    Invoke-LatexmkInstall -Local $f.Local -Prefix $f.Prefix -Rc $f.Rc
    $before = [IO.File]::ReadAllText($f.Rc)
    $second = Join-Path $f.Root 'broken.ps1'
    [IO.File]::WriteAllText($second, "# >>> latexmk CLI >>>`nkeep this`n")
    [IO.File]::WriteAllText($f.Local, 'replacement')
    Assert-Fails { Invoke-LatexmkInstall -Local $f.Local -Prefix $f.Prefix -Rc @($f.Rc, $second) }
    Assert-Fails { Invoke-LatexmkInstall -Local (Join-Path $f.Root 'missing') -Prefix $f.Prefix -Rc $f.Rc }
    Assert-Fails { Invoke-LatexmkInstall -Local $f.Local -Release latest -Prefix $f.Prefix -Rc $f.Rc }
    Assert-Fails { Invoke-LatexmkInstall -Uninstall -Release latest -Prefix $f.Prefix -Rc $f.Rc }
    Assert-Fails { Invoke-LatexmkInstall -Local $f.Local -Prefix ($f.Prefix + ';bad') -Rc $f.Rc }
    Assert-True ([IO.File]::ReadAllText($f.Rc) -ceq $before) 'Failure changed profile'
    Assert-True ([IO.File]::ReadAllText((Join-Path $f.Prefix 'bin/latexmk.exe')) -ceq 'local CLI') 'Failure replaced binary'
    foreach ($text in @("# <<< latexmk CLI <<<`n", "# >>> latexmk CLI >>>`n# >>> latexmk CLI >>>`n# <<< latexmk CLI <<<`n")) {
        Assert-Fails { Remove-LatexmkBlock $text }
    }
}

Test-Installer 'uninstall no-op and multiple profiles' {
    param($f)
    Invoke-LatexmkInstall -Uninstall -Prefix $f.Prefix -Rc $f.Rc
    Assert-True (-not [IO.File]::Exists($f.Rc)) 'Uninstall created profile'
    [IO.File]::WriteAllText($f.Rc, '# no final newline')
    Invoke-LatexmkInstall -Uninstall -Prefix $f.Prefix -Rc $f.Rc
    Assert-True ([IO.File]::ReadAllText($f.Rc) -ceq '# no final newline') 'No-op uninstall changed bytes'
    $second = Join-Path $f.Root 'other edition/profile.ps1'
    Invoke-LatexmkInstall -Local $f.Local -Prefix $f.Prefix -Rc @($f.Rc, $second)
    Assert-True ([IO.File]::ReadAllText($second).Contains('$env:LATEXMK_CLI')) 'Second profile not registered'
}

Test-Installer 'existing BOM-less profile can load Unicode paths in Windows PowerShell 5.1' {
    param($f)
    # Chinese paths can round-trip through the system ANSI code page (e.g. 936).
    # A supplementary character requires Unicode regardless of that code page.
    $f.Prefix += ' ' + [char]0xd83d + [char]0xde80
    $original = "# existing ASCII profile`r`n"
    [IO.File]::WriteAllText($f.Rc, $original, (New-Object Text.UTF8Encoding($false)))
    Invoke-LatexmkInstall -Local $f.Local -Prefix $f.Prefix -Rc $f.Rc
    $bytes = [IO.File]::ReadAllBytes($f.Rc)
    Assert-True ($bytes[0] -eq 239 -and $bytes[1] -eq 187 -and $bytes[2] -eq 191) 'Unicode registration requires a UTF-8 BOM for Windows PowerShell 5.1'
    . $f.Rc
    Assert-True ($env:LATEXMK_CLI -ceq (Join-Path (Join-Path $f.Prefix 'bin') 'latexmk.exe')) 'Unicode command path was corrupted'
    Invoke-LatexmkInstall -Uninstall -Prefix $f.Prefix -Rc $f.Rc
    Assert-True ([IO.File]::ReadAllText($f.Rc) -ceq $original) 'Encoding conversion damaged user text'
}

Test-Installer 'ANSI encoding upgrades without losing existing profile text' {
    param($f)
    $ansi = [Text.Encoding]::GetEncoding(1252)
    $content = '# caf' + [char]0xe9 + "`r`n" + (ConvertTo-LatexmkLiteral $f.Prefix)
    $encoding = Get-LatexmkProfileEncoding $ansi $content
    Assert-True ($encoding.CodePage -eq 65001 -and $encoding.GetPreamble().Length -eq 3) 'ANSI cannot represent Unicode registration; expected UTF-8 with BOM'
    [IO.File]::WriteAllText($f.Rc, $content, $encoding)
    Assert-True ([IO.File]::ReadAllText($f.Rc) -ceq $content) 'Legacy profile text was lost during encoding conversion'
    $ascii = "# ASCII profile`r`n"
    Assert-True ((Get-LatexmkProfileEncoding $ansi $ascii).CodePage -eq 1252) 'Unnecessary ANSI conversion'
}

Test-Installer 'relative aliases of one profile are registered only once' {
    param($f)
    $alias = Join-Path (Split-Path -Parent $f.Rc) './profile.ps1'
    $profiles = @($f.Rc, $alias)
    if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) { $profiles += $f.Rc.ToUpperInvariant() }
    Invoke-LatexmkInstall -Local $f.Local -Prefix $f.Prefix -Rc $profiles
    $text = [IO.File]::ReadAllText($f.Rc)
    Assert-True ([regex]::Matches($text, '# >>> latexmk CLI >>>').Count -eq 1) 'Profile aliases created duplicate registrations'
}

Test-Installer 'typographic quotes in paths remain literal PowerShell text' {
    param($f)
    foreach ($codePoint in @(0x2018, 0x2019, 0x201a, 0x201b)) {
        $value = 'before' + [char] $codePoint + 'after'
        $literal = ConvertTo-LatexmkLiteral $value
        $actual = & ([scriptblock]::Create($literal))
        Assert-True ($actual -ceq $value) 'Typographic quote changed the literal or escaped its string'
    }
    $prefix = $f.Prefix + [char]0x2019 + 's tools'
    Invoke-LatexmkInstall -Local $f.Local -Prefix $prefix -Rc $f.Rc
    . $f.Rc
    Assert-True ($env:LATEXMK_CLI -ceq (Join-Path (Join-Path $prefix 'bin') 'latexmk.exe')) 'Profile path with typographic quote failed'
}

Test-Installer 'machine architecture takes precedence over x64 emulation on ARM64' {
    param($f)
    $script:machineArchitecture = 'ARM64'
    $env:PROCESSOR_ARCHITECTURE = 'AMD64'
    $env:PROCESSOR_ARCHITEW6432 = ''
    Assert-True ((Get-LatexmkArchitecture) -eq 'arm64') 'x64 emulation masked the native ARM64 architecture'
    $script:machineArchitecture = 'AMD64'
    $env:PROCESSOR_ARCHITECTURE = 'x86'
    Assert-True ((Get-LatexmkArchitecture) -eq 'amd64') '32-bit shell masked the native x64 architecture'
}

# Replace download functions in this script scope; no requests leave the process.
function Get-LatexmkLatestRelease {
    $script:requests.Add('latest')
    return 'v9.8.7'
}
function Get-LatexmkDownload([string] $Uri, [string] $Destination) {
    $script:requests.Add($Uri)
    if ($script:downloadFailure) { throw 'Simulated download failure' }
    if ($Uri.EndsWith('/SHA256SUMS')) {
        [IO.File]::WriteAllText($Destination, $script:checksumText)
    } else {
        [IO.File]::WriteAllText($Destination, 'release CLI')
    }
}

Test-Installer 'latest pins once, emulated shells and native ARM64, checksum failures preserve files' {
    param($f)
    $script:downloadFailure = $false
    foreach ($architecture in @('amd64', 'arm64')) {
        $script:requests = New-Object 'Collections.Generic.List[string]'
        $env:PROCESSOR_ARCHITECTURE = 'x86'
        $env:PROCESSOR_ARCHITEW6432 = $architecture.ToUpper()
        if ($architecture -eq 'amd64') { $env:PROCESSOR_ARCHITEW6432 = 'AMD64' }
        $digestFile = Join-Path $f.Root 'digest'
        [IO.File]::WriteAllText($digestFile, 'release CLI')
        $digest = (Get-FileHash -LiteralPath $digestFile -Algorithm SHA256).Hash.ToLower()
        $script:checksumText = "$digest  latexmk_windows_$architecture.exe`n"
        Invoke-LatexmkInstall -Prefix $f.Prefix -Rc $f.Rc
        Assert-True ($script:requests.Count -eq 3 -and $script:requests[0] -eq 'latest') 'Latest release was not pinned once'
        Assert-True ($script:requests[2].EndsWith("/v9.8.7/latexmk_windows_$architecture.exe")) 'Wrong release asset'
        $before = [IO.File]::ReadAllText($f.Rc)
        foreach ($checksums in @(('0' * 64 + "  latexmk_windows_$architecture.exe`n"), '', ($script:checksumText * 2))) {
            $script:checksumText = $checksums
            Assert-Fails { Invoke-LatexmkInstall -Release v9.8.7 -Prefix $f.Prefix -Rc $f.Rc }
            Assert-True ([IO.File]::ReadAllText((Join-Path $f.Prefix 'bin/latexmk.exe')) -ceq 'release CLI') 'Checksum failure replaced binary'
            Assert-True ([IO.File]::ReadAllText($f.Rc) -ceq $before) 'Checksum failure changed profile'
        }
    }
    $env:PROCESSOR_ARCHITEW6432 = ''
    $env:PROCESSOR_ARCHITECTURE = 'ARM64'
    Assert-True ((Get-LatexmkArchitecture) -eq 'arm64') 'Native ARM64 not detected'
    $env:PROCESSOR_ARCHITECTURE = 'x86'
    Assert-Fails { Get-LatexmkArchitecture }
    $env:PROCESSOR_ARCHITECTURE = 'AMD64'
    Assert-Fails { Invoke-LatexmkInstall -Release '../invalid' -Prefix $f.Prefix -Rc $f.Rc }
    $script:downloadFailure = $true
    Assert-Fails { Invoke-LatexmkInstall -Release v9.8.7 -Prefix $f.Prefix -Rc $f.Rc }
    $script:downloadFailure = $false
    Assert-True ([IO.File]::ReadAllText((Join-Path $f.Prefix 'bin/latexmk.exe')) -ceq 'release CLI') 'Download failure replaced binary'
    Assert-True (@(Get-ChildItem -LiteralPath (Join-Path $f.Prefix 'bin') -Filter '.latexmk-*').Count -eq 0) 'Temporary download files leaked'
}

Test-Installer 'script and downloaded-script entry points register local copies' {
    param($f)
    & (Join-Path $PSScriptRoot 'install-cli.ps1') -Local $f.Local -Prefix $f.Prefix -Rc $f.Rc
    $text = [IO.File]::ReadAllText((Join-Path $PSScriptRoot 'install-cli.ps1'))
    & ([scriptblock]::Create($text)) -Uninstall -Prefix $f.Prefix -Rc $f.Rc
    Assert-True (-not [IO.File]::ReadAllText($f.Rc).Contains('# >>> latexmk CLI >>>')) 'Downloaded-script entry point did not remove registration'
}

if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
    Test-Installer 'locked Windows executable preserves old binary and profile on update failure' {
        param($f)
        Invoke-LatexmkInstall -Local $f.Local -Prefix $f.Prefix -Rc $f.Rc
        $command = Join-Path $f.Prefix 'bin/latexmk.exe'
        $before = [IO.File]::ReadAllText($f.Rc)
        $locked = [IO.File]::Open($command, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::None)
        try {
            [IO.File]::WriteAllText($f.Local, 'replacement')
            Assert-Fails { Invoke-LatexmkInstall -Local $f.Local -Prefix $f.Prefix -Rc $f.Rc }
        } finally { $locked.Dispose() }
        Assert-True ([IO.File]::ReadAllText($command) -ceq 'local CLI') 'Locked executable was replaced'
        Assert-True ([IO.File]::ReadAllText($f.Rc) -ceq $before) 'Locked executable failure changed registration'
    }
}

if ($Binary) {
    Test-Installer 'installed native exe runs in a fresh PowerShell process' {
        param($f)
        Invoke-LatexmkInstall -Local $Binary -Prefix $f.Prefix -Rc $f.Rc
        $hostExe = (Get-Process -Id $PID).Path
        $expression = '$ErrorActionPreference = ''Stop''; . ' + (ConvertTo-LatexmkLiteral $f.Rc) + '; if ((Get-Command latexmk -CommandType Application).Source -ine $env:LATEXMK_CLI) { throw ''Wrong CLI on PATH'' }; latexmk version; if ($LASTEXITCODE -ne 0) { exit 1 }; & $env:LATEXMK_CLI help; exit $LASTEXITCODE'
        # UTF-16LE EncodedCommand avoids legacy 5.1 native argument quoting.
        $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($expression))
        $startArguments = @{
            FilePath = $hostExe
            ArgumentList = @('-NoLogo', '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-EncodedCommand', $encoded)
            Wait = $true
            PassThru = $true
        }
        if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) { $startArguments.WindowStyle = 'Hidden' }
        $process = Start-Process @startArguments
        try {
            Assert-True ($process.ExitCode -eq 0) 'Installed exe could not run through PATH and LATEXMK_CLI'
        } finally { $process.Dispose() }
    }
}

if ($script:failures -gt 0) { throw "$script:failures installer test(s) failed" }
Write-Host 'All PowerShell installer tests passed.'
