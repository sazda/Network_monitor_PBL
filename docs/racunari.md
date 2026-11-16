# Računari: merenja

Merenja na računarima sa agentom: latencija, ping, brzina, LAN test.

## Šta se meri na računarima

| Dugme | Merenje | Šta zapravo pokazuje |
|---|---|---|
| **Latencija** | agent → zadati cilj | min/avg/max, jitter, gubitak paketa. Cilj može biti konzola (podrazumevano), IP adresa (ICMP ping) ili `host:port` (TCP). |
| **Ping** | konzola → računar (ICMP), zadato trajanje | Ping računara sa konzole od 30 s do 8 h, sa izveštajem u Rezultatima (HTML/CSV) i spiskom prekida. Radi i kada agent nije na vezi. |
| **Brzina** | agent ↔ konzola (HTTP) | Download/upload između računara i servera sa konzolom. |
| **LAN test** | agent ↔ drugi agent (TCP) | Propusnost između dva računara, bez učešća servera. |
| — | konzola ↔ agent | RTT kontrolnog kanala, kontinualno na svakih 25s (kolona u tabeli). |

### Kako tumačiti rezultate

1. **Test „Brzina” je ograničen i samim serverom.** Rezultat nikad ne može biti
   veći od najsporijeg dela puta, a u njemu su i virtuelna mašina i hipervizor:
   - tip virtuelne mrežne kartice — koristite paravirtuelnu karticu
     (**VMXNET3** na VMware-u, **VirtIO** na KVM/Proxmox/Nutanix i sl.);
     emulirane kartice (E1000/E1000E, Intel PRO/1000) su sporije i opterećuju CPU;
   - fizički uplink hipervizora, koji VM deli sa ostalim VM-ovima;
   - broj vCPU (na 1 vCPU test od nekoliko Gb/s može da zaguši procesor).

   Ako svi računari pokazuju isti „plafon”, najverovatnije je usko grlo server,
   a ne mreža. Tada uporedite sa **LAN testom** između dva računara.
2. **Između VLAN-ova saobraćaj ide kroz L3 svič ili firewall.** I latencija i
   brzina između računara u različitim VLAN-ovima uključuju i taj uređaj, što je
   često baš ono što želite da proverite. ACL-ovi između VLAN-ova mogu da
   blokiraju ICMP ili port LAN testa — tada će merenje prijaviti grešku.
3. **ICMP vs TCP.** ICMP ping (Windows `IcmpSendEcho`, ne traži administratorska
   prava) radi prema svim uređajima koji odgovaraju na ping. TCP meri vreme
   uspostavljanja veze prema otvorenom portu (npr. `fileserver:445`) i prolazi i
   kada je ICMP blokiran. Windows računari podrazumevano **ne odgovaraju** na
   ping, pa za njih koristite TCP ili LAN test.
4. **Testovi brzine zagušuju vezu.** Ako ih pokrenete istovremeno na više
   računara, oni dele isti link servera i rezultati su lažno niski, a korisnici
   osete usporenje. Pokrećite ih jedan po jedan, po mogućstvu van radnog vremena.

## Korišćenje

Otvorite `https://netmon.firma.local:8443` i prijavite se. Na stranici su:

- spisak agenata (na vezi / nije, IP adrese, gateway, RTT kontrolnog kanala),
- dugmad **Latencija**, **Ping**, **Brzina** i **LAN test** za svakog agenta,
- parametri merenja (cilj i metoda latencije, trajanje, broj konekcija, smer),
- istorija rezultata (klik na red prikazuje sve detalje),
- **Podešavanja** (dugme gore desno): token i komanda za instalaciju agenata,
  TLS sertifikat (otpremanje PFX-a), korisnici i promena sopstvene lozinke.

### Lokacije, pretraga i filter

Iznad spiska agenata su pretraga (naziv računara, IP adresa, gateway…), filter
po jednoj ili više lokacija (čekboksovi) i „samo na vezi”. Lokacije definiše administrator u
**Podešavanja → Lokacije**, jedna po redu:

