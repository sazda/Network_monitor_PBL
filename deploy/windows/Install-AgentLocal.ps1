<#
.SYNOPSIS
  Instalira (ili nadograđuje) Network Monitor agenta NA OVOM računaru, bez
  WinRM-a. Ako korisnik nije administrator, traži administratorski nalog.

.DESCRIPTION
  Za računare na koje konzola ne može da instalira agenta (WinRM isključen,
  VPN...). MSI i ova skripta se prebace na računar (npr. skriptom
  Copy-AgentToComputer.ps1 u C:\Temp\NetworkMonitor), pa se skripta pokrene
  na samom računaru – lično ili preko udaljene pomoći.

  Ako PowerShell nije pokrenut kao administrator, skripta traži
  administratorski nalog (Get-Credential), pokreće se ponovo pod tim nalogom
  i traži potvrdu UAC-a. Token za agente se upisuje tek u administratorskom
  prozoru (skriven unos) i ne prosleđuje se kroz komandnu liniju.

.PARAMETER MsiPath
  Putanja do NetworkMonitorAgent.msi (podrazumevano: pored ove skripte).

.PARAMETER ConsoleUrl
  Adresa konzole, npr. https://netmon.firma.local:8443 (ako se ne zada, pita se).

.PARAMETER Upgrade
  Samo nadogradnja: adresa i token ostaju iz postojeće instalacije.

.PARAMETER Credential
  Administratorski nalog (ako se ne zada, a potreban je, pita se).

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File C:\Temp\NetworkMonitor\Install-AgentLocal.ps1

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File C:\Temp\NetworkMonitor\Install-AgentLocal.ps1 -Upgrade
#>
[CmdletBinding()]
param(
    [string]$MsiPath,
    [string]$ConsoleUrl,
    [switch]$Upgrade,
    [System.Management.Automation.PSCredential]$Credential,
    [switch]$Elevated # interno: skripta je ponovo pokrenuta kao administrator
)

$ErrorActionPreference = 'Stop'

