# Za programere

## Struktura projekta

```
cmd/console/          konzola (Windows servis)
cmd/agent/            agent za računare (Windows servis, podešavanja iz registry-ja)
cmd/serveragent/      agent za servere
internal/protocol/    poruke između konzole i agenata, izdanja agenata
internal/agent/       logika agenta (veza, izvršavanje komandi, slanje stanja)
internal/agentcmd/    zajednički main agenata (servis, instalacija, podešavanja)
internal/nettest/     merenja: ICMP/TCP latencija, gateway, HTTP i LAN propusnost
internal/sysinfo/     stanje računara: CPU, RAM, diskovi, procesi, saobraćaj kartica
internal/console/     konzola: hub agenata, HTTP API, mrežna oprema (SNMP, MPLS,
                      temperatura, syslog), serveri, obaveštenja (e-mail, SMS,
                      Teams, ntfy), Održavanje (SSH), istorija (bbolt), web UI (web/)
android/              aplikacija NetMon SMS (Kotlin)
installer/            WiX izvori MSI paketa (konzola, agent, agent za servere)
build.ps1             build na Windows-u (exe, MSI, potpisivanje)
```

Pri izmeni agenta (`internal/agent`, `internal/nettest`, `internal/sysinfo`,
`internal/agentcmd`, `cmd/agent`, `cmd/serveragent`) koja treba da stigne do
računara povećajte izdanje agenta (`AgentRelease` / `ServerAgentRelease` u
`internal/protocol/protocol.go`) i upišite šta donosi; inače konzola neće
tražiti nadogradnju.

## Mogući dalji razvoj

- prijava na konzolu preko Active Directory naloga i grupa umesto lokalnih
  naloga konzole,
- poseban token po agentu i odobravanje novih agenata na konzoli,
- zakazana (periodična) merenja latencije i brzine na računarima,
- traceroute do zadatog cilja,
- baza (SQLite) umesto JSONL fajla za rezultate merenja kada njihov broj poraste,
- šifrovanje lozinki u `console.json` (Windows DPAPI).