```
Centrala = 10.0
VPN      = 10.0.128
Filijala = 10.18, 10.19.5
```

Početak adrese se piše po oktetima (`10.18` = 10.18.x.x) ili kao CIDR
(`10.0.128.0/17`). Kada se opsezi preklapaju, važi uži (VPN `10.0.128` ispred
`10.0`). Lokacija se određuje po adresi sa koje se računar povezao na konzolu,
a ne po gateway-u, jer je kod VPN korisnika gateway obično kućni ruter.

Spisak prikazuje 9 računara po strani (sortirano po nazivu); kada ih ima više,
ispod tabele se pojavljuju strane („‹ Prethodna”, brojevi strana, „Sledeća ›”).
Strana se ne menja pri automatskom osvežavanju spiska, a promena pretrage ili
filtera vraća prikaz na prvu stranu.

Istorija merenja (**Rezultati**) prikazuje 10 merenja po strani, najnovija
prvo. Pretraga po nazivu računara nalazi i LAN testove u kojima je taj računar
bio drugi učesnik; više reči znači da sve moraju da se poklope (npr.
`milosm smiljam` = LAN testovi između ta dva računara). Pretražuje se
poslednjih 5000 merenja koja konzola drži u memoriji; cela istorija je u
`results.jsonl` u folderu sa podacima.

### Širina i izbor kolona

U tabelama **Agenti** i **Rezultati** širina kolone se menja prevlačenjem
desne ivice zaglavlja kolone (dupli klik na ivicu vraća automatsku širinu).
Meni **Kolone** (desno iznad tabele) sakriva i prikazuje kolone i vraća
podrazumevane širine; kolone Računar/Akcije, odnosno Vreme/Agent, ne mogu se
sakriti. Podešavanja se pamte u pretraživaču (važe na tom računaru i u tom
pretraživaču).

### Verzija agenta i nadogradnja

Ispod operativnog sistema u spisku agenata piše verzija agenta (oznaka
build-a). Nadogradnja se ne traži po toj oznaci, nego po **izdanju agenta**:
broju koji se povećava samo kada se agent promeni tako da nadogradnja nešto
donosi (`protocol.AgentRelease` i `protocol.ServerAgentRelease` u
`internal/protocol/protocol.go`). Nova verzija konzole zato ne znači da
agenti moraju da se nadograde. Agenti iz verzija pre uvođenja izdanja
nadograđuju se jednom.

Agent čije je izdanje manje od očekivanog obojen je narandžasto, sa oznakom
„⚠ potrebna nadogradnja” (prelaz mišem objašnjava razlog), a iznad spiska je
obaveštenje sa brojem takvih agenata (za servere: oznaka u detaljima
servera). Dugme **Pripremi nadogradnju** (administrator) otvara *Instalacija
agenata* sa već upisanim računarima **koji su na vezi** (ostali se
preskaču, uz poruku koliko ih je) i uključenom opcijom „samo nadogradnja”;
ostaje da se unese administratorski nalog. Isto radi i dugme
„Dodaj računare sa starom verzijom agenta” u tom delu podešavanja.

### Merenje uživo (skazaljka)

Desno od spiska agenata (na užem ekranu ispod njega) je panel **Merenje
uživo**: tokom testa „Brzina” ili „LAN test” skazaljka i broj pokazuju
trenutnu brzinu (prosek poslednje sekunde), posebno za download (plavo, ↓) i
upload (narandžasto, ↑), a ispod je grafikon toka merenja (prelaz mišem
prikazuje vrednost u tom trenutku). Legenda ispod grafikona pokazuje smer sa
nazivima računara (npr. `RAW-PC → FIN-PC`). Po završetku ostaju konačni
rezultati: download, upload, latencija i gubitak paketa, a dugme **Ponovi
test** pokreće isti test (isti računari i parametri) ponovo. Svaki korisnik u panelu vidi svoja
merenja; merenja drugih korisnika se prikazuju kada se uključi „prikaži i
merenja drugih korisnika” (dok je to isključeno, panel samo napomene da su
tuđa merenja u toku).

