# The RedundaNet 3 installer for Windows, served at https://redundanet.com/install.ps1
# (the file docs/install.ps1 in the repository; install.sh beside it is the
# same for macOS and Linux). Windows PowerShell 5.1, which every Windows 10
# and 11 has, or PowerShell 7.
#
#   irm https://redundanet.com/install.ps1 | iex
#       installs the newest stable release as bin\redundanet.exe in your data
#       directory, where the agent can replace it when it updates itself,
#       adds that folder to your PATH, installs the agent as a logon task
#       that restarts on failure, and opens its page in your browser, which
#       walks you through the rest
#
# Options. A piped script takes no arguments, so they go in the environment
# first: $env:REDUNDANET_CHANNEL = "early" takes the newest release of either
# channel, $env:REDUNDANET_VERSION = "3.0.11" that release. The same as
# parameters:
#   & ([scriptblock]::Create((irm https://redundanet.com/install.ps1))) -Channel early
#
# Where things go. The data directory is the agent's own, %AppData%\redundanet
# (or $env:REDUNDANET_DATA_DIR). The binary lives in its bin\ because the
# agent updates itself by renaming a new version into place; a running one
# is renamed to redundanet.exe.old first, which Windows allows. That folder
# is added to your user PATH once, so terminals opened from then on find
# redundanet, and the one this runs in is told too. Nothing needs
# administrator rights.
#
# What it trusts. The release index, releases/index.yaml on the main branch,
# names every release's archives with their sha256 and size, newest first.
# The script fetches it over HTTPS, downloads the zip for this machine and
# refuses one whose hash or size differs. The index is signed by the release
# key built into every redundanet binary, and the installed binary checks
# that signature itself as the last step, so a tampered index is caught
# here, as it would be by the agent's own updater. To do the same by hand,
# the download page lists the archives and docs/install.md the commands.
#
# The whole script is one function called at the end, so a partial download
# runs nothing. Piped into iex it never calls exit, which would close your
# terminal; run as a file it exits 1 on failure, for scripts and CI.
param(
    [string]$Channel = "",
    [string]$Version = ""
)

