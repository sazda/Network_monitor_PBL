<#
.SYNOPSIS
  Instalira (ili nadograđuje) Network Monitor agenta na udaljenim računarima
  preko PowerShell Remoting-a (WinRM).

.DESCRIPTION
  Skripta traži administratorske kredencijale za ciljne računare i token za
  agente (iz konzole: Podešavanja -> Token za agente), kopira MSI na svaki
  računar kroz WinRM sesiju i pokreće tihu instalaciju.

  MSI se čita sa lokalnog računara pod nalogom koji pokreće skriptu; zadati
  kredencijali se koriste samo za udaljene računare.

  Uslovi na ciljnim računarima: uključen WinRM (Test-WSMan <računar>) i
  zadati nalog je lokalni administrator.

.PARAMETER ComputerName
  Jedan ili više naziva računara.

.PARAMETER ComputerListFile
  Tekstualni fajl sa nazivima računara, jedan po liniji (# = komentar).

.PARAMETER MsiPath
  Putanja do NetworkMonitorAgent.msi (podrazumevano: dist\NetworkMonitorAgent.msi
  pored ove skripte, ili u tekućem folderu).

.PARAMETER ConsoleUrl
  Adresa konzole, npr. https://netmon.firma.local:8443

.PARAMETER Upgrade
  Samo nadogradnja već instaliranih agenata: token se ne traži, adresa i token
  ostaju iz postojeće instalacije.

.EXAMPLE
  .\Install-AgentRemote.ps1 -ComputerName inf-milosm -ConsoleUrl https://netmon.firma.local:8443

.EXAMPLE
  .\Install-AgentRemote.ps1 -ComputerListFile .\racunari.txt -ConsoleUrl https://netmon.firma.local:8443

.EXAMPLE
  .\Install-AgentRemote.ps1 -ComputerListFile .\racunari.txt -Upgrade
#>
[CmdletBinding()]
param(
    [string[]]$ComputerName,
    [string]$ComputerListFile,
    [string]$MsiPath,
    [string]$ConsoleUrl,
    [switch]$Upgrade
)

$ErrorActionPreference = 'Stop'

# --- Spisak računara ---
$computers = @()
if ($ComputerName) { $computers += $ComputerName }
if ($ComputerListFile) {
    $computers += Get-Content -LiteralPath $ComputerListFile |
        ForEach-Object { ($_ -replace '#.*$', '').Trim() } |
        Where-Object { $_ }
}
$computers = @($computers | Sort-Object -Unique)
if ($computers.Count -eq 0) {
    throw 'Zadajte -ComputerName ili -ComputerListFile.'
}

# --- MSI ---
if (-not $MsiPath) {
    foreach ($c in @(
        (Join-Path $PSScriptRoot 'NetworkMonitorAgent.msi'),
        (Join-Path $PSScriptRoot '..\..\dist\NetworkMonitorAgent.msi'),
        (Join-Path (Get-Location) 'NetworkMonitorAgent.msi'),
        (Join-Path (Get-Location) 'dist\NetworkMonitorAgent.msi'))) {
        if (Test-Path -LiteralPath $c) { $MsiPath = $c; break }
    }
}
if (-not $MsiPath -or -not (Test-Path -LiteralPath $MsiPath)) {
    throw "NetworkMonitorAgent.msi nije pronađen. Zadajte -MsiPath."
}
$MsiPath = (Resolve-Path -LiteralPath $MsiPath).Path
try {
    # Provera da tekući nalog može da pročita MSI (česta greška: fajl u
    # profilu drugog korisnika).
    [IO.File]::OpenRead($MsiPath).Close()
} catch {
    throw "Nalog $env:USERNAME ne može da pročita $MsiPath. Kopirajte MSI u npr. C:\Temp i zadajte -MsiPath."
}

# --- Podešavanja agenta ---
$token = $null
if (-not $Upgrade) {
    if (-not $ConsoleUrl) { $ConsoleUrl = Read-Host 'Adresa konzole (npr. https://netmon.firma.local:8443)' }
    if ($ConsoleUrl -notmatch '^https://[^\s/]+(:\d+)?/?$') {
        throw "Neispravna adresa konzole: $ConsoleUrl (očekuje se https://ime:port)"
    }
    $secToken = Read-Host 'Token za agente (konzola -> Podešavanja)' -AsSecureString
    $token = [System.Net.NetworkCredential]::new('', $secToken).Password.Trim()
    if ($token -notmatch '^[0-9a-fA-F]{16,}$') {
        throw 'Token nije ispravan (očekuje se niz heksadecimalnih znakova iz konzole).'
    }
}

# --- Kredencijali za udaljene računare ---
$cred = Get-Credential -Message 'Administratorski nalog za ciljne računare (npr. FIRMA\admin.korisnik)'
if (-not $cred) { throw 'Kredencijali nisu uneti.' }

$results = foreach ($pc in $computers) {
    $s = $null
    $status = ''
    try {
        Write-Host "$pc ..." -NoNewline
        $s = New-PSSession -ComputerName $pc -Credential $cred -ErrorAction Stop
        Invoke-Command -Session $s {
            New-Item -ItemType Directory -Force -Path C:\Windows\Temp\NetMon | Out-Null
        }
        Copy-Item -LiteralPath $MsiPath -Destination 'C:\Windows\Temp\NetMon\NetworkMonitorAgent.msi' -ToSession $s -ErrorAction Stop

        $code = Invoke-Command -Session $s -ArgumentList $ConsoleUrl, $token {
            param($url, $token)
            $msiArgs = '/i "C:\Windows\Temp\NetMon\NetworkMonitorAgent.msi" /qn /norestart'
            if ($token) { $msiArgs += " CONSOLE_URL=$url AGENT_TOKEN=$token" }
            $p = Start-Process msiexec.exe -ArgumentList $msiArgs -Wait -PassThru
            Remove-Item C:\Windows\Temp\NetMon -Recurse -Force -ErrorAction SilentlyContinue
            $svc = Get-Service network-monitor-agent -ErrorAction SilentlyContinue
            [pscustomobject]@{ ExitCode = $p.ExitCode; Service = if ($svc) { [string]$svc.Status } else { 'nema servisa' } }
        }
        $status = switch ($code.ExitCode) {
            0       { "instaliran (servis: $($code.Service))" }
            3010    { "instaliran, potreban restart (servis: $($code.Service))" }
            1603    { 'GREŠKA 1603: instalacija nije uspela (npr. stari agent instaliran drugim putem)' }
            1618    { 'GREŠKA 1618: u toku je druga instalacija, pokušajte kasnije' }
            1638    { 'GREŠKA 1638: ista ili novija verzija je već instalirana' }
            default { "izlazni kod $($code.ExitCode)" }
        }
    } catch {
        $status = "GREŠKA: $($_.Exception.Message)"
    } finally {
        if ($s) { Remove-PSSession $s }
    }
    Write-Host " $status"
    [pscustomobject]@{ Racunar = $pc; Rezultat = $status }
}

$token = $null
Write-Host ''
$results | Format-Table -AutoSize -Wrap
