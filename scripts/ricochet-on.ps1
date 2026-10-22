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

.PARAMETER Restart
    Relance ricochet s'il tourne déjà, pour appliquer de nouvelles options.

.PARAMETER Mitm
    Déchiffre tout le HTTPS (CA dans <dossier du script>\ca\ca.pem).

.PARAMETER MitmHosts
    Déchiffre seulement ces hosts (ex. '*.exemple.com', 'api.foo.com').

.PARAMETER CaptureBodies
    Conserve jusqu'à N octets des corps de requêtes et réponses.

.PARAMETER Bypass
    Hosts qui ne passent pas par le proxy amont.

.PARAMETER NoKeepAlive
    Connexions neuves : timings DNS / connexion / TLS à chaque requête.

.PARAMETER RicochetArgs
    Toute autre option de ricochet, telle quelle (ex. --verbose, --db f.db).

.EXAMPLE
    & "$HOME\.ricochet\ricochet-on.ps1"

.EXAMPLE
    & "$HOME\.ricochet\ricochet-on.ps1" -MitmHosts '*.exemple.com' -CaptureBodies 4096

.EXAMPLE
    & "$HOME\.ricochet\ricochet-on.ps1" -Restart -Mitm --verbose
#>
[CmdletBinding(PositionalBinding = $false)]
param(
    [switch]$Restart,
    [switch]$Mitm,
    [string[]]$MitmHosts,
    [int]$CaptureBodies,
    [string[]]$Bypass,
    [switch]$NoKeepAlive,
    [switch]$Help,
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$RicochetArgs
)

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

# Interroge le dashboard d'une instance en cours (« ricochet --ping ») :
# fiable quel que soit le terminal (PowerShell, Git Bash...) qui l'a lancée.
# Renvoie une table pid/upstream/... ou $null.
function Get-RunningInstance {
    $previousPref = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { $lines = & $bin --dashboard "127.0.0.1:$uiPort" --ping } finally { $ErrorActionPreference = $previousPref }
    if ($LASTEXITCODE -ne 0) { return $null }
    $info = @{}
    foreach ($line in $lines) {
        $k, $v = $line -split '=', 2
        if ($k) { $info[$k] = $v }
    }
    return $info
}

# hôte:port d'une URL de proxy, sans identifiants (port 80 par défaut, comme ricochet).
function Get-HostPort([string]$Url) {
    if (-not $Url) { return '' }
    $hp = $Url -replace '^[A-Za-z][A-Za-z0-9+.-]*://', '' -replace '^[^@/]*@', '' -replace '/.*$', ''
    if ($hp -notmatch ':') { $hp += ':80' }
    return $hp
}

# Lit le log même s'il est ouvert en écriture par ricochet.
function Read-Log {
    try {
        $fs = [IO.File]::Open($log, 'Open', 'Read', 'ReadWrite')
        try { (New-Object IO.StreamReader($fs)).ReadToEnd() } finally { $fs.Dispose() }
    } catch { '' }
}

if ($Help) {
    Get-Help $PSCommandPath -Detailed | Out-String | Write-Host
    $global:LASTEXITCODE = 0
    return
}

# Options transmises à ricochet : paramètres PowerShell, puis options brutes.
$extra = @()
if ($Mitm) { $extra += '--mitm' }
if ($MitmHosts) { $extra += '--mitm-hosts', ($MitmHosts -join ',') }
if ($PSBoundParameters.ContainsKey('CaptureBodies')) { $extra += '--capture-bodies', "$CaptureBodies" }
if ($Bypass) { $extra += '--bypass', ($Bypass -join ',') }
if ($NoKeepAlive) { $extra += '--no-keepalive' }
$hasCa = $false
foreach ($a in @($RicochetArgs | Where-Object { $null -ne $_ })) {
    $name = ($a -replace '^--?', '') -replace '=.*$', ''
    if ($a -like '-*') {
        if (@('listen', 'dashboard', 'rules', 'upstream') -contains $name) {
            Write-Host "✖ $a est géré par le script : utilisez `$env:RICOCHET_PORT, `$env:RICOCHET_UI_PORT," -ForegroundColor Red
            Write-Host '  $env:RICOCHET_RULES ou HTTPS_PROXY (amont) avant ricochet-on.' -ForegroundColor Red
            $global:LASTEXITCODE = 2
            return
        }
        if ($name -eq 'upstream-auth') {
            Write-Host '✖ --upstream-auth apparaîtrait dans la ligne de commande du processus : utilisez' -ForegroundColor Red
            Write-Host '  HTTPS_PROXY=http://user:pass@hôte:port ou $env:RICOCHET_UPSTREAM_AUTH.' -ForegroundColor Red
            $global:LASTEXITCODE = 2
            return
        }
        if ($name -eq 'ca-dir') { $hasCa = $true }
    }
    $extra += $a
}
$userArgs = @($extra)
# La CA du MITM reste dans le dossier d'installation (et non dans le dossier
# courant, défaut de ricochet).
if (-not $hasCa) { $extra += '--ca-dir', (Join-Path $dir 'ca') }

