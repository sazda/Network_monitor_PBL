<#
.SYNOPSIS
  Pravi console.exe i agent.exe (Windows ekvivalent za "make console agents").

.DESCRIPTION
  Potreban je Go 1.24 ili noviji (https://go.dev/dl/). Pri prvom build-u Go
  preuzima zavisnosti sa proxy.golang.org, pa računar mora imati pristup
  internetu (ili podešen GOPROXY).

  Rezultat je u folderu dist\:
    console.exe   - konzola (Windows, amd64)
    agent.exe     - agent za Windows računare (amd64)
    serveragent.exe - agent za Windows servere (amd64; stranica „Serveri”)

.PARAMETER Test
  Pre build-a pokreće "go vet" i testove.

.PARAMETER AllPlatforms
  Pravi i agenta za Windows na ARM-u (arm64).

.PARAMETER Msi
  Pravi i MSI pakete NetworkMonitorConsole.msi i NetworkMonitorAgent.msi.
  Potreban je WiX Toolset 5 (instalira se automatski ako postoji .NET SDK):
    https://dotnet.microsoft.com/download  (.NET SDK 8 ili noviji)

.PARAMETER MsiVersion
  Verzija MSI paketa (npr. 1.2.0). Podrazumevano 1.1.<broj dana od 1.1.2026>,
  tako da svaki noviji build nadograđuje prethodni.

.PARAMETER ConsoleUrl
  Opciono: adresa konzole ugrađena u NetworkMonitorAgent.msi (za instalaciju
  preko GPO Software Installation, gde se svojstva ne mogu zadati).

.PARAMETER AgentToken
  Opciono: token ugrađen u NetworkMonitorAgent.msi (vidi README, bezbednost).

.PARAMETER SignThumbprint
  Opciono: thumbprint sertifikata za potpisivanje koda (Cert:\CurrentUser\My
  ili Cert:\LocalMachine\My). Potpisuju se .exe fajlovi pre pakovanja u MSI,
  a zatim i MSI paketi.

.PARAMETER TimestampServer
  Timestamp server za potpis (podrazumevano http://timestamp.digicert.com;
  prazno = bez vremenskog žiga).

.EXAMPLE
  .\build.ps1
  .\build.ps1 -Test -Msi
#>
[CmdletBinding()]
param(
    [switch]$Test,
    [switch]$AllPlatforms,
    [switch]$Msi,
    [string]$MsiVersion,
    [string]$ConsoleUrl,
    [string]$AgentToken,
    [string]$SignThumbprint,
    [string]$TimestampServer = 'http://timestamp.digicert.com'
)

# Ne koristimo 'Stop': Windows PowerShell 5.1 bi tada prekinuo skriptu čim go
# ispiše bilo šta na stderr (npr. "go: downloading ..."). Greške proveravamo
# preko $LASTEXITCODE.
$ErrorActionPreference = 'Continue'
Set-Location $PSScriptRoot

# Putanja do go.exe. Ako go nije u PATH-u (npr. PowerShell je otvoren pre
# instalacije Go-a, pa nema osveženu PATH promenljivu), tražimo ga na
# uobičajenim mestima i u PATH-u upisanom u sistem posle otvaranja prozora.
$go = (Get-Command go -ErrorAction SilentlyContinue | Select-Object -First 1).Source
if (-not $go) {
    $dirs = New-Object System.Collections.Generic.List[string]
    if ($env:GOROOT) { $dirs.Add((Join-Path $env:GOROOT 'bin')) }
    foreach ($d in @($env:ProgramFiles, $env:LOCALAPPDATA, $env:USERPROFILE)) {
        if (-not $d) { continue }
        $dirs.Add((Join-Path $d 'Go\bin'))
        $dirs.Add((Join-Path $d 'Programs\Go\bin'))
        $dirs.Add((Join-Path $d 'go-sdk\bin'))
        $dirs.Add((Join-Path $d 'sdk\go\bin'))
    }
    $dirs.Add('C:\Go\bin')
    foreach ($scope in 'Machine', 'User') {
        $p = [Environment]::GetEnvironmentVariable('Path', $scope)
        if ($p) { foreach ($d in $p.Split(';')) { if ($d) { $dirs.Add($d) } } }
    }
    foreach ($d in $dirs) {
        $exe = Join-Path $d 'go.exe'
        if (Test-Path -LiteralPath $exe -PathType Leaf) { $go = $exe; break }
    }
    if (-not $go) {
        throw ('Go nije pronađen. Instalirajte ga sa https://go.dev/dl/ (MSI, verzija 1.24+), ' +
               'zatvorite i ponovo otvorite PowerShell, pa pokrenite skriptu ponovo.')
    }
    Write-Host "Go nije u PATH-u ovog prozora; koristim $go"
}
Write-Host (& $go version)

# Verzija: git tag/commit ako je dostupan git, inače datum.
$version = $null
if (Get-Command git -ErrorAction SilentlyContinue) {
    $version = (git describe --tags --always --dirty 2>$null)
}
if (-not $version) { $version = 'build-' + (Get-Date -Format 'yyyyMMdd-HHmm') }
$ldflags = "-s -w -X github.com/sazda/network-monitor/internal/agent.Version=$version" +
           " -X github.com/sazda/network-monitor/internal/console.Version=$version"

if ($Test) {
    & $go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet nije prošao' }
    & $go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'testovi nisu prošli' }
}

