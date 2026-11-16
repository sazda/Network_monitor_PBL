# Mrežna oprema, MPLS i Održavanje

## Mrežna oprema (switch-evi, ruteri)

Stranica **Mrežna oprema** (navigacija u zaglavlju) prati opremu na koju se
ništa ne instalira:

- konzola pinguje svaki aktivan uređaj na 30 s; uređaj je „nije na vezi” tek
  posle 3 uzastopne provere bez odgovora (~1,5 min);
- **lokacije**: uređaj sa ulogom „glavna veza” (npr. WAN/MPLS adresa rutera
  operatera) određuje da li lokacija radi preko rezervne veze (glavna ne
  odgovara, ostali uređaji odgovaraju) ili je nedostupna; tada stiže jedno
  obaveštenje za lokaciju, a ne po jedno za svaki uređaj;
- **SNMP** (samo čitanje, preporučeno v3 authPriv): model, serijski broj,
  verzija, uptime (prepoznaje restart), portovi, saobraćaj, greške i odbačeni
  paketi; grafikon po portu (period kao ispod);
- događaji (padovi, povratci, prekidi lokacija, restarti) i obaveštenja kroz
  postojeća podešavanja (e-mail, Teams, ntfy);
- dugme **Ping** kao kod računara, sa izveštajem u Rezultatima.

Uređaje dodaje administrator pojedinačno ili uvozom CSV-a
(`Lokacija;Mesto;Naziv;IP;Tip;Uloga;Aktivan;Napomena`). Spisak je u
`console-data\netdevices.json`, događaji u `netdev-events.jsonl`, istorija u
`netdev-history.db` (svako merenje 7 dana, 15-minutni proseci i maksimumi
31 dan; u istom fajlu je i istorija MPLS veza i servera).

**Restart uređaja: uzrok i ko ga je pokrenuo.** Kada konzola otkrije restart
(uptime manji nego ranije), čita Cisco razlog poslednjeg restarta (SNMP
`whyReload`, isto što i „Last reload reason” u `show version`) i dodaje ga u
događaj i obaveštenje, npr. `RESTART HQ_UZ_S-I (Glavni Centar) - nestanak ili
uključenje napajanja` ili `- softverski restart (reload)`. Ko je pokrenuo
restart konzola saznaje iz syslog poruke koju switch šalje neposredno pre
restarta (`%SYS-5-RELOAD: Reload requested by <nalog> on vty0 (<adresa>)`):
konzola prima syslog na **UDP 5514** (instalacija dodaje firewall pravilo za
Domain profil) i prihvata samo poruke sa adresa uređaja upisanih u konzolu. Na
svakom switch-u treba podesiti:

```
logging host <adresa konzole> transport udp port 5514
```

Tada obaveštenje glasi npr. `RESTART HQ_UZ_S-I (Glavni Centar) - restart,
pokrenuo FIRMA\korisnik sa 10.0.36.50` (bez SNMP razloga). C2960L posle
komande reload javlja „power-on”, isto kao posle nestanka struje, pa za njega
bez syslog poruke piše „uzrok nepoznat (nestanak struje ili reload)”. U događajima
uređaja ostaje i zapis „restart je zatražio …”. Syslog preko UDP-a nije
garantovan (poruka se retko može izgubiti); bez nje obaveštenje sadrži samo
uzrok. Port se menja u `console.json` (`"syslog_addr": ":5515"`; `"-"` isključuje
prijem; posle izmene restartovati servis i prilagoditi firewall pravilo).

**Temperatura switch-eva** (SNMP): C9200L preko ENTITY-SENSOR-MIB (nazivi
senzora i pragovi upozorenja i kritični pragovi iz CISCO-ENTITY-SENSOR-MIB,
isti kao u CLI-ju), stariji IOS (npr. C2960L) preko CISCO-ENVMON-MIB. Spisak
senzora i pragovi čitaju se jednom na sat, vrednosti pri svakom SNMP čitanju.
U tabeli je kolona **Temp.** (najviša temperatura kućišta; SFP moduli samo u
detaljima), žuta iznad praga upozorenja, crvena iznad kritičnog praga. U
detaljima switch-a su pločica, grafikon (period i Excel kao ostali) i spisak
senzora. Kada je temperatura iznad praga switch-a duže od 10 minuta, beleži
se događaj i šalje obaveštenje (uređaji sa uključenim obaveštenjima, kanali
za mrežnu opremu); obaveštenje stiže i kada se temperatura vrati u granice.
Pragove određuje switch; ako ih switch ne daje, temperatura se samo prikazuje.

