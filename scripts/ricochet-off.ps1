<#
.SYNOPSIS
    Arrête ricochet et restaure la configuration proxy mémorisée par
    ricochet-on.ps1 dans la session PowerShell courante.

.EXAMPLE
    & "$HOME\.ricochet\ricochet-off.ps1"
#>
[CmdletBinding()]
param()

Set-StrictMode -Version 2
$ErrorActionPreference = 'Stop'

$onWindows = ($PSVersionTable.PSEdition -eq 'Desktop') -or $IsWindows
$dir = $PSScriptRoot
$pidFile = Join-Path $dir 'ricochet.pid'
$exe = if ($onWindows) { 'ricochet.exe' } else { 'ricochet' }
$bin = if ($env:RICOCHET_BIN) { $env:RICOCHET_BIN } else { Join-Path $dir $exe }
$uiPort = if ($env:RICOCHET_UI_PORT) { $env:RICOCHET_UI_PORT } else { '3130' }
$port = if ($env:RICOCHET_PORT) { $env:RICOCHET_PORT } else { '3129' }
$proxyUrl = if ($env:RICOCHET_ACTIVE) { $env:RICOCHET_ACTIVE } else { "http://127.0.0.1:$port" }
$names = if ($onWindows) { @('HTTP_PROXY', 'HTTPS_PROXY') } else { @('http_proxy', 'https_proxy', 'HTTP_PROXY', 'HTTPS_PROXY') }

function Get-Var([string]$Name) { [Environment]::GetEnvironmentVariable($Name, 'Process') }
# $null deviendrait "" (variable vide mais définie hors Windows) : NullString la supprime.
function Set-Var([string]$Name, $Value) {
    if ($null -eq $Value) { $Value = [NullString]::Value }
    [Environment]::SetEnvironmentVariable($Name, $Value, 'Process')
}
function Hide-Credentials([string]$Url) { $Url -replace '(://)[^/@]*@', '$1***@' }

# Arrêt propre via le dashboard (« ricochet --stop ») : marche quel que soit le
# terminal qui a lancé ricochet (PowerShell, Git Bash...).
$stoppedPid = $null
if (Test-Path $bin) {
    $previousPref = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { $lines = & $bin --dashboard "127.0.0.1:$uiPort" --stop } finally { $ErrorActionPreference = $previousPref }
    if ($LASTEXITCODE -eq 0) {
        foreach ($line in $lines) { if ($line -like 'pid=*') { $stoppedPid = $line.Substring(4) } }
    }
}
if ($stoppedPid) {
    Write-Host "■ ricochet arrêté (pid $stoppedPid)" -ForegroundColor Green
} else {
    # Repli : instance sans dashboard joignable, arrêtée par son pid.
    $proc = $null
    if (Test-Path $pidFile) {
        $id = (Get-Content $pidFile -Raw).Trim()
        if ($id) {
            $p = Get-Process -Id ([int]$id) -ErrorAction SilentlyContinue
            if ($p -and $p.ProcessName -like 'ricochet*') { $proc = $p }
        }
    }
    if ($proc) {
        Stop-Process -Id $proc.Id -Force
        $proc.WaitForExit(5000) | Out-Null
        Write-Host "■ ricochet arrêté (pid $($proc.Id))" -ForegroundColor Green
    } else {
        Write-Host "● ricochet n'était pas en cours d'exécution" -ForegroundColor DarkGray
    }
}
Remove-Item $pidFile -ErrorAction SilentlyContinue

# Restauration des variables proxy.
$restored = $false
if ($null -ne (Get-Var 'RICOCHET_SAVED_VARS')) {
    $saved = (Get-Var 'RICOCHET_SAVED_VARS') -split ',' | Where-Object { $_ }
    foreach ($n in $names) { Set-Var $n $null }
    foreach ($n in $saved) {
        Set-Var $n (Get-Var "RICOCHET_SAVED_$n")
        Set-Var "RICOCHET_SAVED_$n" $null
    }
    Set-Var 'RICOCHET_SAVED_VARS' $null
    $restored = $true
} else {
    # Session non activée par ricochet-on : on retire seulement ce qui pointe sur ricochet.
    foreach ($n in $names) {
        if ((Get-Var $n) -eq $proxyUrl) { Set-Var $n $null; $restored = $true }
    }
}
Set-Var 'RICOCHET_ACTIVE' $null

if ($restored) {
    Write-Host '  configuration proxy restaurée :'
    foreach ($n in $names) {
        $v = Get-Var $n
        if ($null -ne $v) {
            Write-Host ('    {0,-11} = {1}' -f $n, (Hide-Credentials $v))
        } else {
            Write-Host ('    {0,-11} (non définie)' -f $n) -ForegroundColor DarkGray
        }
    }
} else {
    Write-Host '  variables proxy inchangées' -ForegroundColor DarkGray
}
Write-Host '  les autres sessions activées doivent aussi lancer ricochet-off' -ForegroundColor DarkGray
$global:LASTEXITCODE = 0
