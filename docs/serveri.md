# Serveri

## Serveri (agent za servere)

Na Windows servere se instalira **poseban agent za servere**
(`NetworkMonitorServerAgent.msi`, servis `network-monitor-server-agent`):

```
msiexec /i NetworkMonitorServerAgent.msi /qn CONSOLE_URL=https://netmon.firma.local:8443 AGENT_TOKEN=<token>
```

ili iz konzole: Podešavanja → *Instalacija agenata* → Vrsta agenta
„Serveri” (isti postupak kao za računare: WinRM i administratorski nalog na
serverima; MSI konzole sadrži i MSI agenta za servere).

Agent na 30 s šalje uptime, zauzeće procesora, memorije i diskova i 5
procesa koji najviše opterećuju procesor (naziv, PID, CPU, RAM; bez komandne
linije), a između toga na 5 s samo CPU (prosek za tih 5 s) i RAM, za prikaz
u tabeli (izdanje agenta za servere 2 i novije; stranica se osvežava na 5 s).
Istorija (grafikoni) i obaveštenja „CPU/RAM visok” koriste stanje na 30 s.
Agent ne izvršava merenja mreže. Uz događaj „CPU/RAM visok” čuva se spisak
procesa iz tog trenutka. Serveri su na stranici **Serveri** (spisak,
detalji sa grafikonom CPU/RAM, diskovi, događaji) i ne
pojavljuju se među računarima. Obaveštenja (kolona „Serveri” u tabeli
kanala): server bez veze duže od 2 min (konzola ga tada pinguje – piše da li
je pao server ili samo agent), CPU ili RAM iznad 90% duže od 10 min, restart.
Pragovi se menjaju u kartici Obaveštenja. SMS upit `server` (svi) ili
`server <naziv>` ili `server <broj>` vraća pun izveštaj (stanje, uptime, CPU, RAM, diskovi sa slobodnim prostorom); `server lista`
vraća numerisan spisak svih servera iz konzole (i kada je spisak duži od jednog
SMS-a). Skraćenice
servera (`SOL = srv-monitor`, SMS „server SOL”) se upisuju u Obaveštenja →
SMS telefon → „Skraćenice servera”.