function Build([string]$goos, [string]$goarch, [string]$pkg, [string]$out) {
    Write-Host "build $out ($goos/$goarch)"
    $env:CGO_ENABLED = '0'
    $env:GOOS = $goos
    $env:GOARCH = $goarch
    try {
        & $go build -trimpath -ldflags $ldflags -o $out $pkg
        if ($LASTEXITCODE -ne 0) { throw "build $out nije uspeo" }
    } finally {
        Remove-Item Env:\GOOS, Env:\GOARCH, Env:\CGO_ENABLED -ErrorAction SilentlyContinue
    }
}

$signCert = $null
if ($SignThumbprint) {
    $signCert = Get-ChildItem Cert:\CurrentUser\My, Cert:\LocalMachine\My -CodeSigningCert |
        Where-Object { $_.Thumbprint -eq ($SignThumbprint -replace '\s', '').ToUpper() } | Select-Object -First 1
    if (-not $signCert) { throw "Sertifikat za potpisivanje koda $SignThumbprint nije pronađen" }
}

function Sign([string[]]$files) {
    if (-not $signCert) { return }
    foreach ($f in $files) {
        $params = @{ FilePath = $f; Certificate = $signCert; HashAlgorithm = 'SHA256' }
        if ($TimestampServer) { $params.TimestampServer = $TimestampServer }
        $r = Set-AuthenticodeSignature @params
        if ($r.Status -ne 'Valid') { throw "Potpisivanje $f nije uspelo: $($r.StatusMessage)" }
        Write-Host "potpisan $f"
    }
}

New-Item -ItemType Directory -Force -Path dist | Out-Null
Build 'windows' 'amd64' './cmd/console' 'dist/console.exe'
Build 'windows' 'amd64' './cmd/agent'   'dist/agent.exe'
Build 'windows' 'amd64' './cmd/serveragent' 'dist/serveragent.exe'

if ($AllPlatforms) {
    Build 'windows' 'arm64' './cmd/agent' 'dist/agent-windows-arm64.exe'
}


Sign (Get-ChildItem dist -Filter *.exe | ForEach-Object FullName)

