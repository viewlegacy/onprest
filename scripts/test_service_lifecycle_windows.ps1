param(
  [Parameter(Mandatory = $true)][string]$AgentBin,
  [Parameter(Mandatory = $true)][string]$CapabilityFile
)

$ErrorActionPreference = 'Stop'
$AgentBin = (Resolve-Path $AgentBin).Path
$CapabilityFile = (Resolve-Path $CapabilityFile).Path
$DefaultConfig = Join-Path (Split-Path $AgentBin) 'capability.yaml'
Copy-Item $CapabilityFile $DefaultConfig -Force

function Invoke-AgentService([string[]]$Arguments) {
  $output = & $AgentBin service @Arguments 2>&1
  if ($LASTEXITCODE -ne 0) {
    throw "agent service $($Arguments -join ' ') failed: $output"
  }
  return ($output -join "`n")
}

try {
  & $AgentBin service stop 2>$null | Out-Null
  & $AgentBin service uninstall 2>$null | Out-Null

  Invoke-AgentService @('install') | Out-Null
  $installed = Invoke-AgentService @('status')
  if ($installed -notmatch '(?m)^installed: true$' -or
      $installed -notmatch '(?m)^native: windows-service$' -or
      $installed -notmatch [regex]::Escape("config: $DefaultConfig")) {
    throw "installed status contract failed: $installed"
  }
  $startMode = (Get-CimInstance Win32_Service -Filter "Name='onprest-agent'").StartMode
  if ($startMode -ne 'Manual') {
    throw "service install enabled boot-time start: $startMode"
  }

  Invoke-AgentService @('start') | Out-Null
  $running = $null
  for ($i = 0; $i -lt 40; $i++) {
    $running = Invoke-AgentService @('status')
    if ($running -match '(?m)^state: running$') { break }
    Start-Sleep -Milliseconds 250
  }
  if ($running -notmatch '(?m)^state: running$') {
    throw "service did not reach running state: $running"
  }
  Start-Sleep -Seconds 1
  $stillRunning = Invoke-AgentService @('status')
  if ($stillRunning -notmatch '(?m)^state: running$') {
    throw "service did not remain running: $stillRunning"
  }

  Invoke-AgentService @('stop') | Out-Null
  for ($i = 0; $i -lt 40; $i++) {
    $stopped = Invoke-AgentService @('status')
    if ($stopped -notmatch '(?m)^state: (running|stop_pending)$') { break }
    Start-Sleep -Milliseconds 250
  }
  if ($stopped -match '(?m)^state: (running|stop_pending)$') {
    throw "service did not stop: $stopped"
  }

  Invoke-AgentService @('remove') | Out-Null
  $removed = Invoke-AgentService @('status')
  if ($removed -notmatch '(?m)^installed: false$') {
    throw "service was not removed: $removed"
  }
}
finally {
  & $AgentBin service stop 2>$null | Out-Null
  & $AgentBin service uninstall 2>$null | Out-Null
}
