# Instalira ili nadograđuje Network Monitor agenta na jednom udaljenom
# računaru preko PowerShell Remoting-a (WinRM). Pokreće ga konzola; ulazni
# podaci (uključujući lozinku) stižu kao JSON na standardni ulaz, da se ne bi
# videli u komandnoj liniji procesa. Rezultat se ispisuje kao jedan red JSON-a.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$result = [ordered]@{ ok = $false; exitCode = $null; service = ''; message = '' }
try {
    $in = [Console]::In.ReadToEnd() | ConvertFrom-Json
    $sec = ConvertTo-SecureString $in.password -AsPlainText -Force
    $cred = New-Object System.Management.Automation.PSCredential ($in.user, $sec)

    $s = New-PSSession -ComputerName $in.computer -Credential $cred
    try {
        Invoke-Command -Session $s {
            New-Item -ItemType Directory -Force -Path C:\Windows\Temp\NetMon | Out-Null
        }
        # Paket i servis: agent za računare ili za servere (starija konzola ih ne šalje).
        $msiName = if ($in.msi_name) { $in.msi_name } else { 'NetworkMonitorAgent.msi' }
        $svcName = if ($in.service) { $in.service } else { 'network-monitor-agent' }
        Copy-Item -LiteralPath $in.msi -Destination "C:\Windows\Temp\NetMon\$msiName" -ToSession $s

        $r = Invoke-Command -Session $s -ArgumentList $in.url, $in.token, $in.upgrade, $msiName, $svcName {
            param($url, $token, $upgrade, $msiName, $svcName)
            $msiArgs = "/i `"C:\Windows\Temp\NetMon\$msiName`" /qn /norestart"
            if (-not $upgrade) { $msiArgs += " CONSOLE_URL=$url AGENT_TOKEN=$token" }
            $p = Start-Process msiexec.exe -ArgumentList $msiArgs -Wait -PassThru
            Remove-Item C:\Windows\Temp\NetMon -Recurse -Force -ErrorAction SilentlyContinue
            $svc = Get-Service $svcName -ErrorAction SilentlyContinue
            [pscustomobject]@{
                ExitCode = $p.ExitCode
                Service  = if ($svc) { [string]$svc.Status } else { '' }
            }
        }
        $result.exitCode = $r.ExitCode
        $result.service = $r.Service
        $result.ok = ($r.ExitCode -eq 0 -or $r.ExitCode -eq 3010)
    } finally {
        Remove-PSSession $s
    }
} catch {
    $result.message = $_.Exception.Message
}
$result | ConvertTo-Json -Compress
