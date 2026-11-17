<#
.SYNOPSIS
  Henosis milestone 1 spike. Shows two things on your PC:
    1. what FL Studio does on disk when it saves a project, and
    2. what FL Studio's window caption says about the open project.

.DESCRIPTION
  Watches a folder (and its sub-folders) and logs every file event. When the
  folder has been quiet for a moment it reports which .flp files were saved.
  Every 2 seconds it also checks whether FL Studio is running and logs its
  window captions whenever they change.

  Nothing is modified. Everything shown on screen is also written to
  watchspike-log.txt next to this script, so the log can be sent back.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\watchspike.ps1 -Path "C:\Users\me\Documents\Image-Line\FL Studio\Projects\eltan"
#>
param(
    [string]$Path = (Get-Location).Path,
    [double]$QuietSeconds = 2.0,
    [string]$LogFile = (Join-Path $PSScriptRoot 'watchspike-log.txt')
)

$ErrorActionPreference = 'Stop'

if (-not (Test-Path -LiteralPath $Path -PathType Container)) {
    Write-Host "Folder not found: $Path" -ForegroundColor Red
    exit 1
}
$Path = (Resolve-Path -LiteralPath $Path).Path.TrimEnd('\')

function Write-Log([string]$Message) {
    $line = '{0}  {1}' -f (Get-Date -Format 'HH:mm:ss.fff'), $Message
    Write-Host $line
    Add-Content -LiteralPath $LogFile -Value $line -Encoding UTF8
}

# --- Window captions through the Win32 API -----------------------------------
# Get-Process only exposes one "main" window title. EnumWindows lists every
# visible top-level window of the process, which is what the real app will use.
$winApiSource = @'
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;

public static class HenosisWin
{
    private delegate bool EnumProc(IntPtr hWnd, IntPtr lParam);

    [DllImport("user32.dll")]
    private static extern bool EnumWindows(EnumProc callback, IntPtr lParam);

    [DllImport("user32.dll")]
    private static extern uint GetWindowThreadProcessId(IntPtr hWnd, out uint processId);

    [DllImport("user32.dll")]
    private static extern bool IsWindowVisible(IntPtr hWnd);

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    private static extern int GetWindowTextLength(IntPtr hWnd);

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    private static extern int GetWindowText(IntPtr hWnd, StringBuilder text, int maxCount);

    public static string[] VisibleTitles(int processId)
    {
        List<string> titles = new List<string>();
        EnumProc callback = delegate(IntPtr hWnd, IntPtr lParam)
        {
            uint pid;
            GetWindowThreadProcessId(hWnd, out pid);
            if (pid == (uint)processId && IsWindowVisible(hWnd))
            {
                int length = GetWindowTextLength(hWnd);
                if (length > 0)
                {
                    StringBuilder text = new StringBuilder(length + 1);
                    GetWindowText(hWnd, text, text.Capacity);
                    titles.Add(text.ToString());
                }
            }
            return true;
        };
        EnumWindows(callback, IntPtr.Zero);
        GC.KeepAlive(callback);
        return titles.ToArray();
    }
}
'@

$haveWinApi = $true
try {
    Add-Type -TypeDefinition $winApiSource -ErrorAction Stop
} catch {
    $haveWinApi = $false
}

function Get-FlState {
    $procs = @(Get-Process -Name 'FL64', 'FL' -ErrorAction SilentlyContinue)
    if ($procs.Count -eq 0) { return 'not running' }

    $parts = foreach ($p in $procs) {
        $titles = @()
        if ($haveWinApi) {
            try { $titles = @([HenosisWin]::VisibleTitles($p.Id)) } catch { $titles = @() }
        }
        if ($titles.Count -eq 0 -and $p.MainWindowTitle) { $titles = @($p.MainWindowTitle) }

        if ($titles.Count -gt 0) {
            $shown = ($titles | ForEach-Object { '"' + $_ + '"' }) -join ', '
        } else {
            $shown = '(no window caption)'
        }
        '{0}.exe pid {1}: {2}' -f $p.ProcessName, $p.Id, $shown
    }
    return ($parts -join ' | ')
}

# --- Paths the real app will ignore ------------------------------------------
function Test-Ignored([string]$FullPath) {
    $relative = $FullPath.Substring($Path.Length).TrimStart('\')
    $segments = $relative.Split('\')
    foreach ($segment in $segments) {
        if ($segment -ieq 'Backups' -or $segment -ieq 'Backup' -or $segment -ieq '.henosis') {
            return $true
        }
    }
    $leaf = $segments[$segments.Length - 1]
    return ($leaf -like '*.tmp' -or $leaf -like '*.part' -or $leaf -ieq 'Thumbs.db' -or $leaf -ieq 'desktop.ini')
}

# True when nobody has the file open for writing any more.
function Test-ReadableNow([string]$FullPath) {
    try {
        $stream = [System.IO.File]::Open($FullPath, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::Read)
        $stream.Close()
        return $true
    } catch {
        return $false
    }
}

# --- Watcher ------------------------------------------------------------------
$watcher = New-Object System.IO.FileSystemWatcher
$watcher.Path = $Path
$watcher.IncludeSubdirectories = $true
$watcher.InternalBufferSize = 65536
$watcher.NotifyFilter = [System.IO.NotifyFilters]'FileName, DirectoryName, Size, LastWrite'

$sourceIds = @()
foreach ($eventName in 'Changed', 'Created', 'Deleted', 'Renamed', 'Error') {
    $id = "henosis.$eventName"
    Register-ObjectEvent -InputObject $watcher -EventName $eventName -SourceIdentifier $id | Out-Null
    $sourceIds += $id
}
$watcher.EnableRaisingEvents = $true

Write-Log '--- watchspike started ---'
Write-Log "watching:   $Path"
Write-Log "log file:   $LogFile"
Write-Log ("powershell: {0}, window API: {1}" -f $PSVersionTable.PSVersion, $haveWinApi)
Write-Log 'Press Ctrl+C to stop.'

$pending = @{}          # full path -> last change type since the folder went quiet
$lastEventAt = $null
$lastFlState = $null
$nextFlPoll = Get-Date

try {
    while ($true) {
        $queued = @(Get-Event | Where-Object { $_.SourceIdentifier -like 'henosis.*' })
        foreach ($ev in $queued) {
            $sourceId = $ev.SourceIdentifier
            $fsArgs = $ev.SourceEventArgs
            Remove-Event -EventIdentifier $ev.EventIdentifier

            if ($sourceId -eq 'henosis.Error') {
                Write-Log ('WATCHER ERROR, events were dropped: ' + $fsArgs.GetException().Message)
                continue
            }

            $full = $fsArgs.FullPath
            if ($full -ieq $LogFile) { continue }

            $ignored = Test-Ignored $full
            if ($sourceId -eq 'henosis.Renamed') {
                $detail = '{0}  (was {1})' -f $fsArgs.Name, $fsArgs.OldName
            } else {
                $detail = $fsArgs.Name
            }
            if ($ignored) { $detail += '  [ignored]' }
            Write-Log ('event  {0,-8} {1}' -f $fsArgs.ChangeType, $detail)

            if (-not $ignored) {
                $pending[$full] = [string]$fsArgs.ChangeType
                $lastEventAt = Get-Date
            }
        }

        if ($lastEventAt -and ((Get-Date) - $lastEventAt).TotalSeconds -ge $QuietSeconds) {
            Write-Log ('QUIET for {0} s after changes to {1} path(s)' -f $QuietSeconds, $pending.Count)
            foreach ($full in @($pending.Keys | Sort-Object)) {
                if (-not $full.EndsWith('.flp', [System.StringComparison]::OrdinalIgnoreCase)) { continue }
                if (Test-Path -LiteralPath $full -PathType Leaf) {
                    $item = Get-Item -LiteralPath $full
                    $written = $item.LastWriteTime.ToString('HH:mm:ss.fff')
                    $readable = Test-ReadableNow $full
                    Write-Log ('  SAVE DETECTED  {0}  {1} bytes, written {2}, readable now: {3}' -f $item.Name, $item.Length, $written, $readable)
                } else {
                    Write-Log ('  .flp no longer exists: {0}' -f $full.Substring($Path.Length).TrimStart('\'))
                }
            }
            $pending = @{}
            $lastEventAt = $null
        }

        if ((Get-Date) -ge $nextFlPoll) {
            $state = Get-FlState
            if ($state -ne $lastFlState) {
                Write-Log ('FL STUDIO  ' + $state)
                $lastFlState = $state
            }
            $nextFlPoll = (Get-Date).AddSeconds(2)
        }

        Start-Sleep -Milliseconds 250
    }
} finally {
    $watcher.EnableRaisingEvents = $false
    foreach ($id in $sourceIds) {
        Unregister-Event -SourceIdentifier $id -ErrorAction SilentlyContinue
    }
    Get-Event | Where-Object { $_.SourceIdentifier -like 'henosis.*' } | Remove-Event
    $watcher.Dispose()
    Add-Content -LiteralPath $LogFile -Value ('{0}  --- stopped ---' -f (Get-Date -Format 'HH:mm:ss.fff')) -Encoding UTF8
}
