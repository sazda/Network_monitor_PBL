# Instalacija

Build, instalacija konzole i agenata (računari i serveri).

## Build

### Na Windows-u

1. Instalirajte **Go 1.24+** sa <https://go.dev/dl/> (MSI instaler).
2. Za MSI pakete instalirajte i **.NET SDK 8+** sa
   <https://dotnet.microsoft.com/download>. Skripta sama instalira WiX Toolset 5
   (`dotnet tool install --global wix`) i potrebne ekstenzije.
3. Preuzmite izvorni kod (`git clone` ili ZIP sa GitHub-a za odgovarajuću granu)
   i u PowerShell-u, u folderu projekta, pokrenite:

```powershell
powershell -ExecutionPolicy Bypass -File .\build.ps1 -Test -Msi
```

Rezultat u `dist\`:

| Fajl | Namena |
|---|---|
| `NetworkMonitorConsole.msi` | instalacija konzole na server |
| `NetworkMonitorAgent.msi` | instalacija agenta na računare korisnika |
| `console.exe`, `agent.exe` | isti programi bez instalera |

Ostale opcije: `-MsiVersion 1.2.0` (verzija paketa; podrazumevano raste svakog
dana, pa svaki novi build nadograđuje stari), `-AllPlatforms` (i agent za
Windows na ARM-u), `-ConsoleUrl` / `-AgentToken` (vidi „GPO Software Installation”).

Pri prvom build-u Go i dotnet preuzimaju zavisnosti sa interneta.

### Potpisivanje (preporučeno)

Potpišite programe i pakete sertifikatom za potpisivanje koda iz internog CA
(šablon *Code Signing*), da ih Defender/SmartScreen ne bi blokirali i da bi
radili uz AppLocker/WDAC. Skripta potpisuje `.exe` fajlove pre pakovanja, pa
MSI pakete:

```powershell
Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert     # pronađite thumbprint
.\build.ps1 -Msi -SignThumbprint <thumbprint>
```

Vremenski žig (`-TimestampServer`, podrazumevano DigiCert) zahteva pristup
internetu; bez njega potpis prestaje da važi kada istekne sertifikat.

## Instalacija konzole

Primeri koriste ime `netmon.firma.local` — zamenite ga svojim.

### Pre instalacije

- U DNS-u napravite zaseban zapis (npr. `netmon.firma.local`) za server, umesto
  da agenti koriste ime samog servera — tako se konzola kasnije može preseliti
  bez ponovne instalacije agenata.
- Proverite tip mrežne kartice VM-a:
  `Get-NetAdapter | Format-List Name,InterfaceDescription,LinkSpeed,DriverVersion`.
  Treba da bude paravirtuelna (vmxnet3 ili „Red Hat VirtIO”), a VM da ima bar 2 vCPU.
- Port 8443 mora biti slobodan:
  `Get-NetTCPConnection -LocalPort 8443 -State Listen -ErrorAction SilentlyContinue`
  ne sme ništa da ispiše (u suprotnom instalirajte sa drugim portom, vidi dole).
- Firewall/ACL između VLAN-ova mora da dozvoli TCP 8443 od računara korisnika
  ka serveru.

### 1. MSI

Dvoklik na `NetworkMonitorConsole.msi`, ili:

```powershell
msiexec /i NetworkMonitorConsole.msi /qb             # drugi port: dodati PORT=9443
```

Paket instalira `C:\Program Files\NetworkMonitor\Console\console.exe`, servis
„Network Monitor Console” (automatski start, restart posle pada), firewall
pravilo za port (Domain profil) i prečicu u Start meniju
**Network Monitor → Network Monitor konzola**.

Pri prvom pokretanju konzola sama pravi token za agente i privremeni sertifikat
(`C:\ProgramData\NetworkMonitor\console.json`).

### 2. Administratorska lozinka

**Na samom serveru** otvorite Start meni → *Network Monitor konzola*
(`https://localhost:8443/`). Pretraživač će upozoriti na sertifikat (privremeni
je) — nastavite. Postavite korisničko ime i lozinku. Od tog trenutka konzola je
dostupna sa svih računara, uz prijavu.

### 3. Sertifikat iz AD CS