if ($Msi) {
    $wixVersion = '5.0.2'
    $wix = (Get-Command wix -ErrorAction SilentlyContinue | Select-Object -First 1).Source
    if (-not $wix) {
        $candidate = Join-Path $env:USERPROFILE '.dotnet\tools\wix.exe'
        if (Test-Path $candidate) { $wix = $candidate }
    }
    if (-not $wix) {
        if (-not (Get-Command dotnet -ErrorAction SilentlyContinue)) {
            throw ('Za MSI je potreban WiX Toolset, a za njega .NET SDK 8+ ' +
                   '(https://dotnet.microsoft.com/download). Instalirajte .NET SDK, otvorite novi PowerShell i pokrenite ponovo.')
        }
        Write-Host "Instaliram WiX Toolset $wixVersion (dotnet tool)..."
        dotnet tool install --global wix --version $wixVersion
        if ($LASTEXITCODE -ne 0) { throw 'Instalacija WiX-a nije uspela' }
        $wix = Join-Path $env:USERPROFILE '.dotnet\tools\wix.exe'
    }
    $exts = & $wix extension list -g 2>$null | Out-String
    foreach ($ext in 'WixToolset.Util.wixext', 'WixToolset.Firewall.wixext') {
        if ($exts -notmatch [regex]::Escape($ext)) {
            Write-Host "Dodajem WiX ekstenziju $ext"
            & $wix extension add -g "$ext/$wixVersion"
            if ($LASTEXITCODE -ne 0) { throw "Dodavanje $ext nije uspelo" }
        }
    }

    if (-not $MsiVersion) {
        $days = [int][math]::Floor(((Get-Date) - [datetime]'2026-01-01').TotalDays)
        # 1.1.x: veće od svih ranijih 1.0.x paketa (ranija verzija skripte je
        # broj dana zaokruživala naviše, pa su neki paketi dobili sutrašnji broj).
        $MsiVersion = "1.1.$days"
    }
    $common = @('-arch', 'x64', '-ext', 'WixToolset.Util.wixext', '-ext', 'WixToolset.Firewall.wixext',
                '-d', "Version=$MsiVersion")

    # Prvo agent: njegov MSI se pakuje u MSI konzole (instalacija agenata iz konzole).
    $agentArgs = @('-d', 'AgentExe=dist/agent.exe')
    if ($ConsoleUrl) { $agentArgs += @('-d', "DefaultConsoleUrl=$ConsoleUrl") }
    if ($AgentToken) { $agentArgs += @('-d', "DefaultAgentToken=$AgentToken") }
    Write-Host "MSI dist/NetworkMonitorAgent.msi (verzija $MsiVersion)"
    & $wix build installer/agent.wxs @common @agentArgs -o dist/NetworkMonitorAgent.msi
    if ($LASTEXITCODE -ne 0) { throw 'Pravljenje NetworkMonitorAgent.msi nije uspelo' }
    Sign @('dist/NetworkMonitorAgent.msi')

    # Agent za servere: poseban paket (servis network-monitor-server-agent).
    $srvArgs = @('-d', 'ServerAgentExe=dist/serveragent.exe')
    if ($ConsoleUrl) { $srvArgs += @('-d', "DefaultConsoleUrl=$ConsoleUrl") }
    if ($AgentToken) { $srvArgs += @('-d', "DefaultAgentToken=$AgentToken") }
    Write-Host "MSI dist/NetworkMonitorServerAgent.msi (verzija $MsiVersion)"
    & $wix build installer/serveragent.wxs @common @srvArgs -o dist/NetworkMonitorServerAgent.msi
    if ($LASTEXITCODE -ne 0) { throw 'Pravljenje NetworkMonitorServerAgent.msi nije uspelo' }
    Sign @('dist/NetworkMonitorServerAgent.msi')

    Write-Host "MSI dist/NetworkMonitorConsole.msi (verzija $MsiVersion)"
    & $wix build installer/console.wxs @common -d ConsoleExe=dist/console.exe -d AgentMsi=dist/NetworkMonitorAgent.msi -d ServerAgentMsi=dist/NetworkMonitorServerAgent.msi -o dist/NetworkMonitorConsole.msi
    if ($LASTEXITCODE -ne 0) { throw 'Pravljenje NetworkMonitorConsole.msi nije uspelo' }
    Sign @('dist/NetworkMonitorConsole.msi')

    # wix ostavlja .wixpdb fajlove (simboli za debagovanje) - nisu potrebni za instalaciju
    Remove-Item dist/*.wixpdb -ErrorAction SilentlyContinue
}

Write-Host ''
Write-Host "Gotovo (verzija $version):"
foreach ($f in Get-ChildItem dist) {
    Write-Host ('  {0,-28} {1,6:N1} MB' -f $f.Name, ($f.Length / 1MB))
}
