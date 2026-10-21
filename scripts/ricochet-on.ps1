<#
.SYNOPSIS
    Démarre ricochet en arrière-plan et fait pointer HTTP_PROXY / HTTPS_PROXY
    sur lui pour la session PowerShell courante.

.DESCRIPTION
    Le proxy configuré avant l'activation devient le proxy amont de ricochet.
    Les règles sont lues dans ricochet-rules.txt (créé s'il n'existe pas).

    Variables optionnelles :
      RICOCHET_PORT     port du proxy        (3129)
      RICOCHET_UI_PORT  port du dashboard    (3130)
      RICOCHET_RULES    fichier de règles    (<dossier du script>\ricochet-rules.txt)
      RICOCHET_BIN      binaire ricochet     (<dossier du script>\ricochet.exe)
      RICOCHET_ARGS     options supplémentaires (ex. "--mitm-hosts *.exemple.com")

.EXAMPLE
    & "$HOME\.ricochet\ricochet-on.ps1"
#>
[CmdletBinding()]
param()

Set-StrictMode -Version 2
$ErrorActionPreference = 'Stop'

$onWindows = ($PSVersionTable.PSEdition -eq 'Desktop') -or $IsWindows
$dir = $PSScriptRoot
$exe = if ($onWindows) { 'ricochet.exe' } else { 'ricochet' }
$bin = if ($env:RICOCHET_BIN) { $env:RICOCHET_BIN } else { Join-Path $dir $exe }
$port = if ($env:RICOCHET_PORT) { $env:RICOCHET_PORT } else { '3129' }
$uiPort = if ($env:RICOCHET_UI_PORT) { $env:RICOCHET_UI_PORT } else { '3130' }
$rules = if ($env:RICOCHET_RULES) { $env:RICOCHET_RULES } else { Join-Path $dir 'ricochet-rules.txt' }
$pidFile = Join-Path $dir 'ricochet.pid'
$log = Join-Path $dir 'ricochet.log'
$proxyUrl = "http://127.0.0.1:$port"
# Les noms de variables sont insensibles à la casse sous Windows.
$names = if ($onWindows) { @('HTTP_PROXY', 'HTTPS_PROXY') } else { @('http_proxy', 'https_proxy', 'HTTP_PROXY', 'HTTPS_PROXY') }

function Get-Var([string]$Name) { [Environment]::GetEnvironmentVariable($Name, 'Process') }
# $null deviendrait "" (variable vide mais définie hors Windows) : NullString la supprime.
function Set-Var([string]$Name, $Value) {
    if ($null -eq $Value) { $Value = [NullString]::Value }
    [Environment]::SetEnvironmentVariable($Name, $Value, 'Process')
}
function Hide-Credentials([string]$Url) { $Url -replace '(://)[^/@]*@', '$1***@' }

function Get-RicochetProcess {
    if (-not (Test-Path $pidFile)) { return $null }
    $id = (Get-Content $pidFile -Raw).Trim()
    if (-not $id) { return $null }
    $p = Get-Process -Id ([int]$id) -ErrorAction SilentlyContinue
    if ($p -and $p.ProcessName -like 'ricochet*') { return $p }
    return $null
}

# Lit le log même s'il est ouvert en écriture par ricochet.
function Read-Log {
    try {
        $fs = [IO.File]::Open($log, 'Open', 'Read', 'ReadWrite')
        try { (New-Object IO.StreamReader($fs)).ReadToEnd() } finally { $fs.Dispose() }
    } catch { '' }
}

if (-not (Test-Path $bin)) {
    Write-Host "✖ binaire ricochet introuvable : $bin" -ForegroundColor Red
    Write-Host '  installez-le avec « make install » depuis le dépôt ricochet.'
    $global:LASTEXITCODE = 1
    return
}

# Mémorise la configuration proxy d'origine (une seule fois par session).
if ((Get-Var 'RICOCHET_ACTIVE') -ne $proxyUrl) {
    $saved = @()
    foreach ($n in $names) {
        $v = Get-Var $n
        if ($null -ne $v) {
            Set-Var "RICOCHET_SAVED_$n" $v
            $saved += $n
        }
    }
    Set-Var 'RICOCHET_SAVED_VARS' ($saved -join ',')
}