Potreban je sertifikat sa imenom konzole (npr. `netmon.firma.local`) u SAN
polju, iz šablona tipa „Web Server”, sa **dozvoljenim izvozom privatnog
ključa**. Dostupne šablone vidite sa `certutil -CATemplates`. Na serveru, kao
administrator:

```powershell
$c = Get-Certificate -Template <ImeSablona> -DnsName netmon.firma.local `
       -SubjectName CN=netmon.firma.local -CertStoreLocation Cert:\LocalMachine\My
Export-PfxCertificate -Cert $c.Certificate -FilePath $env:USERPROFILE\Desktop\netmon.pfx `
  -Password (Read-Host -AsSecureString "Lozinka za PFX")
```

(Isto se može kroz `certlm.msc` → Personal → *Request New Certificate*, pa
*Export* sa privatnim ključem.)

U konzoli otvorite **Podešavanja → TLS sertifikat**, izaberite PFX fajl, unesite
lozinku i kliknite **Otpremi i primeni**. Sertifikat se primenjuje odmah, bez
restarta. Posle toga obrišite PFX sa Desktop-a i konzolu otvarajte preko
`https://netmon.firma.local:8443`.

Kada sertifikat bude trebalo obnoviti, isti postupak se ponavlja (konzola u
logu upozorava 30 dana pre isteka).

### Održavanje

- Nadogradnja: instalirajte noviji `NetworkMonitorConsole.msi` preko postojećeg;
  podešavanja i rezultati ostaju.
- Log: `C:\ProgramData\NetworkMonitor\console-data\console.log`; rezultati:
  `results.jsonl` u istom folderu.
- Deinstalacija (*Apps & features*) uklanja program, servis i firewall pravilo;
  `C:\ProgramData\NetworkMonitor` ostaje (obrisati ručno ako treba).

## Instalacija agenata

Agentu su potrebna dva podatka: adresa konzole i token. Oba se vide u konzoli,
**Podešavanja → Instalacija agenata**, gde je i gotova komanda:

```powershell
msiexec /i NetworkMonitorAgent.msi /qn CONSOLE_URL=https://netmon.firma.local:8443 AGENT_TOKEN=<token>
```

Paket instalira `C:\Program Files\NetworkMonitor\Agent\agent.exe`, servis
„Network Monitor Agent”, firewall pravilo za LAN test (Domain profil) i upisuje
adresu i token u zaštićen registry ključ. Agent se odmah povezuje na konzolu.
Nadogradnja: isti MSI noviji verzije, bez parametara (adresa i token se
zadržavaju). Log agenta: `C:\ProgramData\NetworkMonitor\agent.log`.

**Prvo instalirajte agenta ručno na dva računara iz različitih VLAN-ova** i
proverite u konzoli Latencija / Gateway / Brzina / LAN test, pa tek onda na sve.

### Masovna instalacija

**a) SCCM / Intune / PDQ / slično (preporučeno).** Komanda iznad, kao
„Install command”; detekcija po MSI ProductCode ili po servisu
`network-monitor-agent`. Token se prosleđuje pri instalaciji i ne ostaje u
samom paketu.

**Iz konzole (najjednostavnije):** *Podešavanja → Instalacija agenata na
računare*: upišite nazive računara (ili kliknite „Dodaj sve računare iz
konzole” / „Dodaj računare koji nisu na vezi”), administratorski nalog i
lozinku, pa **Instaliraj**. Konzola preko WinRM-a kopira agenta i instalira ga
sa svojom adresom i tokenom (token ne treba kucati); status se prikazuje za
svaki računar. Opcija „samo nadogradnja” nadograđuje postojeće agente.
Najviše 4 računara se obrađuju istovremeno. Nalog i lozinka se koriste samo za
taj posao: ne čuvaju se i ne upisuju u log (u logu ostaje ko je pokrenuo
instalaciju, kojim nalogom i rezultat po računaru). MSI agenta je deo MSI-ja
konzole, pa su konzola i agenti uvek iste verzije. Preporuka: poseban nalog
koji je lokalni administrator samo na radnim stanicama, a ne Domain Admin.
Za IP adresu konzola pokušava da pronađe naziv računara (obrnuti DNS), jer
WinRM sa domenskim nalogom radi sa nazivima.

