package build

// WindowsUpdateScript is the PowerShell script uploaded to the VM to search,
// download, and install Windows Updates. It mirrors the behavior of
// packer-plugin-windows-update:
//   - Exit 0: no more updates, done
//   - Exit 101: reboot required, caller should restart VM and re-run
//   - Other: error
const WindowsUpdateScript = `param(
    [string]$SearchCriteria = "BrowseOnly=0 and IsInstalled=0",
    [string[]]$Filters = @('include:$true'),
    [int]$UpdateLimit = 1000,
    [switch]$OnlyCheckForRebootRequired
)

$ErrorActionPreference = "Stop"

function Test-RebootRequired {
    # Check Windows Update API
    try {
        $sysInfo = New-Object -ComObject Microsoft.Update.SystemInfo
        if ($sysInfo.RebootRequired) { return $true }
    } catch {}
    # Check Component-Based Servicing
    $cbsKey = "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending"
    if (Test-Path $cbsKey) { return $true }
    # Check pending file renames
    $pfro = Get-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager" -Name PendingFileRenameOperations -ErrorAction SilentlyContinue
    if ($pfro.PendingFileRenameOperations) { return $true }
    return $false
}

if ($OnlyCheckForRebootRequired) {
    if (Test-RebootRequired) {
        Write-Host "Reboot is still required."
        exit 101
    }
    Write-Host "No reboot required."
    exit 0
}

# Check for pending reboot first
if (Test-RebootRequired) {
    Write-Host "Reboot is required before searching for updates."
    exit 101
}

Write-Host "Searching for updates: $SearchCriteria"
$session = New-Object -ComObject Microsoft.Update.Session
$searcher = $session.CreateUpdateSearcher()

try {
    $result = $searcher.Search($SearchCriteria)
} catch {
    Write-Host "Search failed: $_"
    # Try repairing Windows Update
    Write-Host "Attempting Windows Update repair..."
    Stop-Service -Name wuauserv -Force -ErrorAction SilentlyContinue
    Remove-Item "$env:systemroot\SoftwareDistribution" -Recurse -Force -ErrorAction SilentlyContinue
    Start-Service -Name wuauserv -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 10
    $result = $searcher.Search($SearchCriteria)
}

$updates = $result.Updates
Write-Host "Found $($updates.Count) updates before filtering."

# Apply filters
$filtered = New-Object -ComObject Microsoft.Update.UpdateColl
foreach ($update in $updates) {
    $matched = $false
    $action = "include"
    foreach ($filter in $Filters) {
        $parts = $filter -split ":", 2
        if ($parts.Count -ne 2) { continue }
        $filterAction = $parts[0].Trim().ToLower()
        $expr = $parts[1].Trim()
        $_ = $update
        try {
            if (Invoke-Expression $expr) {
                $action = $filterAction
                $matched = $true
                break
            }
        } catch {}
    }
    if (-not $matched) { continue }
    if ($action -eq "include") {
        Write-Host "  [include] $($update.Title)"
        $update.AcceptEula() | Out-Null
        $filtered.Add($update) | Out-Null
        if ($filtered.Count -ge $UpdateLimit) {
            Write-Host "  Reached update limit ($UpdateLimit), will continue after reboot."
            break
        }
    } else {
        Write-Host "  [exclude] $($update.Title)"
    }
}

if ($filtered.Count -eq 0) {
    # Windows Update can report zero results while newer cumulative updates
    # are still pending — the metadata index hasn't refreshed yet. Pause and
    # re-query once to confirm there truly is nothing left.
    Write-Host "No updates found, verifying..."
    Start-Sleep -Seconds 30
    try { $verify = $searcher.Search($SearchCriteria) } catch { $verify = $null }
    if ($verify -and $verify.Updates.Count -gt 0) {
        Write-Host "Verification found $($verify.Updates.Count) additional updates, requesting reboot cycle."
        exit 101
    }
    Write-Host "Verified: no updates to install."
    exit 0
}

Write-Host "Downloading $($filtered.Count) updates..."
$downloader = $session.CreateUpdateDownloader()
$downloader.Updates = $filtered
$dlResult = $downloader.Download()
Write-Host "Download result: $($dlResult.ResultCode)"

Write-Host "Installing $($filtered.Count) updates..."
$installer = $session.CreateUpdateInstaller()
$installer.Updates = $filtered
$installResult = $installer.Install()
Write-Host "Install result: $($installResult.ResultCode)"

for ($i = 0; $i -lt $filtered.Count; $i++) {
    $ur = $installResult.GetUpdateResult($i)
    $title = $filtered.Item($i).Title
    Write-Host "  [$($ur.ResultCode)] $title"
}

if ($installResult.RebootRequired -or (Test-RebootRequired)) {
    Write-Host "Reboot required after update installation."
    exit 101
}

if ($filtered.Count -ge $UpdateLimit) {
    Write-Host "Update limit reached, reboot to continue."
    exit 101
}

Write-Host "All updates installed successfully."
exit 0
`
