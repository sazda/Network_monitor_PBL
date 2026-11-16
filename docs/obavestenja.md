# Obaveštenja (e-mail, SMS, Teams, ntfy)

## Obaveštenja kada računar nije na vezi (i na telefonu)

**Podešavanja → Obaveštenja** (samo administrator). Kada računar nije na vezi
duže od zadatog vremena (podrazumevano 5 min), konzola šalje jedno
obaveštenje; više računara koji odu u isto vreme (npr. pala lokacija) stiže u
jednoj poruci. Opciono stiže i poruka kada se računar vrati. Kratki prekidi
(restart, kratak gubitak Wi-Fi-ja) ne šalju ništa. Ako konzola bude
restartovana, računari koji se ne vrate u roku takođe izazivaju obaveštenje.

Računari se gase svako veče, zato je podrazumevano da se prate samo **označeni**
računari: dugme 🔔 u koloni Akcije (označeni imaju 🔔 pored naziva). Može se
izabrati i praćenje svih računara.

**Sva obaveštenja** je glavni prekidač: kada je isključen, ne šalje se ništa.
Svaki kanal ima svoj prekidač i bira za šta se koristi – **računari** i/ili
**mrežna oprema** (tabela kanala). Dugme **Proba** u redu kanala šalje probnu
poruku i kada je kanal isključen; pored kanala piše da li se zaista šalje
(npr. „ne šalje se – glavni prekidač je isključen”), poslednje slanje i
poslednja greška.

| Kanal | Na telefonu | Potrebno |
|---|---|---|
| E-mail | Outlook aplikacija (push obaveštenje) | interni SMTP relay (Exchange) koji prima poštu sa servera konzole |
| Microsoft Teams | Teams aplikacija | Workflows webhook i izlaz servera ka internetu (Microsoft) |
| ntfy | ntfy aplikacija (Android/iOS) | ntfy.sh ili sopstveni ntfy server; izlaz ka internetu |
| SMS | Android telefon sa aplikacijom NetMon SMS | SMTP server (isti kao za e-mail) i sanduče koje telefon čita |

- **E-mail:** SMTP server i port (25; 587 sa prijavom; 465 = TLS od početka),
  pošiljalac i primaoci. Ako server nudi STARTTLS, veza se šifruje i sertifikat
  se proverava. Prijava (nalog i lozinka) je dozvoljena samo preko šifrovane
  veze. Na Exchange-u je obično potrebno dozvoliti relay sa IP adrese servera
  konzole (Receive Connector) ili koristiti nalog za slanje.
- **Teams:** u kanalu ili četu → *Workflows* → šablon „Post to a channel when a
  webhook request is received” (ili „…to a chat” – lično obaveštenje), adresu
  toka nalepite u konzolu. Stari „Incoming Webhook” konektori su ukinuti.
- **ntfy:** instalirajte aplikaciju ntfy, pretplatite se na temu i istu adresu
  (npr. `https://ntfy.sh/netmon-<dugačko-slučajno-ime>`) unesite u konzolu.
  Na javnom ntfy.sh tema nije zaštićena lozinkom (ko zna ime, vidi poruke), a
  poruke sadrže nazive računara i lokacija; zato koristite dugačko slučajno ime
  ili sopstveni ntfy server sa access token-om.
- Ako server izlazi na internet preko proxy-ja, upišite ga u polje *HTTP proxy*.

Lozinka za SMTP i ntfy token čuvaju se u `console.json` (pristup imaju samo
SYSTEM i administratori servera) i nikada se ne vraćaju u web interfejs.
Ishod poslednjeg slanja i poslednja greška vide se ispod podešavanja i u logu
konzole.

## SMS obaveštenja (Android telefon)

Za mrežnu opremu konzola može da šalje i SMS: kratka poruka (do 160 znakova)
ide e-mailom na posebno sanduče, a Android telefon sa aplikacijom NetMon SMS
je prosleđuje kao SMS na brojeve upisane u aplikaciji. Svako jutro stiže
kontrolni SMS „NetMon SMS veza OK”. SMS upiti na broj NetMon telefona (sa
broja iz aplikacije): „status” vraća spisak komandi; „lokacija lista”
numerisan spisak lokacija sa mrežnom opremom; „lokacija <broj | naziv |
skraćenica>” pun izveštaj za lokaciju; „server lista” i „server <broj |
naziv | skraćenica>” isto za servere.
Podešavanje konzole i telefona, kao i build APK-a:
[android/README.md](../android/README.md).