function Install-RedundaNet([string]$Channel, [string]$Version) {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'   # Invoke-WebRequest is many times slower with its progress bar
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    } catch { }

    $repo = if ($env:REDUNDANET_REPO) { $env:REDUNDANET_REPO } else { 'adefilippo83/redundanet' }
    $index = if ($env:REDUNDANET_INDEX) { $env:REDUNDANET_INDEX } else { "https://raw.githubusercontent.com/$repo/main/releases/index.yaml" }
    if (-not $Channel) { $Channel = if ($env:REDUNDANET_CHANNEL) { $env:REDUNDANET_CHANNEL } else { 'stable' } }
    if (-not $Version) { $Version = "$env:REDUNDANET_VERSION" }
    $Version = $Version.TrimStart('v')
    if ($Channel -ne 'stable' -and $Channel -ne 'early') { throw "the channel is stable or early, not $Channel" }

    # The releases cover windows/amd64. A 32-bit PowerShell on a 64-bit
    # Windows reports the machine in PROCESSOR_ARCHITEW6432.
    $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    if ($arch -ne 'AMD64') { throw "no release for Windows on $arch; the releases cover windows/amd64" }
    $target = 'windows/amd64'

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('redundanet-install-' + [IO.Path]::GetRandomFileName())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        $text = Get-Text $index
        $rel = Select-Release -Text $text -Target $target -Channel $Channel -Want $Version
        if (-not $rel) {
            if ($Version) { throw "the release index does not list $Version for $target" }
            throw "the release index lists no $Channel release for $target"
        }
        if ($rel.Sha256.Length -ne 64) { throw "the index entry for $($rel.Version) has no usable sha256" }
        $name = $rel.Url.Substring($rel.Url.LastIndexOf('/') + 1)

        $real = Join-Path (Join-Path (Get-DataDir) 'bin') 'redundanet.exe'
        $have = ''
        if (Test-Path -LiteralPath $real) {
            $r = Invoke-Native $real @('version')
            if ($r.ExitCode -eq 0) { $have = ($r.StdOut -split '\s+')[1] }
        }
        if ($have -eq $rel.Version) {
            Note "redundanet $($rel.Version) is already at $real; the agent keeps itself up to date from here"
        } else {
            Note "downloading redundanet $($rel.Version) for $target"
            $zip = Join-Path $tmp $name
            try { Invoke-WebRequest -UseBasicParsing -Uri $rel.Url -OutFile $zip } catch { throw "could not download $($rel.Url): $($_.Exception.Message)" }
            $got = (Get-Item -LiteralPath $zip).Length
            if ($got -ne $rel.Size) { throw "$name is $got bytes, the index says $($rel.Size): refusing it" }
            $gotSha = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($gotSha -ne $rel.Sha256) { throw "$name has sha256 $gotSha, the index says $($rel.Sha256): refusing it" }
            $x = Join-Path $tmp 'x'
            Expand-Archive -LiteralPath $zip -DestinationPath $x -Force
            $exe = Get-ChildItem -LiteralPath $x -Recurse -Filter 'redundanet.exe' | Select-Object -First 1
            if (-not $exe) { throw "$name holds no redundanet.exe" }
            Unblock-File -LiteralPath $exe.FullName -ErrorAction SilentlyContinue
            $r = Invoke-Native $exe.FullName @('version')
            if ($r.ExitCode -ne 0) { throw "the downloaded binary does not run on this machine: $($r.Output)" }

            # Into the data directory, ours to write, so the agent can replace
            # it when it updates itself; a running one is renamed aside first,
            # as the updater does.
            New-Item -ItemType Directory -Force -Path (Split-Path -Parent $real) | Out-Null
            Copy-Item -LiteralPath $exe.FullName -Destination "$real.new" -Force
            if (Test-Path -LiteralPath $real) {
                if (Test-Path -LiteralPath "$real.old") { Remove-Item -LiteralPath "$real.old" -Force -ErrorAction SilentlyContinue }
                Move-Item -LiteralPath $real -Destination "$real.old" -Force
            }
            Move-Item -LiteralPath "$real.new" -Destination $real -Force
            Note "installed redundanet $($rel.Version) at $real"
        }

        # The folder on the PATH: the user's, in the registry, kept as the
        # kind it has (so %VAR% entries stay unexpanded), then announced to
        # Explorer so that terminals opened from now on see it; and this
        # process's own, so the commands below work here too.
        $binDir = Split-Path -Parent $real
        if (Add-UserPath $binDir) { Note "$binDir is on your PATH from now on; terminals opened from here find redundanet" }
        if (-not (Test-OnPath $env:Path $binDir)) { $env:Path = "$($env:Path.TrimEnd(';'));$binDir" }

        # The binary checks the index's signature with the release key built
        # into it, the same check its updater makes every hour.
        $r = Invoke-Native $real @('update', 'check', '--channel', $Channel)
        if ($r.ExitCode -eq 0) { Note "the signed release index verifies: $($r.Output)" }
        else { Note "warning: could not verify the signed release index with the installed binary: $($r.Output)" }

        # The agent as a logon task: without an identity it serves its page
        # alone, with the setup, and the page walks the member through the
        # rest. A service installed earlier is left as it is.
        $installed = $false
        $r = Invoke-Native $real @('service', 'status')
        if ($r.ExitCode -eq 0) { try { $installed = [bool]($r.StdOut | ConvertFrom-Json).installed } catch { } }
        if ($installed) {
            Note "a service is installed already; redundanet service install points it at this binary if it runs another"
        } else {
            $svcArgs = @('service', 'install')
            if ($Channel -ne 'stable') { $svcArgs += @('--channel', $Channel) }
            $r = Invoke-Native $real $svcArgs
            if ($r.ExitCode -eq 0) { Note "the agent runs as a logon task that starts at login" }
            else { Note "warning: the service could not be installed; redundanet service install says why" }
        }

        # The page: the agent needs a moment to come up.
        $link = ''
        for ($i = 0; $i -lt 15 -and -not $link; $i++) {
            $r = Invoke-Native $real @('open')
            if ($r.ExitCode -eq 0) { $link = ($r.StdOut -split "`r?`n" | Where-Object { $_ } | Select-Object -First 1) }
            if (-not $link) { Start-Sleep -Seconds 1 }
        }
        if ($link) {
            Note ""
            Note "the page is open in your browser and walks you through the rest: your identity,"
            Note "then the join request. If it did not open, this link does, once, for two minutes:"
            Note "  $link"
            Note "A new link any time: redundanet open. In a terminal instead: redundanet setup"
        } else {
            Note ""
            Note "next, on this computer:"
            Note "  redundanet open                 # the page, which walks you through identity and join request"
            Note "  redundanet setup                # the same, in this terminal"
        }
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

function Note([string]$Message) { Write-Host $Message }

# Get-DataDir is the agent's data directory for this user, as the binary
# computes it (Go's os.UserConfigDir, %AppData% on Windows, plus "redundanet").
function Get-DataDir {
    if ($env:REDUNDANET_DATA_DIR) { return $env:REDUNDANET_DATA_DIR }
    return Join-Path ([Environment]::GetFolderPath('ApplicationData')) 'redundanet'
}

function Get-Text([string]$Url) {
    try { $content = (Invoke-WebRequest -UseBasicParsing -Uri $Url).Content } catch { throw "could not fetch the release index at ${Url}: $($_.Exception.Message)" }
    if ($content -is [byte[]]) { $content = [Text.Encoding]::UTF8.GetString($content) }
    return [string]$content
}

