package build

// ElevatedCredsPath is where the coordinator writes the SSH user's
// credentials before each elevated task launch. The wrapper's Start phase
// reads this file and immediately deletes it so the credentials only exist
// on disk for the brief window between SFTP upload and the wrapper's first
// Remove-Item call.
const ElevatedCredsPath = "C:/Windows/Temp/aileron-elevated-creds.txt"

// ElevatedRunnerScript is a PowerShell script that launches another script
// with full administrative privileges as the SSH user via a Windows Scheduled
// Task registered with stored credentials, and exposes its output and
// completion through short, repeatable RPC-style invocations.
//
// OpenSSH on Windows hands admin users a UAC-filtered (non-elevated) token,
// so HKLM writes, service control, Windows Update, and bcdedit fail in the
// SSH session. We can't simply elevate the existing token because UAC
// elevation requires interactive consent. We can't use S4U logon (which
// doesn't need a password) because S4U is restricted to domain accounts and
// virtual/managed service accounts; it rejects local users on standalone
// machines with HRESULT 0x80070057 (E_INVALIDARG) at the UserId field. And
// we can't use a GroupId principal because Task Scheduler runs that with
// the invoker's filtered token rather than promoting to the linked admin
// token.
//
// Stored-credential registration (-User <name> -Password <pass> -RunLevel
// Highest) works for local admin accounts, runs the task in the user's
// own security context (so HKCU resolves to the right hive), and grants
// the full unfiltered admin token.
//
// The wrapper is invoked in three phases so that any SSH session loss —
// e.g. when the target script reconfigures the VM's network — does not
// stall the build. Each phase is a short SSH command that the coordinator
// can retry independently:
//
//	Start  -Script <path> [-EnvB64 <b64>] : register & launch the task, wait
//	                                        briefly for launch evidence, exit
//	Poll   -Offset <bytes>                : emit new log bytes since offset,
//	                                        report state
//	Cleanup                               : unregister task and delete temp files
//
// -EnvB64 is a base64-encoded, newline-separated NAME=VALUE list that Start
// turns into $env: assignments inside the runner, so provisioner env vars
// reach the elevated child. Base64 survives the cmd.exe -> powershell.exe
// argument boundary without any quoting hazards. Values must not contain
// newlines (the framing is line-based).
//
// The runner's first action is to write a marker line
// ("[elevated] runner: user=<name> elevated=True|False") to the log file and
// verify its token actually holds the Administrator role; if not, it writes
// exit code 252 and refuses to run the target. The marker doubles as launch
// evidence for Start's bounded wait, closing the race where an async
// Start-ScheduledTask hasn't taken effect by the first Poll.
//
// Poll's final stdout line is always a status marker
// (`__AILERON_STATUS__ state=running|completed offset=N [exit=N]`) that the
// coordinator parses to know when to stop polling.
const ElevatedRunnerScript = `param(
    [Parameter(Mandatory=$true)]
    [ValidateSet("Start","Poll","Cleanup")]
    [string]$Phase,

    [string]$Script,
    [long]$Offset = 0,
    [string]$EnvB64 = ""
)

$ErrorActionPreference = "Stop"

$taskName   = "AileronElevated"
$logFile    = "C:\Windows\Temp\aileron-elevated.log"
$exitFile   = "C:\Windows\Temp\aileron-elevated.exit"
$runnerPath = "C:\Windows\Temp\aileron-elevated-runner.ps1"
$credsPath  = "C:\Windows\Temp\aileron-elevated-creds.txt"
$statusMark = "__AILERON_STATUS__"

switch ($Phase) {
    "Start" {
        if (-not $Script) { throw "Start phase requires -Script" }
        if (-not (Test-Path $credsPath)) {
            throw "elevation credentials not found at $credsPath"
        }
        $credsLines = Get-Content -Path $credsPath
        Remove-Item -Force $credsPath -ErrorAction SilentlyContinue
        if ($credsLines.Count -lt 2) {
            throw "elevation credentials at $credsPath are malformed"
        }
        # WindowsIdentity returns the fully-qualified form (MACHINE\User on
        # workgroup, DOMAIN\User on domain-joined) that Register-ScheduledTask
        # requires. Constructing the qualifier ourselves (".\user" or
        # "$env:COMPUTERNAME\user") is fragile — the resolution path inside
        # the Task Scheduler service can return ERROR_NONE_MAPPED for
        # shorthand forms in some Windows versions.
        $user = ([System.Security.Principal.WindowsIdentity]::GetCurrent()).Name
        $pass = $credsLines[1]

        Remove-Item $logFile, $exitFile -ErrorAction SilentlyContinue

        # Provisioner env vars travel as a base64 NAME=VALUE list and become
        # $env: assignments inside the runner. Split/trim use [char]10/[char]13
        # instead of backtick escapes on purpose; values get single-quoted with
        # doubled inner quotes so arbitrary content survives as PS source.
        $envLines = @()
        if ($EnvB64) {
            $envText = [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String($EnvB64))
            foreach ($pair in ($envText -split [char]10)) {
                $pair = $pair.TrimEnd([char]13)
                if (-not $pair) { continue }
                $eq = $pair.IndexOf('=')
                if ($eq -lt 1) { continue }
                $n = $pair.Substring(0, $eq)
                $v = $pair.Substring($eq + 1)
                $envLines += ('$env:' + $n + " = '" + ($v -replace "'", "''") + "'")
            }
        }

        # Runner script invokes the target as a child process so the target's
        # ` + "`" + `exit` + "`" + ` statement terminates the child, not the runner. Output is
        # redirected to a log file and the exit code is saved to a separate
        # file so the Poll phase can detect completion without parsing logs.
        #
        # Before anything else the runner writes a marker line to the log and
        # checks its token really holds the Administrator role. The marker is
        # the launch evidence Start waits on below, and the role check is the
        # only authoritative elevation signal: Register-ScheduledTask happily
        # registers RunLevel Highest for a non-admin account and then runs the
        # task with a plain token. In that case the runner refuses to run the
        # target and reports sentinel exit 252 so the coordinator can fail the
        # step with a clear error instead of letting the script die on
        # access-denied.
        #
        # The write order in the finally block matters: the exit file goes
        # down FIRST so a target script that schedules its own shutdown (and
        # races the runner against the OS shutdown sequence) cannot leave us
        # without a result. The log append happens after -- losing the log
        # line is annoying but recoverable; losing the exit file wedges the
        # build. The target redirect appends (*>>) because the marker line is
        # already in the log.
        $runnerHead = @(
            '$id = [System.Security.Principal.WindowsIdentity]::GetCurrent()',
            '$principal = New-Object System.Security.Principal.WindowsPrincipal($id)',
            '$isAdmin = $principal.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator)',
            ('Set-Content -Path "' + $logFile + '" -Value ("[elevated] runner: user=" + $id.Name + " elevated=" + $isAdmin)'),
            'if (-not $isAdmin) {',
            ('  "252" | Set-Content "' + $exitFile + '" -NoNewline'),
            ('  Add-Content -Path "' + $logFile + '" -Value "[elevated] ERROR: task token is NOT elevated; the SSH account is not a local Administrator or UAC token filtering applied. Refusing to run target script." -ErrorAction SilentlyContinue'),
            '  exit 252',
            '}'
        )
        $runnerTail = @(
            'try {',
            ('  powershell.exe -NoProfile -ExecutionPolicy Bypass -File "' + $Script + '" *>> "' + $logFile + '"'),
            '  $childExit = $LASTEXITCODE',
            '} catch {',
            '  $childExit = 1',
            '} finally {',
            '  if ($null -eq $childExit) { $childExit = 1 }',
            ('  $childExit | Set-Content "' + $exitFile + '" -NoNewline'),
            ('  Add-Content -Path "' + $logFile + '" -Value ("[elevated] script exited with code " + $childExit) -ErrorAction SilentlyContinue'),
            '}'
        )
        $runnerLines = $runnerHead + $envLines + $runnerTail
        $runnerLines | Set-Content -Path $runnerPath

        $action = New-ScheduledTaskAction -Execute "powershell.exe" -Argument ('-NoProfile -ExecutionPolicy Bypass -File "' + $runnerPath + '"')

        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
        Register-ScheduledTask -TaskName $taskName -Action $action -User $user -Password $pass -RunLevel Highest | Out-Null
        Write-Output "[elevated] running as $user (RunLevel=Highest)"
        Start-ScheduledTask -TaskName $taskName

        # Start-ScheduledTask is asynchronous: the call returns as soon as the
        # request is queued, not when the runner process exists. Without this
        # bounded wait the first Poll can observe a task still in Ready state
        # that has never run (LastTaskResult 0x41303) and misread it as
        # completed-with-error. Launch evidence is any of: the runner's marker
        # line in the log, an exit file (instant failures), or the task state
        # reaching Running. On timeout emit diagnostics but do NOT throw --
        # Poll owns the verdict, and its never-ran handling keeps waiting.
        $launched = $false
        for ($i = 0; $i -lt 40; $i++) {
            if ((Test-Path $logFile) -or (Test-Path $exitFile)) { $launched = $true; break }
            $t = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
            if ($t -and $t.State -eq "Running") { $launched = $true; break }
            Start-Sleep -Milliseconds 500
        }
        if ($launched) {
            Write-Output "[elevated] task started"
        } else {
            $t = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
            $info = Get-ScheduledTaskInfo -TaskName $taskName -ErrorAction SilentlyContinue
            $st = if ($t) { $t.State } else { "unknown" }
            $ltr = if ($info) { '0x{0:X}' -f $info.LastTaskResult } else { "unknown" }
            Write-Output ("[elevated] WARNING: task showed no launch evidence within 20s (state=" + $st + " LastTaskResult=" + $ltr + "); polling anyway")
        }
    }

    "Poll" {
        # Read new bytes from the log since $Offset. FileShare.ReadWrite lets
        # us read while the runner is still writing.
        $newOffset = [long]$Offset
        if (Test-Path $logFile) {
            $stream = [System.IO.File]::Open(
                $logFile,
                [System.IO.FileMode]::Open,
                [System.IO.FileAccess]::Read,
                [System.IO.FileShare]::ReadWrite)
            try {
                if ($newOffset -gt $stream.Length) {
                    # Log shorter than our offset — runner restarted or
                    # truncated. Resync from the start.
                    $newOffset = 0
                }
                if ($newOffset -gt 0) {
                    [void]$stream.Seek($newOffset, [System.IO.SeekOrigin]::Begin)
                }
                $reader = New-Object System.IO.StreamReader($stream)
                while (-not $reader.EndOfStream) {
                    [Console]::Out.WriteLine($reader.ReadLine())
                }
                $newOffset = $stream.Position
                $reader.Dispose()
            } finally {
                $stream.Dispose()
            }
        }

        # Completion is gated on the exit file: the runner writes it only
        # after the target script has exited, so its presence is unambiguous.
        # If no exit file exists we fall back to the task's own state: a
        # registered task that is no longer Running (vanished, or back to
        # Ready) means the runner died without flushing an exit code — most
        # commonly because the target script rebooted the VM out from under
        # itself. Either way the task is not coming back, so report
        # completed-with-error rather than poll forever.
        $state = "running"
        $exitLine = ""
        if (Test-Path $exitFile) {
            $state = "completed"
            $code = (Get-Content -Path $exitFile -Raw)
            if ($null -eq $code) { $code = "" }
            $code = $code.Trim()
            if (-not $code) { $code = "1" }
            $exitLine = " exit=$code"
        } else {
            $task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
            if (-not $task) {
                $state = "completed"
                $exitLine = " exit=1"
            } elseif ($task.State -ne "Running") {
                # Task is registered but not running and there is no exit
                # file. Two very different cases hide here:
                #
                #   LastTaskResult 0x41303 (267011, SCHED_S_TASK_HAS_NOT_RUN):
                #   the task has NEVER run -- Start-ScheduledTask is async and
                #   its effect may not have landed yet. Report running so the
                #   coordinator keeps polling; the build context bounds the
                #   total wait.
                #
                #   Anything else: the task ran and stopped without producing
                #   the exit file. Trust Windows's own LastTaskResult before
                #   defaulting to error -- a clean exit gets recorded there
                #   even if the runner couldn't write the file (most commonly
                #   because the target script rebooted the VM mid-runner).
                $info = Get-ScheduledTaskInfo -TaskName $taskName -ErrorAction SilentlyContinue
                if ($info -and $info.LastTaskResult -eq 267011) {
                    [Console]::Out.WriteLine("[elevated] task registered but not yet run (0x41303); waiting")
                } else {
                    $state = "completed"
                    if ($info -and $null -ne $info.LastTaskResult) {
                        $exitLine = " exit=$($info.LastTaskResult)"
                    } else {
                        $exitLine = " exit=1"
                    }
                }
            }
        }

        Write-Output ("$statusMark state=$state offset=$newOffset$exitLine")
    }

    "Cleanup" {
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
        Remove-Item -Force $runnerPath, $logFile, $exitFile -ErrorAction SilentlyContinue
    }
}
`
