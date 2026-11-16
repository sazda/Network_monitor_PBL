<#
.SYNOPSIS
  Kopira MSI agenta i skriptu Install-AgentLocal.ps1 na udaljene računare
  (\\racunar\C$\Temp\NetworkMonitor), sa administratorskim nalogom.

.DESCRIPTION
  Za računare na kojima ne radi instalacija iz konzole (WinRM isključen ili
  blokiran, npr. preko VPN-a). Kopiranje ide preko deljenog foldera C$ (SMB,
  port 445), pa ne zahteva WinRM. Instalaciju zatim treba pokrenuti NA
  samom računaru (lično ili preko udaljene pomoći):

    powershell -ExecutionPolicy Bypass -File C:\Temp\NetworkMonitor\Install-AgentLocal.ps1

  Kredencijali se koriste samo za kopiranje i nigde se ne čuvaju.

.PARAMETER ComputerName
  Jedan ili više naziva računara.

.PARAMETER MsiPath
  Putanja do NetworkMonitorAgent.msi (podrazumevano: dist\NetworkMonitorAgent.msi
  u folderu projekta, ili pored ove skripte).

.PARAMETER Credential
  Administratorski nalog za ciljne računare (ako se ne zada, pita se).

.EXAMPLE
  .\Copy-AgentToComputer.ps1 -ComputerName raw-perat
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string[]]$ComputerName,
    [string]$MsiPath,
    [System.Management.Automation.PSCredential]$Credential
)

$ErrorActionPreference = 'Stop'

if (-not $MsiPath) {
    foreach ($c in @((Join-Path $PSScriptRoot '..\..\dist\NetworkMonitorAgent.msi'),
                     (Join-Path $PSScriptRoot 'NetworkMonitorAgent.msi'),
                     '.\dist\NetworkMonitorAgent.msi', '.\NetworkMonitorAgent.msi')) {
        if (Test-Path -LiteralPath $c -PathType Leaf) { $MsiPath = $c; break }
    }
}
if (-not $MsiPath -or -not (Test-Path -LiteralPath $MsiPath -PathType Leaf)) {
    throw 'NetworkMonitorAgent.msi nije pronađen; zadajte ga parametrom -MsiPath.'
}
$MsiPath = (Resolve-Path -LiteralPath $MsiPath).Path
$installScript = Join-Path $PSScriptRoot 'Install-AgentLocal.ps1'

if (-not $Credential) {
    $Credential = Get-Credential -Message 'Administratorski nalog za ciljne računare (npr. FIRMA\admin.korisnik)'
    if (-not $Credential) { Write-Host 'Prekinuto.'; exit 1 }
}

$failed = 0
foreach ($pc in $ComputerName) {
    $drive = 'NMCopy'
    try {
        # Deljeni folder C$ sa zadatim nalogom (bez WinRM-a).
        New-PSDrive -Name $drive -PSProvider FileSystem -Root "\\$pc\C$" -Credential $Credential -Scope Script | Out-Null
        $dest = "${drive}:\Temp\NetworkMonitor"
        New-Item -ItemType Directory -Force -Path $dest | Out-Null
        Copy-Item -LiteralPath $MsiPath -Destination $dest -Force
        Copy-Item -LiteralPath $installScript -Destination $dest -Force
        Write-Host "$pc : kopirano u C:\Temp\NetworkMonitor" -ForegroundColor Green
    } catch {
        $failed++
        $msg = $_.Exception.Message
        if ($msg -match '1219|multiple connections|više veza') {
            $msg += ' (Već postoji veza ka tom računaru pod drugim nalogom; prekinite je komandom: net use \\' + $pc + '\IPC$ /delete, pa pokušajte ponovo.)'
        }
        Write-Host "$pc : kopiranje nije uspelo – $msg" -ForegroundColor Red
    } finally {
        if (Get-PSDrive -Name $drive -ErrorAction SilentlyContinue) { Remove-PSDrive -Name $drive -Force }
    }
}

Write-Host ''
Write-Host 'Na svakom računaru pokrenite (lično ili preko udaljene pomoći):'
Write-Host '  powershell -ExecutionPolicy Bypass -File C:\Temp\NetworkMonitor\Install-AgentLocal.ps1'
Write-Host 'Ako korisnik nije administrator, skripta će tražiti administratorski nalog.'
if ($failed) { exit 1 }