# Select-Release reads the index and returns the release to install: the one
# named by Want, or the newest one of the channel (stable takes stable
# releases, early takes either) that is not withdrawn and has an archive for
# the target. Null means none. It mirrors pick() in install.sh.
function Select-Release([string]$Text, [string]$Target, [string]$Channel, [string]$Want) {
    $entries = @()
    $cur = $null
    foreach ($line in ($Text -split "`r?`n")) {
        if ($line -match '^  - version: (\S+)') {
            if ($cur) { $entries += $cur }
            $cur = [pscustomobject]@{ Version = $Matches[1]; Channel = ''; Withdrawn = ''; Url = ''; Sha256 = ''; Size = '' }
            continue
        }
        if (-not $cur) { continue }
        if ($line -match '^    channel: (\S+)') { $cur.Channel = $Matches[1]; continue }
        if ($line -match '^    withdrawn: (\S+)') { $cur.Withdrawn = $Matches[1]; continue }
        if ($line -match ('^\s+' + [regex]::Escape($Target) + ': ')) {
            if ($line -match 'url: ([^,} ]+)') { $cur.Url = $Matches[1] }
            if ($line -match 'sha256: ([0-9a-f]+)') { $cur.Sha256 = $Matches[1] }
            if ($line -match 'size: ([0-9]+)') { $cur.Size = $Matches[1] }
        }
    }
    if ($cur) { $entries += $cur }
    foreach ($e in $entries) {
        if ($Want) {
            if ($e.Version -ne $Want) { continue }
        } else {
            if ($e.Withdrawn -eq 'true') { continue }
            if ($e.Channel -ne 'stable' -and $Channel -ne 'early') { continue }
        }
        if ($e.Url -and $e.Sha256 -and $e.Size) {
            return [pscustomobject]@{ Version = $e.Version; Url = $e.Url; Sha256 = $e.Sha256; Size = [int64]$e.Size }
        }
        if ($Want) { return $null }
    }
    return $null
}

# Invoke-Native runs a program with its output captured, apart from this
# shell's error handling (a line on stderr is not an error here).
function Invoke-Native([string]$Exe, [string[]]$Arguments) {
    $psi = New-Object Diagnostics.ProcessStartInfo
    $psi.FileName = $Exe
    $quoted = foreach ($a in $Arguments) { if ($a -match '[\s"]') { '"' + ($a -replace '"', '\"') + '"' } else { $a } }
    $psi.Arguments = ($quoted -join ' ')
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.CreateNoWindow = $true
    $p = [Diagnostics.Process]::Start($psi)
    $errTask = $p.StandardError.ReadToEndAsync()
    $out = $p.StandardOutput.ReadToEnd()
    $err = $errTask.Result
    $p.WaitForExit()
    return [pscustomobject]@{ ExitCode = $p.ExitCode; StdOut = $out.Trim(); StdErr = $err.Trim(); Output = ($out + $err).Trim() }
}

function Test-OnPath([string]$PathList, [string]$Dir) {
    $want = $Dir.TrimEnd('\')
    foreach ($p in ("$PathList" -split ';')) {
        if ($p.Trim().TrimEnd('\') -ieq $want) { return $true }
    }
    return $false
}

# Add-UserPath appends Dir to the user's PATH in the registry unless it is
# there, keeping the value's kind, and tells running programs. True when it
# changed something.
function Add-UserPath([string]$Dir) {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
    if (-not $key) { $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment') }
    try {
        $raw = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        if (Test-OnPath ([Environment]::ExpandEnvironmentVariables($raw)) $Dir) { return $false }
        $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
        if ($raw) { try { $kind = $key.GetValueKind('Path') } catch { } }
        $new = if ($raw) { $raw.TrimEnd(';') + ';' + $Dir } else { $Dir }
        $key.SetValue('Path', $new, $kind)
    } finally {
        $key.Close()
    }
    Publish-Environment
    return $true
}

# Publish-Environment broadcasts WM_SETTINGCHANGE for "Environment", which
# makes Explorer reread the registry, so a terminal opened from the Start
# menu or the taskbar from now on has the new PATH. Best effort.
function Publish-Environment {
    try {
        if (-not ('RedundaNet.Native' -as [type])) {
            Add-Type -Namespace RedundaNet -Name Native -MemberDefinition @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
        }
        $result = [UIntPtr]::Zero
        [RedundaNet.Native]::SendMessageTimeout([IntPtr]0xffff, 0x1A, [UIntPtr]::Zero, 'Environment', 2, 5000, [ref]$result) | Out-Null
    } catch { }
}

# Run as a file (a download, CI) the script exits with a code; piped into
# iex it must not, since exit would close the terminal. Under iex
# $MyInvocation is the caller's, whose script, if any, is not this one.
$asFile = $false
try { $asFile = [bool]($MyInvocation.MyCommand.ScriptContents -like '*function Install-RedundaNet*') } catch { }
try {
    Install-RedundaNet -Channel $Channel -Version $Version
} catch {
    Write-Host "install.ps1: $($_.Exception.Message)" -ForegroundColor Red
    if ($asFile) { exit 1 }
    return
}
if ($asFile) { exit 0 }