# Start-Process concatène les arguments : on protège ceux qui contiennent des
# espaces ou sont vides.
function ConvertTo-Arg([string]$Value) {
    if ($Value -eq '' -or $Value -match '\s') { return '"' + $Value + '"' }
    return $Value
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

$shownUp = Get-HostPort $upstream
$running = Get-RunningInstance
$proc = Get-RicochetProcess
if ($Restart -and ($running -or $proc)) {
    # -Restart : arrêt de l'instance en cours pour appliquer les options.
    $stoppedPid = if ($running) { $running.pid } else { $proc.Id }
    $previousPref = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { & $bin --dashboard "127.0.0.1:$uiPort" --stop | Out-Null } finally { $ErrorActionPreference = $previousPref }
    if ($LASTEXITCODE -ne 0 -and $proc) {
        Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        $proc.WaitForExit(5000) | Out-Null
    }
    Write-Host "■ ricochet arrêté (pid $stoppedPid) pour redémarrer avec les nouvelles options" -ForegroundColor Green
    Remove-Item $pidFile -ErrorAction SilentlyContinue
    $running = $null
    $proc = $null
}
if ($running) {
    Write-Host "● ricochet tourne déjà (pid $($running.pid))" -ForegroundColor Green
    if (-not $proc -or ("$($proc.Id)" -ne $running.pid)) {
        Write-Host "  lancé depuis un autre terminal : cette session l'utilise sans le relancer" -ForegroundColor DarkGray
    }
    $runUp = if ($running.upstream -eq 'none (direct)') { '' } else { "$($running.upstream)" }
    if ($runUp -ne $shownUp) {
        $a = if ($runUp) { $runUp } else { 'aucun' }
        $b = if ($shownUp) { $shownUp } else { 'aucun' }
        Write-Host "! l'instance en cours utilise l'amont « $a » et non « $b » ;" -ForegroundColor Yellow
        Write-Host '  pour changer : ricochet-on -Restart' -ForegroundColor Yellow
    }
    if ($userArgs.Count -gt 0) {
        Write-Host "! options ignorées, ricochet tourne déjà : $($userArgs -join ' ')" -ForegroundColor Yellow
        Write-Host "  pour les appliquer : ricochet-on -Restart ..." -ForegroundColor Yellow
    }
    $shownUp = $runUp
} elseif ($proc) {
    # Processus présent mais dashboard muet (démarrage en cours).
    Write-Host "● ricochet tourne déjà (pid $($proc.Id)), dashboard pas encore joignable" -ForegroundColor Yellow
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

    $arguments = @('--listen', "127.0.0.1:$port", '--dashboard', "127.0.0.1:$uiPort", '--rules', (ConvertTo-Arg $rules))
    $arguments += @($extra | ForEach-Object { ConvertTo-Arg $_ })
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
            Write-Host "✖ ricochet n'a pas démarré." -ForegroundColor Red
            # « listen tcp ...: bind: » : port pris (message système traduit selon la langue).
            if ((Read-Log) -match 'listen tcp [^ ]+: bind:') {
                Write-Host "  le port $port ou $uiPort est déjà utilisé par un autre programme"
                Write-Host '  (autres ports : $env:RICOCHET_PORT / $env:RICOCHET_UI_PORT)'
            }
            Write-Host "  dernières lignes de ${log} :"
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
if ($shownUp) {
    Write-Host "  amont     : $shownUp"
} else {
    Write-Host '  amont     : aucun (connexions directes)'
}
$countText = if ($ruleCount) { " ($($ruleCount.Groups[1].Value) règle(s))" } else { '' }
Write-Host "  règles    : $rules$countText"
# État MITM de l'instance (lancée ici ou ailleurs).
$status = Get-RunningInstance
$mitmScope = if ($status -and $status.ContainsKey('mitm')) { $status.mitm } else { '' }
switch ($mitmScope) {
    '' { Write-Host '  mitm      : désactivé (ricochet-on -Restart -Mitm ou -MitmHosts ...)' -ForegroundColor DarkGray }
    'all' { Write-Host '  mitm      : tous les hosts' -ForegroundColor Yellow }
    default { Write-Host "  mitm      : $mitmScope" -ForegroundColor Yellow }
}
if ($mitmScope -and $status.ContainsKey('ca') -and $status.ca) {
    Write-Host "  CA        : $($status.ca)"
    # SSL_CERT_FILE est ignoré par la plupart des outils sous Windows.
    $curlHint = if ($onWindows) { "curl --cacert `"$($status.ca)`" ..." } else { "curl --cacert `"$($status.ca)`" ...   ou   `$env:SSL_CERT_FILE = `"$($status.ca)`"" }
    Write-Host "              $curlHint" -ForegroundColor DarkGray
    # kubectl/helm (Go) ignorent SSL_CERT_FILE dès que la kubeconfig fournit la CA du cluster.
    Write-Host "              kubectl --certificate-authority `"$($status.ca)`" ...  (pas avec une auth par certificat client)" -ForegroundColor DarkGray
}
Write-Host "  logs      : $log"
Write-Host "  arrêt     : & `"$(Join-Path $dir 'ricochet-off.ps1')`"" -ForegroundColor DarkGray
$global:LASTEXITCODE = 0
