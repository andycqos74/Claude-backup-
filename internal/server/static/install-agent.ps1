# Central Backup agent installer for Windows Server (run as Administrator).
# Usage (from the server GUI enrollment dialog):
#   .\install-agent.ps1 -Server https://SERVER:8443 -Token TOKEN -Fingerprint FP [-Name NAME]
param(
    [Parameter(Mandatory=$true)][string]$Server,
    [Parameter(Mandatory=$true)][string]$Token,
    [string]$Fingerprint = "",
    [string]$Name = ""
)
$ErrorActionPreference = "Stop"

# The server uses a self-signed certificate; downloads skip validation here,
# authenticity is enforced by the fingerprint pin during enrollment.
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
[Net.ServicePointManager]::ServerCertificateValidationCallback = { $true }

$dir = "$env:ProgramFiles\BackupAgent"
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$exe = "$dir\backup-agent.exe"

Write-Host "Downloading agent binary..."
Invoke-WebRequest -UseBasicParsing "$Server/dl/backup-agent-windows-amd64.exe" -OutFile $exe

Write-Host "Enrolling with $Server ..."
$enrollArgs = @("enroll", "--server", $Server, "--token", $Token)
if ($Fingerprint) { $enrollArgs += @("--fingerprint", $Fingerprint) }
if ($Name)        { $enrollArgs += @("--name", $Name) }
& $exe @enrollArgs
if ($LASTEXITCODE -ne 0) { throw "enrollment failed" }

Write-Host "Installing Windows service..."
& $exe service install
if ($LASTEXITCODE -ne 0) { throw "service install failed" }

Write-Host ""
Write-Host "Done. The agent service (CentralBackupAgent) is running."
Write-Host "  jobs:   & '$exe' job list"
Write-Host "  config: C:\ProgramData\BackupAgent\agent.yaml (syncs with the server GUI)"