**Grafikoni istorije** (Mrežna oprema, MPLS, Serveri): period 15 min, 1 h,
6 h, 24 h, 7 dana, 30 dana ili **Proizvoljno** (od – do, datum i vreme).
Za period do 24 h unutar poslednjih 7 dana prikazuju se sva merenja, inače
15-minutni proseci (ispod grafikona piše „proseci po 15 min”). Pored svakog
grafikona je **„Preuzmi (Excel)”**: prikazani period kao .xlsx (vreme kao
Excel datum, vrednosti sa jedinicama u zaglavlju – ms, Mb/s, %; za proseke i
maksimume i gubitak, greške i odbačene pakete).
SNMP podešavanja (sa lozinkama) su u `console.json`. Za uređaje sa drugim
SNMP podacima (npr. operater CE ruteri, v2c) postoje **dodatni SNMP nalozi**
(kartica SNMP → „+ Dodaj SNMP nalog”); u Izmeni uređaja bira se „SNMP nalog”.

Primer SNMPv3 naloga samo za čitanje na Cisco IOS-XE, dozvoljenog samo sa
adrese konzole (postojeći community-ji i drugi sistemi se ne diraju):

```
ip access-list standard NETMON-SNMP
 permit <adresa konzole>
snmp-server view NETMON-RO iso included
snmp-server group NETMON v3 priv read NETMON-RO access NETMON-SNMP
snmp-server user netmon NETMON v3 auth sha <lozinka-1> priv aes 128 <lozinka-2> access NETMON-SNMP
```

Konzola mora da dosegne uređaje na ICMP i UDP 161.

### Wi-Fi računari (Cisco WLC)

Kada AP radi preko WLC-a, saobraćaj računara ide tunelom do kontrolera, pa
switch ne zna na kom je AP-u računar. Ako je WLC (Cisco AireOS, npr. 3504)
dodat kao uređaj vrste **Wi-Fi kontroler** sa SNMP-om, konzola na 5 min čita
njegovu tabelu klijenata (AIRESPACE-WIRELESS-MIB: klijent → AP, SSID) i:

- u detaljima switch-a, na portu na kome je AP (CDP/LLDP sused), navodi
  računare sa agentom koji su na Wi-Fi-ju preko tog AP-a („📶 Wi-Fi · naziv
  AP-a”; brzina u tom redu je ukupan saobraćaj AP-a);
- u detaljima WLC-a prikazuje broj klijenata i AP-ova i spisak računara sa
  agentom i AP-a na kome su.

SNMPv3 nalog na WLC-u: Management → SNMP → SNMP V3 Users (isti korisnik i
lozinke kao na switch-evima; AireOS podržava HMAC-SHA i AES-128).

Brzina u delu „Računari na ovom switch-u” je brzina same mrežne kartice
računara: agent (izdanje 2 i novije) je šalje konzoli na 10 s. Za agenta
starije verzije prikazan je saobraćaj porta, sivom bojom, sa oznakom „port”
(kod Wi-Fi računara ceo AP); prelaz mišem prikazuje obe vrednosti.

## MPLS i Održavanje

Kartica **MPLS** (pored Mrežne opreme): veze lokacija sa Glavnim Centrom
preko operatera. Na svakoj lokaciji su dva operater CE rutera u VRRP-u –
primarni i sekundarni (rezervni) link, sa vrstom linka (optika ili
radio-relejni) koja se upisuje uz ruter. CE ruter se dodaje
dugmetom „Dodaj CE ruter” (ili u Izmeni uređaja, polje „MPLS link”), sa
dodatnim SNMP nalogom za operater rutere, WAN interfejsom (prazno = onaj
čiji opis sadrži „WAN”, npr. `Gi0/0/0.2804 WAN-L3VPN`; ako takvog nema,
jedini podinterfejs fizičkog porta, npr. `Gi0/0/1.2700`; ako ih je više, bira
se ručno iz spiska interfejsa rutera) i ugovorenom brzinom.
CE ruteri se ne prikazuju u Mrežnoj opremi.