**Više korisnika istovremeno:** svako može da pokreće svoja merenja nezavisno.
Jedan računar se ne može meriti dva puta u isto vreme (oba rezultata bi bila
pogrešna): dok traje merenje, njegova dugmad su onemogućena i kod drugih
korisnika (uz oznaku ko je pokrenuo merenje), a konzola takav zahtev odbija.
Ako u isto vreme teku dva testa brzine na različitim računarima, panel upozori
da rezultati mogu biti niži ako testovi dele istu vezu (npr. ka konzoli).

Gubitak paketa je izmeren na 5 probnih paketa (TCP konekcija) neposredno pre
testa brzine, pa pokazuje samo grube probleme (svaki izgubljen paket = 20%);
gubici tokom samog prenosa se ne vide, jer ih TCP sam nadoknađuje (oni se
vide kao manja brzina). Za precizniji gubitak pokrenite **Latencija** ili
**Ping** u dužem trajanju.

- **Brzina (računar ↔ konzola):** podaci prolaze kroz konzolu, pa ona sama meri
  trenutnu brzinu — radi sa svim verzijama agenta.
- **LAN test (računar ↔ računar):** podaci ne prolaze kroz konzolu, pa
  međurezultate šalje agent koji meri. Za to je potrebna nova verzija agenta
  (nadogradnja iz konzole: *Instalacija agenata* → „samo
  nadogradnja”). Sa starijim agentom vidi se samo konačan rezultat.

Traka iznad skazaljke pokazuje koliko je testa završeno (procenat i preostalo
vreme). Po završetku traka postaje zelena („✓ Test završen”), skazaljka se
vraća na nulu, a rezultat ostaje kao broj i u poljima ispod.

### Ping u trajanju i izveštaj

Dugme **Ping** kod računara pokreće ping **tog računara sa konzole**: konzola
šalje ICMP ping na adresu sa koje se računar javlja konzoli, pa adresu ne treba
upisivati. Radi i kada agent nije na vezi (računar može da radi i bez agenta).
U prozoru se bira trajanje – ponuđeno (30 s – 8 h), **proizvoljno** (sati,
minuti, sekunde; od 5 s do 24 h) ili **neprekidno** (kao `ping -t`: traje dok
ga ne zaustavi korisnik koji ga je pokrenuo ili administrator, dugmetom
„Zaustavi ping” u panelu Merenje uživo; iz bezbednosnih razloga se sam
zaustavlja posle 24 h) – i razmak između pingova (0,5 – 10 s). Ping sa
konzole ne blokira ostala merenja na tom računaru (samo drugi ping istog
računara). Pri gašenju ili restartu servisa konzole pingovi u toku se
zaustavljaju, a njihovi dotadašnji rezultati čuvaju. Ping teče na serveru, pa se
pretraživač može zatvoriti; razmak se računa od početka prethodnog pinga, pa
izgubljeni paketi ne produžavaju ukupno trajanje. Dok traje, panel **Merenje
uživo** prikazuje grafikon, napredak i statistiku, a ping može da zaustavi
korisnik koji ga je pokrenuo ili administrator (dotadašnji rezultat se čuva).

**Izveštaj je u tabeli Rezultati:** klik na red pinga (ili merenja latencije)
otvara izveštaj na samoj stranici – ocena, statistika, grafikon, prekidi
(uzastopno izgubljeni paketi, sa vremenom) i objašnjenje. Kada se ping koji ste
pokrenuli završi, izveštaj se sam otvori. Izveštaj vide i preuzimaju **svi
korisnici** (i operateri), bez obzira ko je pokrenuo merenje:

- **HTML** – za čitanje i štampu; Ctrl+P → „Sačuvaj kao PDF” daje PDF.
- **CSV** – sažetak i svaki ping (redni broj, vreme, odgovor u ms, status), sa
  `;` i decimalnim zarezom, za srpska podešavanja Excel-a.

Izveštaj je dostupan za poslednjih 5000 merenja.