**b) PowerShell Remoting (WinRM)** — sa svog računara, na jedan ili više
računara odjednom. Skripta traži administratorske kredencijale za ciljne
računare i token, kopira MSI kroz WinRM sesiju i instalira ga:

```powershell
powershell -ExecutionPolicy Bypass -File .\deploy\windows\Install-AgentRemote.ps1 `
  -ComputerName PC01, PC02 -ConsoleUrl https://netmon.firma.local:8443
# ili spisak iz fajla (jedan računar po liniji):
powershell -ExecutionPolicy Bypass -File .\deploy\windows\Install-AgentRemote.ps1 `
  -ComputerListFile .\racunari.txt -ConsoleUrl https://netmon.firma.local:8443
# nadogradnja postojećih agenata (bez tokena):
powershell -ExecutionPolicy Bypass -File .\deploy\windows\Install-AgentRemote.ps1 -ComputerListFile .\racunari.txt -Upgrade
```

Uslov: WinRM uključen na ciljnim računarima (`Test-WSMan PC01`; za sve
računare GPO *Windows Components → Windows Remote Management → WinRM Service →
Allow remote server management through WinRM*).

**b2) Bez WinRM-a (npr. računar na VPN-u):** MSI i skripta se kopiraju na
računar preko deljenog foldera `C$` (SMB, port 445), a instalacija se pokreće na
samom računaru – lično ili preko udaljene pomoći. Obe skripte traže
administratorski nalog (`Get-Credential`) i nigde ga ne čuvaju:

```powershell
# sa svog računara: kopira MSI i Install-AgentLocal.ps1 u C:\Temp\NetworkMonitor
powershell -ExecutionPolicy Bypass -File .\deploy\windows\Copy-AgentToComputer.ps1 -ComputerName PC01
# na samom računaru PC01 (i kao običan korisnik):
powershell -ExecutionPolicy Bypass -File C:\Temp\NetworkMonitor\Install-AgentLocal.ps1
# nadogradnja (adresa i token ostaju):
powershell -ExecutionPolicy Bypass -File C:\Temp\NetworkMonitor\Install-AgentLocal.ps1 -Upgrade
```

Ako PowerShell nije pokrenut kao administrator, `Install-AgentLocal.ps1` traži
administratorski nalog, pokreće se ponovo pod njim (uz UAC potvrdu) i tek u tom
prozoru pita za adresu konzole i token (skriven unos; token se ne prosleđuje
kroz komandnu liniju). Prikazuje ishod instalacije i stanje servisa agenta.
Ako kopiranje javi grešku 1219, postoji veza ka tom računaru pod drugim
nalogom: `net use \\PC01\IPC$ /delete`.

**c) GPO Software Installation.** GPO ne može da prosledi parametre MSI-ju, pa
se adresa i token ugrađuju u paket pri build-u:

```powershell
.\build.ps1 -Msi -ConsoleUrl https://netmon.firma.local:8443 -AgentToken <token>
```

Paket stavite na share sa pravima **`Domain Computers` — Read, `Domain Admins` —
Full, korisnici bez pristupa**, pa u GPO-u povezanom sa OU računara:
*Computer Configuration → Policies → Software Settings → Software installation →
New → Package* (UNC putanja, npr. `\\fs01\NetMon$\NetworkMonitorAgent.msi`),
tip **Assigned**. Uključite i *Computer Configuration → Administrative Templates
→ System → Logon → „Always wait for the network at computer startup and logon”*.
Agent se instalira pri sledećem restartu računara. Za nadogradnju: novi MSI
dodajte u isti GPO i u *Upgrades* označite da nadograđuje prethodni.

Rizik ove varijante: Windows čuva kopiju instaliranog MSI-ja u
`C:\Windows\Installer`, pa je token ugrađen u paket potencijalno čitljiv
lokalnim korisnicima (proverite `icacls C:\Windows\Installer` u vašem
okruženju). Posledica bi bila mogućnost povezivanja lažnog agenta, ne kontrola
nad računarima. Ako to nije prihvatljivo, koristite varijantu a).
