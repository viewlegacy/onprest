param(
  [Parameter(Mandatory = $true)][string]$AgentBin,
  [Parameter(Mandatory = $true)][string]$GatewayBin,
  [Parameter(Mandatory = $true)][string]$CapabilityFile
)

$ErrorActionPreference = 'Stop'
$AgentBin = (Resolve-Path $AgentBin).Path
$GatewayBin = (Resolve-Path $GatewayBin).Path
$CapabilityFile = (Resolve-Path $CapabilityFile).Path
$DefaultConfig = Join-Path (Split-Path $AgentBin) 'capability.yaml'
$artifactDir = Split-Path $AgentBin
$gatewayProcess = $null
$blockingValidation = $null
$blackhole = $null
$runtimeWriter = $null
$readerName = $null
$readerCreated = $false
$primaryFailure = $null
$cleanupFailure = $null
$previousGatewayAddr = [Environment]::GetEnvironmentVariable('GATEWAY_ADDR', 'Process')
$previousAgentPublicKey = [Environment]::GetEnvironmentVariable('GATEWAY_AGENT_PUBLIC_KEY', 'Process')
$previousAPIKeys = [Environment]::GetEnvironmentVariable('GATEWAY_API_KEYS_JSON', 'Process')
$previousRateLimitRPS = [Environment]::GetEnvironmentVariable('GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND', 'Process')
$previousRateLimitBurst = [Environment]::GetEnvironmentVariable('GATEWAY_RATE_LIMIT_BURST', 'Process')