**Firewall:** Windows podrazumevano ne odgovara na ping. Nova verzija MSI-ja
agenta dodaje pravilo „Network Monitor Agent (ping)” (dolazni ICMPv4 echo,
samo domenski profil). Za računare sa starijim agentom ping se može dozvoliti
nadogradnjom agenta ili GPO-om (pravilo „File and Printer Sharing (Echo Request
– ICMPv4-In)”); inače ping pokazuje 100% gubitka iako računar radi.

Za skripte: `POST /api/agents/{id}/ping` sa `{"duration_sec":60,"interval_ms":1000}`
(ping sa konzole) ili sa `"source":"agent","target":"10.0.0.1"` (ping sa agenta
ka zadatoj adresi); neprekidni ping: `{"continuous":true,"interval_ms":1000}`, zaustavljanje:
`POST /api/live/{id}/stop`; izveštaj: `GET /api/results/{id}/report.html|csv|json`.

**Latencija i Ping** se prikazuju u istom panelu: grafikon vremena odgovora
za svaki paket (✕ = izgubljen paket, isprekidana linija = prosek; prelaz mišem
pokazuje pojedinačni paket), prosek, min, max, jitter, 95. percentil i gubitak,
ocena (Odlično / Dobro / Prihvatljivo / Loše, po uobičajenim preporukama za
glas i video: latencija < 150 ms, jitter < 30 ms, gubitak < 1%) i objašnjenje
rezultata na srpskom (šta je očekivano do gateway-a ili konzole, Wi-Fi, skokovi,
gubici). Graf se crta paket po paket sa novom verzijom agenta; sa starijim
agentom pojavljuje se kada se merenje završi (i tada se ne zna koji su tačno
paketi izgubljeni, samo koliko).

### LAN test

Kliknite „LAN test” kod računara koji meri. Otvara se prozor u
kome se drugi računar bira pretragom (naziv ili IP adresa) i po lokaciji
(prikazani su samo računari na vezi; Enter bira prvi sa spiska). Zatim se
podešavaju broj merenja latencije, trajanje testa brzine, broj paralelnih
konekcija i smer (početne vrednosti su iz „Parametara merenja”). Konzola bira
IP adresu drugog računara ovim redom: adresa sa koje se on povezao na konzolu
→ adresa u istoj podmreži → prva privatna IPv4 adresa. Time se izbegavaju
adrese virtuelnih (Hyper-V, VMware Workstation) i VPN adaptera. Po potrebi IP
se može zadati ručno (u prozoru, „Napredno”). Broj merenja latencije za LAN
test poštuje samo nova verzija agenta; starija uvek meri 5 puta.

## REST API

Sve rute zahtevaju prijavu (za skripte: HTTP Basic); POST zahtevi moraju imati
`Content-Type: application/json`. Primeri (PowerShell 7):

```powershell
$cred = Get-Credential admin
$api  = "https://netmon.firma.local:8443/api"
Invoke-RestMethod "$api/agents" -Authentication Basic -Credential $cred
Invoke-RestMethod "$api/agents/<id>/latency" -Method Post -Authentication Basic -Credential $cred `
  -ContentType application/json -Body '{"target":"gateway","method":"icmp","count":20}'
Invoke-RestMethod "$api/agents/<id>/speedtest" -Method Post -Authentication Basic -Credential $cred `
  -ContentType application/json -Body '{"direction":"both","seconds":5,"streams":4}'
Invoke-RestMethod "$api/lan-test" -Method Post -Authentication Basic -Credential $cred `
  -ContentType application/json -Body '{"client_id":"<id1>","server_id":"<id2>","seconds":5}'
Invoke-RestMethod "$api/results?agent=<id>&limit=50" -Authentication Basic -Credential $cred
Invoke-RestMethod "$api/results?q=INF-PC01&limit=10&offset=10" -Authentication Basic -Credential $cred
```

`/api/results` prima i `q` (pretraga po nazivu računara) i `offset` (preskače
toliko najnovijih merenja); ukupan broj pogodaka je u zaglavlju `X-Total-Count`.
