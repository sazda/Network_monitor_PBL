# Network Monitor

Nadzor lokalne mreže sa jedne centralne konzole u web pretraživaču: računari
korisnika, mrežna oprema, MPLS veze lokacija i serveri, sa obaveštenjima na
e-mail i SMS.

## Delovi sistema

| Deo | Gde radi | Šta radi |
|---|---|---|
| **Konzola** (`NetworkMonitorConsole.msi`) | Windows server, servis | web interfejs, čitanje mrežne opreme, istorija, obaveštenja |
| **Agent za računare** (`NetworkMonitorAgent.msi`) | računari korisnika, servis | merenja latencije i brzine na zahtev konzole |
| **Agent za servere** (`NetworkMonitorServerAgent.msi`) | Windows serveri, servis | CPU, RAM, diskovi, procesi |
| **NetMon SMS** (Android aplikacija) | telefon sa SIM karticom | obaveštenja kao SMS i SMS upiti |

Programi su napisani u Go-u: po jedan `.exe`, bez .NET-a, Jave i sličnog.

## Kartice konzole

- **Računari** – agenti, merenja latencije i brzine, LAN test, ping u trajanju, izveštaji.
- **Mrežna oprema** – stanje uređaja i lokacija, portovi i saobraćaj, koji računar je na kom portu, temperatura, restarti sa uzrokom.
- **MPLS** – primarni i rezervni link svake lokacije, saobraćaj uživo, obaveštenje o padu sa grafikonom.
- **Serveri** – CPU, RAM, diskovi i procesi Windows servera.
- **Održavanje** – log i backup konfiguracije switch-eva (SSH, samo administrator).

Grafikoni imaju izbor perioda (15 min – 30 dana ili proizvoljno) i preuzimanje u Excel.

## Mrežni saobraćaj i portovi

| Od | Do | Port / protokol | Čemu služi |
|---|---|---|---|
| pretraživač | konzola | TCP 8443 (HTTPS) | web interfejs i REST API |
| agent (računar, server) | konzola | TCP 8443 (HTTPS/WebSocket) | veza agenta, test brzine |
| konzola | računar, server | ICMP echo | dugme Ping (instalacija agenta otvara dolazni ping, samo Domain profil) |
| agent | agent | TCP, privremeni port | LAN test (pravilo za program agenta, samo Domain profil; port otvoren samo dok test traje) |
| konzola | mrežna oprema | ICMP echo | stanje uređaja (na 30 s) |
| konzola | mrežna oprema | UDP 161 (SNMP v2c/v3) | portovi, saobraćaj, MAC tabela, temperatura, MPLS (BGP, VRRP) |
| konzola | switch | TCP 22 (SSH) | Održavanje: `show logging`, `show running-config` |
| switch | konzola | UDP 5514 (syslog) | ko je pokrenuo restart (`logging host … transport udp port 5514`) |
| konzola | kontroler domena | Kerberos/NTLM (Windows) | provera naloga pre prijave na switch-eve |
| konzola | računari | WinRM (TCP 5985/5986) | instalacija agenata iz konzole (opciono) |
| konzola | mail server | SMTP (25/465/587) | e-mail i SMS obaveštenja |
| konzola | mail server | IMAP (993) | SMS upiti sa telefona (opciono) |
| konzola | Teams, ntfy | HTTPS | obaveštenja (opciono, može preko proxy-ja) |
| Android telefon | mail server | IMAP 993, SMTP 465 | prijem obaveštenja i slanje upita |

## Brza instalacija

1. **Build** (Windows, Go 1.24+ i .NET SDK 8+): `.\build.ps1 -Msi` → paketi u `dist\`.
   Gotovi paketi su i na GitHub-u, u build-u grane (Actions → Artifacts).
2. **Konzola:** na serveru pokrenite `NetworkMonitorConsole.msi`, otvorite
   *Network Monitor konzola* iz Start menija i postavite administratorsku lozinku.
   Zatim otpremite sertifikat iz internog CA (Podešavanja → TLS sertifikat).
3. **Agenti:** Podešavanja → *Instalacija agenata* (iz konzole, preko WinRM-a),
   ili ručno:
   `msiexec /i NetworkMonitorAgent.msi /qn CONSOLE_URL=https://<konzola>:8443 AGENT_TOKEN=<token>`
4. **Mrežna oprema:** SNMP nalog samo za čitanje na uređajima, pa uređaji u
   kartici Mrežna oprema (pojedinačno ili uvozom CSV-a).

## Uputstva

| Tema | Fajl |
|---|---|
| Build i instalacija (konzola, sertifikat, agenti) | [docs/instalacija.md](docs/instalacija.md) |
| Računari: merenja i tumačenje rezultata, REST API | [docs/racunari.md](docs/racunari.md) |
| Mrežna oprema, MPLS, temperatura, syslog, Održavanje | [docs/mrezna-oprema.md](docs/mrezna-oprema.md) |
| Serveri | [docs/serveri.md](docs/serveri.md) |
| Obaveštenja i SMS | [docs/obavestenja.md](docs/obavestenja.md), [android/README.md](android/README.md) |
| Bezbednost, korisnici i uloge | [docs/bezbednost.md](docs/bezbednost.md) |
| Za programere (struktura projekta) | [docs/razvoj.md](docs/razvoj.md) |
