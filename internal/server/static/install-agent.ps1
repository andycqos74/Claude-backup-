# Central Backup agent installer for Windows (Server or Windows 11).
# Run in an ELEVATED PowerShell (Run as Administrator). Works on both
# Windows PowerShell 5.1 and PowerShell 7+.
#
# Usage (from the server GUI enrollment dialog):
#   .\install-agent.ps1 -Server https://SERVER:8443 -Token TOKEN -Fingerprint FP [-Name NAME]
param(
    [Parameter(Mandatory=$true)][string]$Server,
    [Parameter(Mandatory=$true)][string]$Token,
    [string]$Fingerprint = "",
    [string]$Name = ""
)
$ErrorActionPreference = "Stop"
$Server = $Server.TrimEnd('/')

# Must be elevated to install a service.
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "This installer must be run in an elevated PowerShell (right-click > Run as Administrator)."
}

# The server uses a self-signed certificate, so the binary download skips
# TLS validation here; authenticity is then enforced by the certificate
# fingerprint pin during enrollment (and on every later connection).
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$iwrArgs = @{ UseBasicParsing = $true }
if ($PSVersionTable.PSVersion.Major -ge 6) {
    # PowerShell 7+: the ServicePointManager callback below doesn't apply
    # (System.Net.Http-based); use the switch instead.
    $iwrArgs["SkipCertificateCheck"] = $true
} else {
    # Windows PowerShell 5.1 (.NET Framework): the modern
    # ServerCertificateValidationCallback delegate is unreliable in some
    # environments (AV/EDR hooking, ServicePoint caching). The legacy
    # ICertificatePolicy interface is the long-established, more reliable
    # way to bypass validation on this stack.
    #
    # Guard with a type-exists check rather than relying on
    # -ErrorAction SilentlyContinue: Add-Type's "type already exists"
    # failure is a terminating error that ignores -ErrorAction under
    # $ErrorActionPreference = "Stop" (as set above), which matters if this
    # script runs twice in the same PowerShell process (e.g. re-invoked
    # after a bootstrapping wrapper already defined the same class).
    if (-not ('CBTrustAllCertsPolicy' -as [type])) {
        Add-Type @"
using System.Net;
using System.Security.Cryptography.X509Certificates;
public class CBTrustAllCertsPolicy : ICertificatePolicy {
    public bool CheckValidationResult(ServicePoint sp, X509Certificate cert, WebRequest req, int problem) { return true; }
}
"@
    }
    [Net.ServicePointManager]::CertificatePolicy = New-Object CBTrustAllCertsPolicy
}

$dir = "$env:ProgramFiles\BackupAgent"
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$exe = "$dir\backup-agent.exe"

# If a previous install exists, stop and remove its service so this is
# idempotent (safe to re-run).
if (Get-Service -Name CentralBackupAgent -ErrorAction SilentlyContinue) {
    Write-Host "Existing agent found - removing old service..."
    & $exe service uninstall 2>$null
    Start-Sleep -Seconds 2
}

Write-Host "Downloading agent binary from $Server ..."
Invoke-WebRequest @iwrArgs -Uri "$Server/dl/backup-agent-windows-amd64.exe" -OutFile $exe
# Clear the mark-of-the-web so the service can launch it without prompts.
Unblock-File -Path $exe -ErrorAction SilentlyContinue

Write-Host "Enrolling with $Server ..."
$enrollArgs = @("enroll", "--server", $Server, "--token", $Token)
if ($Fingerprint) { $enrollArgs += @("--fingerprint", $Fingerprint) }
if ($Name)        { $enrollArgs += @("--name", $Name) }
& $exe @enrollArgs
if ($LASTEXITCODE -ne 0) { throw "enrollment failed (exit $LASTEXITCODE)" }

Write-Host "Installing and starting the Windows service..."
& $exe service install
if ($LASTEXITCODE -ne 0) { throw "service install failed (exit $LASTEXITCODE)" }

Write-Host ""
Write-Host "Done. The agent service (CentralBackupAgent) is installed and running."
Write-Host "  service: Get-Service CentralBackupAgent"
Write-Host "  logs:    Event Viewer > Windows Logs > Application (source CentralBackupAgent)"
Write-Host "  jobs:    & '$exe' job list"
Write-Host "  config:  C:\ProgramData\BackupAgent\agent.yaml (syncs with the server GUI)"
Write-Host ""
Write-Host "The client should appear under Clients in the server GUI within a few seconds."