function Test-Admin {
    $p = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
    return $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Wait-Exit([int]$code) {
    if ($Elevated) { Read-Host 'Pritisnite Enter za kraj' | Out-Null }
    exit $code
}

# --- MSI ---
if (-not $MsiPath) { $MsiPath = Join-Path $PSScriptRoot 'NetworkMonitorAgent.msi' }
if (-not (Test-Path -LiteralPath $MsiPath -PathType Leaf)) {
    Write-Host "MSI nije pronađen: $MsiPath" -ForegroundColor Red
    Wait-Exit 2
}
$MsiPath = (Resolve-Path -LiteralPath $MsiPath).Path

# --- Nije administrator: ponovo pokretanje pod administratorskim nalogom ---
if (-not (Test-Admin)) {
    if ($Elevated) {
        Write-Host 'Instalacija nije pokrenuta kao administrator (UAC potvrda je odbijena?).' -ForegroundColor Red
        Wait-Exit 5
    }
    if (-not $Credential) {
        $Credential = Get-Credential -Message 'Administratorski nalog za instalaciju agenta (npr. FIRMA\admin.korisnik)'
        if (-not $Credential) { Write-Host 'Prekinuto.'; exit 1 }
    }

    # Administratorski nalog možda ne može da čita fajlove iz profila ovog
    # korisnika, pa se skripta i MSI kopiraju u javni folder.
    $script = $PSCommandPath
    $msi = $MsiPath
    if ($script -like "$env:SystemDrive\Users\*" -or $msi -like "$env:SystemDrive\Users\*") {
        $stage = Join-Path $env:PUBLIC 'NetworkMonitorSetup'
        New-Item -ItemType Directory -Force -Path $stage | Out-Null
        Copy-Item -LiteralPath $script -Destination $stage -Force
        Copy-Item -LiteralPath $msi -Destination $stage -Force
        $script = Join-Path $stage (Split-Path $script -Leaf)
        $msi = Join-Path $stage (Split-Path $msi -Leaf)
    }

    # Argumenti za administratorski prozor (bez tokena).
    $args2 = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', "`"$script`"", '-MsiPath', "`"$msi`"", '-Elevated')
    if ($Upgrade) { $args2 += '-Upgrade' }
    if ($ConsoleUrl) { $args2 += @('-ConsoleUrl', "`"$ConsoleUrl`"") }
    $argList = ($args2 | ForEach-Object { "'" + ($_ -replace "'", "''") + "'" }) -join ','
    # Proces pod administratorskim nalogom (bez punih prava zbog UAC-a)
    # pokreće skriptu sa punim pravima (-Verb RunAs) – pojaviće se UAC potvrda.
    $inner = "Start-Process -FilePath powershell.exe -Verb RunAs -Wait -ArgumentList @($argList)"
    $enc = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($inner))
    Write-Host 'Pokrećem instalaciju pod administratorskim nalogom; potvrdite UAC upit ako se pojavi...'
    try {
        Start-Process -FilePath powershell.exe -Credential $Credential -WorkingDirectory $env:SystemRoot `
            -ArgumentList "-NoProfile -EncodedCommand $enc" -Wait -WindowStyle Hidden
    } catch {
        Write-Host "Pokretanje pod nalogom $($Credential.UserName) nije uspelo: $($_.Exception.Message)" -ForegroundColor Red
        Write-Host '(Proverite korisničko ime i lozinku i da je servis Secondary Logon (seclogon) pokrenut.)'
        exit 3
    }
    Write-Host 'Gotovo. Rezultat je prikazan u administratorskom prozoru; proverite i konzolu.'
    exit 0
}

# --- Administrator: instalacija ---
Write-Host "Network Monitor agent – instalacija na računaru $env:COMPUTERNAME" -ForegroundColor Cyan
$msiArgs = @('/i', "`"$MsiPath`"", '/qn', '/l*v', "`"$env:SystemRoot\Temp\netmon-agent-install.log`"")
if (-not $Upgrade) {
    while (-not $ConsoleUrl) { $ConsoleUrl = (Read-Host 'Adresa konzole (npr. https://netmon.firma.local:8443)').Trim() }
    $sec = Read-Host 'Token za agente (konzola: Podešavanja → Instalacija agenata → Kopiraj)' -AsSecureString
    $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec)
    try { $token = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr).Trim() }
    finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr) }
    if ($token.Length -lt 16) {
        Write-Host 'Token je prekratak – kopirajte ga ponovo iz konzole.' -ForegroundColor Red
        Wait-Exit 4
    }
    $msiArgs += @("CONSOLE_URL=$ConsoleUrl", "AGENT_TOKEN=$token")
}

Write-Host 'Instaliram...'
$p = Start-Process -FilePath msiexec.exe -ArgumentList $msiArgs -Wait -PassThru
$token = $null
switch ($p.ExitCode) {
    0 { Write-Host 'Instalacija je uspela.' -ForegroundColor Green }
    3010 { Write-Host 'Instalacija je uspela (potreban je restart računara).' -ForegroundColor Green }
    1619 { Write-Host "MSI se ne može otvoriti (1619): $MsiPath" -ForegroundColor Red }
    1603 { Write-Host "Instalacija nije uspela (1603). Detalji: $env:SystemRoot\Temp\netmon-agent-install.log" -ForegroundColor Red }
    default { Write-Host "Instalacija nije uspela (kod $($p.ExitCode)). Detalji: $env:SystemRoot\Temp\netmon-agent-install.log" -ForegroundColor Red }
}
if ($p.ExitCode -in 0, 3010) {
    Start-Sleep -Seconds 3
    $svc = Get-Service -Name 'network-monitor-agent' -ErrorAction SilentlyContinue
    if ($svc) { Write-Host "Servis agenta: $($svc.Status). Računar bi za nekoliko sekundi trebalo da se pojavi u konzoli kao 'na vezi'." }
    # Privremene kopije iz javnog foldera više nisu potrebne.
    $stage = Join-Path $env:PUBLIC 'NetworkMonitorSetup'
    if ($MsiPath -like "$stage\*") { Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue }
}
Wait-Exit $p.ExitCode