try {
$agentSecret = (& $GatewayBin create-agent-secret | ConvertFrom-Json)
(Get-Content $CapabilityFile -Raw).Replace('PRIVATE_KEY', $agentSecret.agent_private_key) | Set-Content $DefaultConfig
$apiSecret = (& $GatewayBin create-key --name service-test --capabilities runtime_marker_a,runtime_marker_b,rollout_marker_new | ConvertFrom-Json)
$env:GATEWAY_ADDR = '127.0.0.1:18080'
$env:GATEWAY_AGENT_PUBLIC_KEY = $agentSecret.agent_public_key
$env:GATEWAY_API_KEYS_JSON = ConvertTo-Json -Compress -InputObject @(@{name=$apiSecret.name; key_hash=$apiSecret.key_hash; capabilities=@($apiSecret.capabilities)})
$env:GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND = '1000'
$env:GATEWAY_RATE_LIMIT_BURST = '1000'
$gatewayOutput = Join-Path (Split-Path $AgentBin) 'service-test-gateway.jsonl'
$gatewayProcess = Start-Process -FilePath $GatewayBin -RedirectStandardOutput $gatewayOutput -RedirectStandardError "$gatewayOutput.stderr" -PassThru -WindowStyle Hidden
for ($i = 0; $i -lt 100; $i++) {
  try { Invoke-RestMethod http://127.0.0.1:18080/healthz | Out-Null; break } catch { Start-Sleep -Milliseconds 50 }
}
Invoke-RestMethod http://127.0.0.1:18080/healthz | Out-Null
if ($gatewayProcess.HasExited) { throw "test Gateway exited early with code $($gatewayProcess.ExitCode)" }

function Invoke-AgentService([string[]]$Arguments) {
  $output = & $AgentBin service @Arguments 2>&1
  if ($LASTEXITCODE -ne 0) {
    throw "agent service $($Arguments -join ' ') failed: $output"
  }
  return ($output -join "`n")
}

function Remove-AgentServiceIfPresent {
  $service = Get-Service -Name 'onprest-agent' -ErrorAction SilentlyContinue
  if ($null -eq $service) {
    return
  }
  if ($service.Status -ne 'Stopped') {
    Invoke-AgentService @('stop') | Out-Null
  }
  Invoke-AgentService @('uninstall') | Out-Null
}

function Invoke-RuntimeMarker([string]$Capability) {
  try {
    Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:18080/api/v1/capabilities/$Capability" -Method Post `
      -Headers @{Authorization="Bearer $($apiSecret.api_key)"} -ContentType 'application/json' -Body '{}' | Out-Null
  }
  catch {
    if ($_.Exception.Response.StatusCode.value__ -ne 400) { throw }
  }
}

function Invoke-MCP([string]$Body) {
  return Invoke-RestMethod -Uri http://127.0.0.1:18080/mcp -Method Post `
    -Headers @{Authorization="Bearer $($apiSecret.api_key)"} -ContentType 'application/json' -Body $Body
}

function Get-OpenAPIText {
  return (Invoke-WebRequest -UseBasicParsing -Uri http://127.0.0.1:18080/openapi.json `
    -Headers @{Authorization="Bearer $($apiSecret.api_key)"}).Content
}

function Get-SharedFileHash([string]$Path) {
  $stream = [System.IO.FileStream]::new(
    $Path,
    [System.IO.FileMode]::Open,
    [System.IO.FileAccess]::Read,
    ([System.IO.FileShare]::ReadWrite -bor [System.IO.FileShare]::Delete)
  )
  try {
    $hasher = [System.Security.Cryptography.SHA256]::Create()
    try {
      return ([System.BitConverter]::ToString($hasher.ComputeHash($stream))).Replace('-', '')
    }
    finally {
      $hasher.Dispose()
    }
  }
  finally {
    $stream.Dispose()
  }
}

function Assert-OldPublicContract {
  $rest = Invoke-RestMethod -Uri http://127.0.0.1:18080/api/v1/capabilities/runtime_marker_a -Method Post `
    -Headers @{Authorization="Bearer $($apiSecret.api_key)"} -ContentType 'application/json' -Body '{"runtime_marker_a":"service-old"}'
  if ($rest.count -ne 1 -or $rest.rows[0].value -ne 'service-old') { throw "old REST contract changed: $($rest | ConvertTo-Json -Compress -Depth 10)" }
  $mcp = Invoke-MCP '{"jsonrpc":"2.0","id":"old-call","method":"tools/call","params":{"name":"runtime_marker_a","arguments":{"runtime_marker_a":"service-old"}}}'
  if ($mcp.result.isError -eq $true -or $mcp.result.structuredContent.count -ne 1 -or $mcp.result.structuredContent.rows[0].value -ne 'service-old') {
    throw "old MCP contract changed: $($mcp | ConvertTo-Json -Compress -Depth 10)"
  }
  $tools = Invoke-MCP '{"jsonrpc":"2.0","id":"old-list","method":"tools/list"}'
  if (@($tools.result.tools.name) -notcontains 'runtime_marker_a') { throw 'old MCP tool disappeared' }
  if ((Get-OpenAPIText) -notmatch [regex]::Escape('/api/v1/capabilities/runtime_marker_a')) { throw 'old OpenAPI path disappeared' }
}

function Assert-NewCapabilityAbsent {
  $tools = Invoke-MCP '{"jsonrpc":"2.0","id":"before-restart-list","method":"tools/list"}'
  if (@($tools.result.tools.name) -contains 'rollout_marker_new') { throw 'new MCP tool appeared before service restart' }
  if ((Get-OpenAPIText) -match [regex]::Escape('/api/v1/capabilities/rollout_marker_new')) { throw 'new OpenAPI path appeared before service restart' }
  $unexpectedSuccess = $false
  try {
    Invoke-WebRequest -UseBasicParsing -Uri http://127.0.0.1:18080/api/v1/capabilities/rollout_marker_new -Method Post `
      -Headers @{Authorization="Bearer $($apiSecret.api_key)"} -ContentType 'application/json' -Body '{}' | Out-Null
    $unexpectedSuccess = $true
  }
  catch {
    if ($_.Exception.Response.StatusCode.value__ -ne 404) { throw }
  }
  if ($unexpectedSuccess) { throw 'new REST capability succeeded before service restart' }
}

  Remove-AgentServiceIfPresent

  Invoke-AgentService @('install') | Out-Null
  $installed = Invoke-AgentService @('status')
  if ($installed -notmatch '(?m)^installed: true$' -or
      $installed -notmatch '(?m)^native: windows-service$' -or
      $installed -notmatch [regex]::Escape("config: $DefaultConfig") -or
      $installed -notmatch [regex]::Escape("binary: $AgentBin")) {
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
  $processId = (Get-CimInstance Win32_Service -Filter "Name='onprest-agent'").ProcessId
  if ($processId -le 0) {
    throw "native service manager did not report a running process ID"
  }
  Start-Sleep -Seconds 1
  $stillRunning = Invoke-AgentService @('status')
  if ($stillRunning -notmatch '(?m)^state: running$') {
    throw "service did not remain running: $stillRunning"
  }

  for ($i = 0; $i -lt 100; $i++) {
    $health = Invoke-RestMethod http://127.0.0.1:18080/healthz
    if ($health.agent_connected) { break }
    Start-Sleep -Milliseconds 50
  }
  if (-not $health.agent_connected) { throw 'Agent did not connect to the test Gateway' }

  # Exercise the documented rollout through the actual Windows Service. An
  # invalid edit must not stop the running service or change its old public
  # contract; a valid edit is loaded only by an explicit stop/start.
  Assert-OldPublicContract
  Assert-NewCapabilityAbsent
  $initialConfigContent = Get-Content $DefaultConfig -Raw
  [System.IO.File]::WriteAllText($DefaultConfig, $initialConfigContent + "`ninvalid_rollout_field: true`n")
  $invalidValidation = & $AgentBin validate --config $DefaultConfig --format json
  if ($LASTEXITCODE -ne 1 -or ($invalidValidation -join "`n") -notmatch '"stage":"config"') {
    throw "invalid rollout edit validation contract failed: $invalidValidation"
  }
  $invalidStatus = Invoke-AgentService @('status')
  if ($invalidStatus -notmatch '(?m)^state: running$') { throw "invalid edit stopped service: $invalidStatus" }
  Assert-OldPublicContract
  Assert-NewCapabilityAbsent

  $newCapability = @'

  rollout_marker_new:
    sql: select 'rollout-v2' as value
    policy:
      readonly: true
    result:
      value:
        type: string
'@
  [System.IO.File]::WriteAllText($DefaultConfig, $initialConfigContent + $newCapability)
  $validValidation = & $AgentBin validate --config $DefaultConfig --format json
  if ($LASTEXITCODE -ne 0 -or ($validValidation -join "`n") -notmatch '"valid":true') { throw "valid rollout edit failed validation: $validValidation" }
  $preRestartStatus = Invoke-AgentService @('status')
  if ($preRestartStatus -notmatch '(?m)^state: running$') { throw "service stopped before rollout restart: $preRestartStatus" }
  Assert-OldPublicContract
  Assert-NewCapabilityAbsent

  Invoke-AgentService @('stop') | Out-Null
  for ($i = 0; $i -lt 40; $i++) {
    $rolloutStopped = Invoke-AgentService @('status')
    if ($rolloutStopped -notmatch '(?m)^state: (running|stop_pending)$') { break }
    Start-Sleep -Milliseconds 250
  }
  if ($rolloutStopped -match '(?m)^state: (running|stop_pending)$') { throw "service did not stop for capability rollout: $rolloutStopped" }
  Invoke-AgentService @('start') | Out-Null
  $newOpenAPI = ''
  for ($i = 0; $i -lt 100; $i++) {
    try { $newOpenAPI = Get-OpenAPIText } catch { $newOpenAPI = '' }
    if ($newOpenAPI -match [regex]::Escape('/api/v1/capabilities/rollout_marker_new')) { break }
    Start-Sleep -Milliseconds 50
  }
  if ($newOpenAPI -notmatch [regex]::Escape('/api/v1/capabilities/rollout_marker_new')) { throw 'new OpenAPI path did not appear after service restart' }
  $newRest = Invoke-RestMethod -Uri http://127.0.0.1:18080/api/v1/capabilities/rollout_marker_new -Method Post `
    -Headers @{Authorization="Bearer $($apiSecret.api_key)"} -ContentType 'application/json' -Body '{}'
  if ($newRest.count -ne 1 -or $newRest.rows[0].value -ne 'rollout-v2') { throw "new REST contract failed: $($newRest | ConvertTo-Json -Compress -Depth 10)" }
  $newTools = Invoke-MCP '{"jsonrpc":"2.0","id":"new-list","method":"tools/list"}'
  if (@($newTools.result.tools.name) -notcontains 'rollout_marker_new') { throw 'new MCP tool did not appear after service restart' }
  $newMCP = Invoke-MCP '{"jsonrpc":"2.0","id":"new-call","method":"tools/call","params":{"name":"rollout_marker_new","arguments":{}}}'
  if ($newMCP.result.isError -eq $true -or $newMCP.result.structuredContent.count -ne 1 -or $newMCP.result.structuredContent.rows[0].value -ne 'rollout-v2') {
    throw "new MCP contract failed: $($newMCP | ConvertTo-Json -Compress -Depth 10)"
  }

  Invoke-RuntimeMarker runtime_marker_a
  Invoke-RuntimeMarker runtime_marker_b

  $validation = & $AgentBin validate --config $DefaultConfig --format json
  if ($LASTEXITCODE -ne 0 -or ($validation -join "`n") -notmatch '"valid":true') {
    throw "validate failed while service was running: $validation"
  }
  $runtimeLog = "$AgentBin.log"
  $rotatedLog = "$runtimeLog.1"
  if (-not (Test-Path $runtimeLog) -or -not (Test-Path $rotatedLog)) {
    throw 'runtime log rotation did not occur'
  }
  if ((Get-Content $runtimeLog -Raw) -notmatch 'runtime_marker_b' -or (Get-Content $rotatedLog -Raw) -notmatch 'runtime_marker_a') {
    throw 'runtime marker records were not preserved across rotation generations'
  }
  $runtimeWriter = Start-Job -ScriptBlock {
    param($Key)
    for ($i = 0; $i -lt 10; $i++) {
      foreach ($capability in @('runtime_marker_a', 'runtime_marker_b')) {
        try {
          Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:18080/api/v1/capabilities/$capability" -Method Post `
            -Headers @{Authorization="Bearer $Key"} -ContentType 'application/json' -Body '{}' | Out-Null
        } catch {
          if ($_.Exception.Response.StatusCode.value__ -ne 400) { throw }
        }
      }
    }
  } -ArgumentList $apiSecret.api_key
  $overlapValidation = & $AgentBin validate --config $DefaultConfig --format json
  if ($LASTEXITCODE -ne 0 -or ($overlapValidation -join "`n") -notmatch '"valid":true') { throw "overlap validate failed: $overlapValidation" }
  Receive-Job -Job (Wait-Job $runtimeWriter) | Out-Null
  Remove-Job $runtimeWriter
  if ((Get-Content $runtimeLog -Raw) -notmatch 'runtime_marker_b' -or (Get-Content $rotatedLog -Raw) -notmatch 'runtime_marker_a') {
    throw 'runtime markers were lost while validate overlapped rotation'
  }
  $postRotationStatus = Invoke-AgentService @('status')
  if ($postRotationStatus -notmatch '(?m)^state: running$') { throw "service stopped during runtime rotation: $postRotationStatus" }
  $runtimeBefore = @((Get-SharedFileHash $runtimeLog), (Get-SharedFileHash $rotatedLog))

  $invalidA = Join-Path (Split-Path $AgentBin) 'capability.validate-a.yaml'
  $invalidB = Join-Path (Split-Path $AgentBin) 'capability.validate-b.yaml'
  (Get-Content $DefaultConfig -Raw).Replace('name: postgres', 'name: validate_missing_database_a') | Set-Content $invalidA
  (Get-Content $DefaultConfig -Raw).Replace('name: postgres', 'name: validate_missing_database_b') | Set-Content $invalidB
  $failureA = & $AgentBin validate --config $invalidA --format json
  if ($LASTEXITCODE -ne 1 -or ($failureA -join "`n") -notmatch '"stage":"database_ping"') {
    throw "validate missing database A contract failed: $failureA"
  }
  $fixedLog = Join-Path (Split-Path $AgentBin) 'onprest-agent.validate.log'
  if ((Get-Content $fixedLog -Raw) -notmatch 'validate_missing_database_a') {
    throw 'first validate detail was not committed'
  }
  $failureB = & $AgentBin validate --config $invalidB --format json
  if ($LASTEXITCODE -ne 1 -or ($failureB -join "`n") -notmatch '"stage":"database_ping"') {
    throw "validate missing database B contract failed: $failureB"
  }
  $latest = Get-Content $fixedLog -Raw
  if ($latest -notmatch 'validate_missing_database_b' -or $latest -match 'validate_missing_database_a') {
    throw 'validate latest-failure replacement contract failed'
  }

  $allowed = @([System.Security.Principal.WindowsIdentity]::GetCurrent().Name, 'NT AUTHORITY\SYSTEM', 'BUILTIN\Administrators')
  $blockingConfig = Join-Path (Split-Path $AgentBin) 'capability.validate-blocking.yaml'
  (Get-Content $DefaultConfig -Raw).Replace('port: 5432', 'port: 18081') | Set-Content $blockingConfig
  $blackhole = Start-Job -ScriptBlock {
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 18081)
    $listener.Start()
    try { $client = $listener.AcceptTcpClient(); Start-Sleep -Seconds 15; $client.Dispose() } finally { $listener.Stop() }
  }
  Start-Sleep -Milliseconds 500
  $blockingValidation = Start-Process -FilePath $AgentBin -ArgumentList @('validate', '--config', $blockingConfig, '--format', 'json') `
    -RedirectStandardOutput "$blockingConfig.out" -RedirectStandardError "$blockingConfig.err" -PassThru -WindowStyle Hidden
  $temporaryLog = $null
  for ($i = 0; $i -lt 100; $i++) {
    $temporaryLog = Get-ChildItem (Split-Path $AgentBin) -Filter '.onprest-agent.validate.*.tmp' -File | Select-Object -First 1
    if ($null -ne $temporaryLog) { break }
    Start-Sleep -Milliseconds 50
  }
  if ($null -eq $temporaryLog) { throw 'production validate did not expose a live temporary file for DACL verification' }
  foreach ($privatePath in @($fixedLog, $temporaryLog.FullName, (Join-Path (Split-Path $AgentBin) '.onprest-agent.validate.lock'))) {
    $acl = Get-Acl $privatePath
    if (-not $acl.AreAccessRulesProtected) { throw "$privatePath inherits its DACL" }
    foreach ($rule in $acl.Access) {
      if ($rule.AccessControlType -eq 'Allow' -and $allowed -notcontains $rule.IdentityReference.Value) {
        throw "$privatePath grants access to $($rule.IdentityReference.Value)"
      }
    }
  }
  # Windows local account names are limited to 20 characters. A random suffix
  # avoids colliding with an account left by an interrupted previous run.
  $readerName = "OnprestVal$([Guid]::NewGuid().ToString('N').Substring(0, 10))"
  $readerPasswordPlain = 'Onprest-Validate-Reader-42!'
  & net.exe user $readerName $readerPasswordPlain /add 2>$null | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "failed to create test reader account: exit $LASTEXITCODE" }
  $readerCreated = $true
  try {
    $readerPassword = ConvertTo-SecureString $readerPasswordPlain -AsPlainText -Force
    $credential = [pscredential]::new("$env:COMPUTERNAME\$readerName", $readerPassword)
    foreach ($privatePath in @($fixedLog, $temporaryLog.FullName, (Join-Path (Split-Path $AgentBin) '.onprest-agent.validate.lock'))) {
      $probe = Start-Process -FilePath cmd.exe -ArgumentList @('/c', 'type', $privatePath) -Credential $credential -WindowStyle Hidden -Wait -PassThru
      if ($probe.ExitCode -eq 0) { throw "non-privileged user read $privatePath" }
    }
  }
  finally {
    if ($readerCreated) {
      & net.exe user $readerName /delete 2>$null | Out-Null
      if ($LASTEXITCODE -eq 0) {
        $readerCreated = $false
      }
      else {
        Write-Warning "test reader cleanup failed; final cleanup will retry: exit $LASTEXITCODE"
      }
    }
    if (-not $blockingValidation.HasExited) {
      Stop-Process -Id $blockingValidation.Id -Force
      $blockingValidation.WaitForExit()
    }
    Stop-Job $blackhole -ErrorAction SilentlyContinue
    Remove-Job $blackhole -Force -ErrorAction SilentlyContinue
  }
  $lockFile = Join-Path (Split-Path $AgentBin) '.onprest-agent.validate.lock'
  $fixedHashBeforeBusy = Get-SharedFileHash $fixedLog
  $temporaryBeforeBusy = @((Get-ChildItem (Split-Path $AgentBin) -Filter '.onprest-agent.validate.*.tmp' -File).FullName)
  $lockStream = [System.IO.FileStream]::new(
    $lockFile,
    [System.IO.FileMode]::Open,
    [System.IO.FileAccess]::ReadWrite,
    ([System.IO.FileShare]::ReadWrite -bor [System.IO.FileShare]::Delete)
  )
  try {
    $lockStream.Lock(0, 1)
    $busy = & $AgentBin validate --config $DefaultConfig --format json
    if ($LASTEXITCODE -ne 1 -or ($busy -join "`n") -notmatch '"stage":"busy"') {
      throw "second validate did not report busy: $busy"
    }
    if ((Get-SharedFileHash $fixedLog) -ne $fixedHashBeforeBusy) {
      throw 'busy validation changed the completed failure log'
    }
    $temporaryAfterBusy = @((Get-ChildItem (Split-Path $AgentBin) -Filter '.onprest-agent.validate.*.tmp' -File).FullName)
    if (($temporaryBeforeBusy -join ',') -ne ($temporaryAfterBusy -join ',')) {
      throw 'busy validation changed the temporary-file set'
    }
  }
  finally {
    try { $lockStream.Unlock(0, 1) } catch {}
    $lockStream.Dispose()
  }
  $success = & $AgentBin validate --config $DefaultConfig
  if ($LASTEXITCODE -ne 0 -or (Test-Path $fixedLog)) { throw "validate success cleanup failed: $success" }
  $runtimeAfter = @((Get-SharedFileHash $runtimeLog), (Get-SharedFileHash $rotatedLog))
  if (($runtimeBefore -join ',') -ne ($runtimeAfter -join ',')) { throw 'validate changed runtime logs' }
  Remove-Item $invalidA, $invalidB -Force

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
catch {
  $primaryFailure = $_
}
finally {
  try {
    Remove-AgentServiceIfPresent
  }
  catch {
    Write-Warning "service cleanup failed: $_"
  }
  if ($null -ne $blockingValidation -and -not $blockingValidation.HasExited) {
    Stop-Process -Id $blockingValidation.Id -Force -ErrorAction SilentlyContinue
    $blockingValidation.WaitForExit()
  }
  foreach ($job in @($blackhole, $runtimeWriter)) {
    if ($null -ne $job) {
      Stop-Job $job -ErrorAction SilentlyContinue
      Remove-Job $job -Force -ErrorAction SilentlyContinue
    }
  }
  if ($null -ne $gatewayProcess -and -not $gatewayProcess.HasExited) {
    Stop-Process -Id $gatewayProcess.Id -Force
    $gatewayProcess.WaitForExit()
  }
  if ($readerCreated) {
    & net.exe user $readerName /delete 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) {
      $cleanupFailure = "final test reader cleanup failed: exit $LASTEXITCODE"
      if ($null -ne $primaryFailure) {
        Write-Warning $cleanupFailure
      }
    }
    else {
      $readerCreated = $false
    }
  }
  Remove-Item -Force -ErrorAction SilentlyContinue @(
    $DefaultConfig,
    "$gatewayOutput", "$gatewayOutput.stderr",
    (Join-Path $artifactDir 'capability.validate-a.yaml'), (Join-Path $artifactDir 'capability.validate-b.yaml'),
    (Join-Path $artifactDir 'capability.validate-blocking.yaml'),
    (Join-Path $artifactDir 'capability.validate-blocking.yaml.out'), (Join-Path $artifactDir 'capability.validate-blocking.yaml.err'),
    (Join-Path $artifactDir 'onprest-agent.validate.log'), (Join-Path $artifactDir '.onprest-agent.validate.lock'),
    "$AgentBin.log", "$AgentBin.log.1", "$AgentBin.log.2"
  )
  Get-ChildItem $artifactDir -Filter '.onprest-agent.validate.*.tmp' -File -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
  [Environment]::SetEnvironmentVariable('GATEWAY_ADDR', $previousGatewayAddr, 'Process')
  [Environment]::SetEnvironmentVariable('GATEWAY_AGENT_PUBLIC_KEY', $previousAgentPublicKey, 'Process')
  [Environment]::SetEnvironmentVariable('GATEWAY_API_KEYS_JSON', $previousAPIKeys, 'Process')
  [Environment]::SetEnvironmentVariable('GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND', $previousRateLimitRPS, 'Process')
  [Environment]::SetEnvironmentVariable('GATEWAY_RATE_LIMIT_BURST', $previousRateLimitBurst, 'Process')
  $apiSecret = $null
  $agentSecret = $null
}
if ($null -ne $primaryFailure) {
  throw $primaryFailure
}
if ($null -ne $cleanupFailure) {
  throw $cleanupFailure
}