Konzola na 30 s čita sa CE rutera: stanje WAN interfejsa, BGP sesije ka
operateru (BGP4-MIB) i VRRP stanje (VRRP-MIB), i saobraćaj na WAN
interfejsu (↓ ka lokaciji, ↑ sa lokacije, % od ugovorene brzine). Link „ne
radi” kada WAN interfejs nije aktivan ili BGP sesija nije uspostavljena dva
čitanja zaredom, ili kada ruter ne odgovara na ping. Aktivni link se
određuje po VRRP-u (ruter koji je master za sve VRRP grupe), inače po
saobraćaju. Ruter sa više VRRP grupa koji je master samo za deo njih
prikazuje se kao „VRRP master 3/8”. Dok je kartica MPLS otvorena,
saobraćaj i stanje WAN interfejsa svih CE rutera koji odgovaraju na ping
čitaju se na 5 s i u tabeli se prikazuje prosek za 10 s (BGP i VRRP i
dalje na 30 s; jedan ruter se ne čita češće od 4 s i kada je konzola
otvorena kod više korisnika). Klik na lokaciju
prikazuje grafikon zauzeća oba linka; klik na naziv rutera
otvara njegove detalje u istoj kartici (stanje, BGP, VRRP, model, verzija,
interfejsi, Ping, Proveri SNMP, Izmeni) sa zauzećem WAN interfejsa uživo
(očitavanje na 5 s dok su detalji otvoreni, prikazuje se prosek za 10 s). Stanje lokacije u
Mrežnoj opremi (radi / preko rezervne veze / nedostupna, sa obaveštenjima)
za lokacije sa MPLS vezom računa se iz ovih podataka; uloge „glavna/rezervna
veza” ostalih uređaja tada se ne koriste.

**Obaveštenje o padu i povratku MPLS linka** (primarnog ili sekundarnog, sa
razlogom: WAN interfejs, BGP) šalje se za CE rutere sa uključenim
obaveštenjima, preko kanala za mrežnu opremu (SMS: `MPLS Dolina: sekundarni MPLS
link (…) ne radi - BGP …`). E-mail je HTML sa tabelom (stanje oba linka,
preko kog linka lokacija radi) i grafikonom (PNG) saobraćaja i odziva oba
linka: pri padu za poslednjih 20 minuta, pri povratku za ceo prekid (od 5
minuta pre pada). Za e-mail uključite kanal E-mail sa kolonom „Mrežna oprema”.

Kartica **Održavanje** (samo administrator): preuzimanje loga (`show
logging`) i backup-a konfiguracije (`show running-config`) sa Cisco switch-eva
C9200L i C2960L (model iz SNMP-a), kao fajl na računar. Konzola se prijavljuje
preko SSH-a nalogom koji administrator upiše (domenski nalog; switch ga
proverava preko AAA); nalog i lozinka se ne čuvaju, a u logu konzole se
beleži ko je šta preuzeo. Izvršavaju se samo te dve komande; backup-i se ne
čuvaju u konzoli. Pri prvoj prijavi konzola pamti SSH ključ switch-a
(`ssh-known-hosts.json`); promenjen ključ se odbija dok ga administrator ne
obriše (dugme posle zamene switch-a). Potreban je SSH (TCP 22) od konzole do
switch-eva.

Pre prijave na switch konzola proverava nalog i lozinku u domenu (Windows
`LogonUser`; server konzole mora biti član domena): pogrešna lozinka troši
jedan neuspeli pokušaj u AD-u, a switch-evi se ne kontaktiraju, pa se nalog ne
zaključava. Nalog se upisuje sa domenom (`FIRMA\nalog` ili `nalog@firma.local`).
Uspešna provera se pamti 5 minuta, samo u memoriji i bez lozinke. Ako
kontroler domena nije dostupan, radnja se ne izvršava.

Više switch-eva odjednom: izaberite ih u tabeli (polje u zaglavlju bira sve
prikazane, npr. posle pretrage po lokaciji) i kliknite „Preuzmi log za
izabrane” ili „Preuzmi backup za izabrane”. Konzola preuzima sa 4 switch-a
istovremeno i prikazuje napredak; na kraju se preuzima jedan ZIP sa fajlom za
svaki switch i `izvestaj.txt` (šta je uspelo, šta nije i zašto). Ako neki
switch odbije nalog, preuzimanje se prekida i ostali switch-evi se ne
pokušavaju. Switch sa promenjenim SSH ključem se preskače; ključ se zaboravlja
pojedinačnom radnjom za taj switch.
