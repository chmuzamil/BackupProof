# BackupProof dashboard installer for Windows. Run in PowerShell as Administrator:
#
#   irm https://github.com/chmuzamil/BackupProof/releases/latest/download/install.ps1 | iex
#
# To pass options, download the script first:
#   & ([scriptblock]::Create((irm https://github.com/chmuzamil/BackupProof/releases/latest/download/install.ps1))) -Listen 0.0.0.0:8420
#
# Running it again upgrades BackupProof in place; your data is kept.
param(
  [string]$Version = "latest",
  [string]$Listen = "0.0.0.0:8420",
  [string]$PublicUrl = "",
  [switch]$Uninstall
)
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$Repo = "chmuzamil/BackupProof"
$Dir = Join-Path $env:ProgramFiles "BackupProof"
$Data = Join-Path $env:ProgramData "BackupProof\server"
$Exe = Join-Path $Dir "backupproof.exe"
$Task = "BackupProof Dashboard"

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw "Please run PowerShell as Administrator (right-click Start, Terminal (Admin))."
}

if ($Uninstall) {
  Write-Host "Removing BackupProof (your data in $Data is kept)..."
  Stop-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
  Unregister-ScheduledTask -TaskName $Task -Confirm:$false -ErrorAction SilentlyContinue
  Get-Process backupproof -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $Exe } | Stop-Process -Force
  Remove-NetFirewallRule -DisplayName "BackupProof Dashboard" -ErrorAction SilentlyContinue
  Remove-Item -Recurse -Force $Dir -ErrorAction SilentlyContinue
  Write-Host "Done."
  return
}

$fresh = -not (Test-Path (Join-Path $Data "backupproof.db"))
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$name = "backupproof-windows-$arch.exe"
$base = if ($Version -eq "latest") { "https://github.com/$Repo/releases/latest/download" } else { "https://github.com/$Repo/releases/download/$Version" }

$tmp = Join-Path $env:TEMP ("bp-install-" + [guid]::NewGuid())
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
try {
  Write-Host "Downloading BackupProof ($Version) for windows/$arch..."
  Invoke-WebRequest -UseBasicParsing -Uri "$base/$name" -OutFile (Join-Path $tmp $name)
  Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile (Join-Path $tmp "SHA256SUMS")

  Write-Host "Checking the download..."
  $line = Get-Content (Join-Path $tmp "SHA256SUMS") | Where-Object { $_ -match "[\s*]$([regex]::Escape($name))$" } | Select-Object -First 1
  if (-not $line) { throw "No checksum for $name in SHA256SUMS." }
  $expected = ($line -split "\s+")[0].ToLower()
  $actual = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $name)).Hash.ToLower()
  if ($expected -ne $actual) { throw "Checksum mismatch for $name; the download is damaged or tampered with." }

  Stop-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
  Get-Process backupproof -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $Exe } | Stop-Process -Force
  New-Item -ItemType Directory -Force -Path $Dir, $Data | Out-Null
  Copy-Item -Force (Join-Path $tmp $name) $Exe
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$taskArgs = "server --data `"$Data`" --listen $Listen"
if ($PublicUrl) { $taskArgs += " --public-url $PublicUrl" }
$action = New-ScheduledTaskAction -Execute $Exe -Argument $taskArgs
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName $Task -Action $action -Trigger $trigger -Settings $settings -User "SYSTEM" -RunLevel Highest -Force | Out-Null
Start-ScheduledTask -TaskName $Task

$port = ($Listen -split ":")[-1]
if (-not (Get-NetFirewallRule -DisplayName "BackupProof Dashboard" -ErrorAction SilentlyContinue)) {
  New-NetFirewallRule -DisplayName "BackupProof Dashboard" -Direction Inbound -Protocol TCP -LocalPort $port -Action Allow -Profile Private,Domain | Out-Null
}

$codeFile = Join-Path $Data "setup-code.txt"
if ($fresh) { for ($i = 0; $i -lt 30 -and -not (Test-Path $codeFile); $i++) { Start-Sleep -Seconds 1 } }

$url = if ($PublicUrl) { $PublicUrl } else { "http://localhost:$port" }
Write-Host ""
Write-Host "BackupProof is running." -ForegroundColor Green
Write-Host "  Open:        $url"
if (Test-Path $codeFile) { Write-Host "  Setup code:  $(Get-Content $codeFile)   (needed once, to create the admin account)" }
Write-Host "  Data:        $Data"
