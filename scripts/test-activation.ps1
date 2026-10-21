# Test fonctionnel de ricochet-on.ps1 / ricochet-off.ps1.
#   pwsh -NoProfile -File scripts/test-activation.ps1
# Compile ricochet dans un dossier temporaire puis vérifie : démarrage,
# variables proxy, amont, masquage des identifiants, idempotence, arrêt et
# restauration.
Set-StrictMode -Version 2
$ErrorActionPreference = 'Stop'

function Fail([string]$Message) { throw "FAIL: $Message" }

$onWindows = ($PSVersionTable.PSEdition -eq 'Desktop') -or $IsWindows
$repo = Split-Path -Parent $PSScriptRoot
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("ricochet-test-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
$exe = if ($onWindows) { 'ricochet.exe' } else { 'ricochet' }

try {
    Push-Location $repo
    try { & go build -o (Join-Path $tmp $exe) ./cmd/ricochet } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { Fail 'go build' }
    Copy-Item (Join-Path $PSScriptRoot 'ricochet-on.ps1'), (Join-Path $PSScriptRoot 'ricochet-off.ps1') $tmp

    $env:RICOCHET_PORT = '39129'
    $env:RICOCHET_UI_PORT = '39130'
    $original = 'http://alice:s3cret@127.0.0.1:9'
    $env:HTTP_PROXY = $original
    $env:HTTPS_PROXY = $original
    if (-not $onWindows) { Remove-Item Env:http_proxy, Env:https_proxy -ErrorAction SilentlyContinue }
    $want = 'http://127.0.0.1:39129'

    & (Join-Path $tmp 'ricochet-on.ps1')
    if ($LASTEXITCODE -ne 0) { Fail 'ricochet-on a échoué' }
    $names = if ($onWindows) { @('HTTP_PROXY', 'HTTPS_PROXY') } else { @('http_proxy', 'https_proxy', 'HTTP_PROXY', 'HTTPS_PROXY') }
    foreach ($n in $names) {
        $v = [Environment]::GetEnvironmentVariable($n, 'Process')
        if ($v -ne $want) { Fail "$n=$v, attendu $want" }
    }
    if (-not (Test-Path (Join-Path $tmp 'ricochet-rules.txt'))) { Fail 'fichier de règles non créé' }
    $procId = [int](Get-Content (Join-Path $tmp 'ricochet.pid') -Raw).Trim()
    if (-not (Get-Process -Id $procId -ErrorAction SilentlyContinue)) { Fail 'processus absent' }

    # -NoProxy n'existe qu'à partir de PowerShell 6 ; la 5.1 ignore HTTP_PROXY.
    $irm = @{ Uri = 'http://127.0.0.1:39130/api/info' }
    if ($PSVersionTable.PSVersion.Major -ge 6) { $irm.NoProxy = $true }
    $info = Invoke-RestMethod @irm
    if ($info.amont -ne '127.0.0.1:9') { Fail "amont inattendu: $($info.amont)" }
    if (($info | ConvertTo-Json) -match 's3cret') { Fail 'identifiants dans le dashboard' }
    if ((Get-Content (Join-Path $tmp 'ricochet.log') -Raw) -match 's3cret') { Fail 'identifiants dans les logs' }

    # Deuxième activation : idempotente, l'amont mémorisé est conservé.
    & (Join-Path $tmp 'ricochet-on.ps1') 6>&1 | Out-Null
    if ($env:RICOCHET_SAVED_HTTPS_PROXY -ne $original) { Fail 'amont mémorisé écrasé' }
    if ([int](Get-Content (Join-Path $tmp 'ricochet.pid') -Raw).Trim() -ne $procId) { Fail 'second processus lancé' }

    & (Join-Path $tmp 'ricochet-off.ps1')
    if ($env:HTTPS_PROXY -ne $original) { Fail "HTTPS_PROXY non restauré: $env:HTTPS_PROXY" }
    if ($env:HTTP_PROXY -ne $original) { Fail 'HTTP_PROXY non restauré' }
    if (-not $onWindows -and ($null -ne [Environment]::GetEnvironmentVariable('http_proxy', 'Process'))) { Fail 'http_proxy aurait dû être supprimée' }
    if ($null -ne $env:RICOCHET_SAVED_VARS) { Fail 'état non nettoyé' }
    Start-Sleep -Milliseconds 300
    if (Get-Process -Id $procId -ErrorAction SilentlyContinue) { Fail 'processus toujours actif' }
    if (Test-Path (Join-Path $tmp 'ricochet.pid')) { Fail 'pidfile non supprimé' }

    $out = & (Join-Path $tmp 'ricochet-off.ps1') 6>&1 | Out-String
    if ($out -notmatch "n'était pas") { Fail "message d'arrêt à vide absent" }
    Write-Host 'OK (test-activation.ps1)'
} finally {
    $pidFile = Join-Path $tmp 'ricochet.pid'
    if (Test-Path $pidFile) { Stop-Process -Id ([int](Get-Content $pidFile -Raw).Trim()) -Force -ErrorAction SilentlyContinue }
    Start-Sleep -Milliseconds 300
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