# Proxy amont : HTTPS_PROXY en priorité, puis HTTP_PROXY.
$upstream = ''
foreach ($n in @('https_proxy', 'HTTPS_PROXY', 'http_proxy', 'HTTP_PROXY')) {
    if ($names -cnotcontains $n) { continue }
    $v = Get-Var "RICOCHET_SAVED_$n"
    if ($v) { $upstream = $v; break }
}
if ($upstream -match "(127\.0\.0\.1|localhost):$port\b") {
    Write-Host '! le proxy actuel pointe déjà sur ricochet : pas de proxy amont' -ForegroundColor Yellow
    $upstream = ''
}
$httpUp = if (Get-Var 'RICOCHET_SAVED_http_proxy') { Get-Var 'RICOCHET_SAVED_http_proxy' } else { Get-Var 'RICOCHET_SAVED_HTTP_PROXY' }
if ($httpUp -and $upstream -and ($httpUp -ne $upstream)) {
    Write-Host "! HTTP_PROXY et HTTPS_PROXY diffèrent : ricochet utilise $(Hide-Credentials $upstream) comme amont unique" -ForegroundColor Yellow
}

$proc = Get-RicochetProcess
if ($proc) {
    Write-Host "● ricochet tourne déjà (pid $($proc.Id))" -ForegroundColor Green
} else {
    Remove-Item $pidFile -ErrorAction SilentlyContinue
    if (-not (Test-Path $rules)) {
        @(
            '# Règles ricochet : FQDN → IP (rechargées à chaud, sans redémarrer).'
            '#'
            '#   <ip>[:port]        <host> [<host>...]'
            '#'
            '# Exemples :'
            '# 10.1.2.3             api.exemple.com'
            '# 10.1.2.4             *.interne.exemple.com     # sous-domaines uniquement'
            '# 10.1.2.5:8443        svc.exemple.com           # override du port'
            '# 2001:db8::1          v6.exemple.com'
        ) | Set-Content -Path $rules -Encoding UTF8
        Write-Host "+ fichier de règles créé : $rules" -ForegroundColor DarkGray
    }

    $arguments = @('--listen', "127.0.0.1:$port", '--dashboard', "127.0.0.1:$uiPort", '--rules', "`"$rules`"")
    if ($env:RICOCHET_ARGS) { $arguments += $env:RICOCHET_ARGS }
    $start = @{
        FilePath               = $bin
        ArgumentList           = $arguments
        RedirectStandardError  = $log
        PassThru               = $true
    }
    if ($onWindows) { $start.WindowStyle = 'Hidden' }

    # RICOCHET_UPSTREAM plutôt que --upstream : les identifiants éventuels
    # n'apparaissent pas dans la ligne de commande du processus.
    $previous = $env:RICOCHET_UPSTREAM
    $env:RICOCHET_UPSTREAM = $upstream
    try { $proc = Start-Process @start } finally { $env:RICOCHET_UPSTREAM = $previous }
    Set-Content -Path $pidFile -Value $proc.Id

    # Attend le message de démarrage (5 s max).
    $deadline = (Get-Date).AddSeconds(5)
    while ((Read-Log) -notmatch 'msg="ricochet started"') {
        if ($proc.HasExited -or (Get-Date) -gt $deadline) {
            Write-Host "✖ ricochet n'a pas démarré. Dernières lignes de ${log} :" -ForegroundColor Red
            (Read-Log) -split "`r?`n" | Where-Object { $_ } | Select-Object -Last 5 | ForEach-Object { Write-Host "  $_" }
            if (-not $proc.HasExited) { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue }
            Remove-Item $pidFile -ErrorAction SilentlyContinue
            $global:LASTEXITCODE = 1
            return
        }
        Start-Sleep -Milliseconds 100
    }
    Write-Host "▶ ricochet démarré (pid $($proc.Id))" -ForegroundColor Green
}

foreach ($n in $names) { Set-Var $n $proxyUrl }
Set-Var 'RICOCHET_ACTIVE' $proxyUrl

$ruleCount = ([regex]::Matches((Read-Log), 'rule_count=(\d+)') | Select-Object -Last 1)
Write-Host "  proxy     : $proxyUrl  ($($names -join ', '))"
Write-Host "  dashboard : http://127.0.0.1:$uiPort/"
if ($upstream) {
    Write-Host "  amont     : $(Hide-Credentials $upstream)"
} else {
    Write-Host '  amont     : aucun (connexions directes)'
}
$countText = if ($ruleCount) { " ($($ruleCount.Groups[1].Value) règle(s))" } else { '' }
Write-Host "  règles    : $rules$countText"
Write-Host "  logs      : $log"
Write-Host "  arrêt     : & `"$(Join-Path $dir 'ricochet-off.ps1')`"" -ForegroundColor DarkGray
$global:LASTEXITCODE = 0
