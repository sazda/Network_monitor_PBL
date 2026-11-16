# NetMon SMS – Android telefon koji prosleđuje obaveštenja kao SMS

Konzola šalje kratka obaveštenja o mrežnoj opremi (pad uređaja, lokacija na
backup vezi, oporavak) e-mailom na posebno sanduče. Aplikacija na telefonu
prati to sanduče preko IMAP-a (IDLE – poruka stiže za nekoliko sekundi) i
tekst šalje kao SMS na brojeve upisane u aplikaciji.

```
konzola ──SMTP──► sanduče (npr. netmon@firma.rs) ◄──IMAP (TLS)── telefon ──SMS──► dežurni
```

Ograničenje: ako ne radi mail server ili veza konzole do njega, SMS ne stiže.
Zato konzola svako jutro šalje kontrolnu poruku; telefon na nju šalje
administratoru SMS „NetMon SMS veza OK”. Ako taj SMS izostane, lanac ne radi.

## 1. Konzola

Podešavanja → kartica Obaveštenja:

1. Deo „SMTP server”: server, port, pošiljalac i nalog (primaoci običnog
   e-maila nisu obavezni ako se koristi samo SMS).
2. Deo „SMS telefon”: adresa sanduča (npr. `netmon@firma.rs`) i vreme dnevne
   provere (npr. 08:00).
3. U tabeli kanala: SMS „Uključen” i „Mrežna oprema” (po želji i
   „Računari”). Glavni prekidač „Sva obaveštenja uključena” mora biti
   označen – u koloni „Stanje” mora pisati „šalje se: …”. Za uređaje u
   „Mrežna oprema” mora biti uključeno „Obaveštenja”.
4. „Proba” u redu SMS – kada je telefon podešen, SMS stiže na sve brojeve.

Podrazumevano SMS ide samo za mrežnu opremu i lokacije. Tekst je do 160
znakova, bez naših slova (č → c, đ → dj), da bi stao u jedan SMS.

## 2. Telefon

Preporuka: poseban telefon samo za ovu namenu.

- SIM sa paketom SMS poruka i mobilnog interneta; PIN za SIM po želji.
- Bez drugih naloga i aplikacija; zaključan ekran.
- Stalno na punjaču, na vidljivom i nezapaljivom mestu (stara baterija koja
  je stalno na punjaču može da se naduva). Ako telefon nudi „ograniči punjenje
  na 80%”, uključite.

Instalacija:

1. Preuzmite `netmon-sms-N.apk` sa GitHub-a (Actions → Build → Artifacts) i
   prebacite ga na telefon (USB kabl ili e-mail).
2. Otvorite APK; Android će tražiti dozvolu za instalaciju iz tog izvora.
3. Otvorite „NetMon SMS” i unesite:
   - Server: `webmail.firma.rs`, port `993`
   - Nalog i lozinka sanduča (lozinka se čuva šifrovano u Android Keystore-u)
   - Adresa konzole: tačno ona iz polja „Pošiljalac” u konzoli. Poruke sa
     drugih adresa se ne šalju (i ako imaju oznaku `[NETMON-SMS]`).
   - Brojevi za obaveštenja (jedan po redu, npr. `+38164…`)
   - Broj administratora (dnevna provera i upozorenja aplikacije)
   - Najviše SMS obaveštenja na sat (podrazumevano 20)
   - Do kada mora stići dnevna provera konzole (podrazumevano 08:30)
4. Dozvolite slanje SMS-a i obaveštenja. Ako Android ne nudi dozvolu za SMS
   („ograničeno podešavanje” za ručno instalirane aplikacije): Podešavanja →
   Aplikacije → NetMon SMS → ⋮ → „Dozvoli ograničena podešavanja”, pa ponovo.
5. „Isključi štednju baterije za aplikaciju” → Dozvoli. Na Samsung, Xiaomi i
   sličnim telefonima dodatno: Podešavanja → Aplikacije → NetMon SMS →
   Baterija → „Bez ograničenja” (nazivi se razlikuju po proizvođaču).
6. „Proveri vezu sa sandučetom” – u dnevniku mora pisati „Prijava uspela”.
7. „Pošalji probni SMS administratoru”.
8. „Sačuvaj i pokreni” – u traci obaveštenja stoji „NetMon SMS radi”.
9. Iz konzole „Proba” u redu SMS (Podešavanja → Obaveštenja).

Kako radi:

- Servis radi stalno (obaveštenje „NetMon SMS radi”), sam se pokreće posle
  restarta telefona, a na 15 minuta se proverava da li radi.
- Obrađena poruka se označi kao pročitana i premesti u folder
  `NetMon-poslato` u sanduču (na serverima kao cPanel/Dovecot:
  `INBOX.NetMon-poslato`). Ista poruka se nikad ne šalje dvaput.
- Ako dnevna provera konzole ne stigne do zadatog vremena (polje „Dnevna
  provera konzole mora stići do”, podrazumevano 08:30), telefon sam šalje
  administratoru SMS „dnevna provera nije stigla”. To radi i kada telefon
  nema internet. Vreme mora biti posle vremena provere u konzoli.
- Poruke starije od 60 minuta se ne šalju (npr. posle dužeg prekida).
- Ako SMS nije poslat ni na jedan broj (nema signala), poruka ostaje i šalje
  se pri sledećem pokušaju.
- Preko ograničenja na sat poruke se ne šalju; administrator dobija jedno
  upozorenje.

## SMS upiti („status” = spisak komandi)

Sa broja koji je u aplikaciji (brojevi za obaveštenja ili broj
administratora) pošaljite SMS na broj NetMon telefona:

| SMS | Odgovor (primer) |
|---|---|
| `status` | spisak komandi, jedna po redu (isto i za nepoznatu komandu) |
| `lokacija lista` | `LOKACIJE 10:42 (8):` pa jedna lokacija po redu (`1 Filijala`, `2 Istok (KV)`, …) – lokacije koje trenutno imaju mrežnu opremu u konzoli, abecednim redom |
| `lokacija 3`, `lokacija PS`, `lokacija Glavni Centar` | `LOKACIJA Glavni Centar 10:42: sve operativno (5/5 na vezi)` – pun izveštaj, bez ograničenja dužine |
| `lokacija SU` | `LOKACIJA Sever 10:42: radi preko REZERVNE veze (MPLS ne radi); MPLS ruter offline od 09:12 (2/3 na vezi)` |
| `lokacija` | `LOKACIJE 10:42: problemi - SU rezervna veza; NI 1 offline` (kratak pregled) |
| `server` | `SERVERI 10:42: 5/6 online; offline: APP02 od 09:12` |
| `server SQL01` | `SERVER SQL01 10:42: online, uptime 12d 4h, CPU 23%, RAM 61% (9.8 GB/16 GB), disk C: 72% (slobodno 28 GB), D: 20% (slobodno 400 GB)` |
| `server SOL` | isto, za skraćenicu `SOL = srv-monitor` |
| `server lista` | `SERVERI 10:42 (3):` pa jedan server po redu (`1 APP02`, `2 srv-monitor`, …) – svi serveri iz konzole, abecednim redom, i kada spisak ne staje u jedan SMS (šalje se kao jedna spojena poruka od više delova; potrebna je aplikacija iz istog ili novijeg build-a) |
| `server 3` | pun izveštaj za server broj 3 sa spiska |
| `mpls lista` | još nije dostupno |

Skraćenice lokacija se podešavaju u konzoli (Obaveštenja → SMS telefon →
„Skraćenice lokacija”, npr. `PS = Glavni Centar`); podrazumevano Filijala, PS,
ZE, NS, SU, KV, NI, ZA. Skraćenice servera su odmah ispod („Skraćenice
servera”, npr. `SOL = srv-monitor`); pun naziv servera radi i bez njih. Radi i pun naziv ili njegov početak („subot”);
velika i mala slova i naša slova nisu bitni. Telefon šalje upit konzoli e-mailom (`[NETMON-UPIT]`,
SMTP 465, isti nalog), konzola čita sanduče (IMAP 993) na 20 sekundi i
odgovara; odgovor ide samo broju koji je pitao. Odgovor obično stiže za
20–60 sekundi; ako ne stigne za 3 minuta, telefon javi da konzola nije
odgovorila. Telefon konzoli prosleđuje svaki SMS sa brojeva iz aplikacije (na
nepoznatu komandu konzola odgovara spiskom komandi). Najviše 10 upita na sat
po broju; upozorenje „previše upita” ide jednom u satu. SMS sa drugih brojeva
se ignoriše.

Spiskovi i spisak komandi stižu u više redova: konzola u tekst stavlja
oznaku `\n`, a aplikacija (build 36 i noviji) je pretvara u novi red. Zato
prvo instalirajte novu aplikaciju, pa novu konzolu; starija aplikacija bi
oznaku prikazala kao tekst.

Uključuje se u konzoli: Podešavanja → Obaveštenja → SMS telefon →
„Odgovaraj na SMS upite”. U aplikaciji treba dozvoliti i **primanje** SMS-a
(isti postupak kao za slanje).

## 3. Ključ za potpis APK-a (jednom)

Svaka nova verzija mora biti potpisana istim ključem, inače Android odbija
ažuriranje (aplikacija mora da se obriše i ponovo podesi).

Na računaru sa Javom (JDK, alat `keytool`):

```
keytool -genkeypair -v -keystore netmon-sms.jks -alias netmon-sms -keyalg RSA -keysize 4096 -validity 10000
```

Zatim u GitHub repozitorijumu: Settings → Secrets and variables → Actions →
New repository secret:

| Tajna | Vrednost |
|---|---|
| `NETMON_KEYSTORE_B64` | sadržaj fajla u base64 (PowerShell: `[Convert]::ToBase64String([IO.File]::ReadAllBytes("netmon-sms.jks"))`) |
| `NETMON_KEYSTORE_PASSWORD` | lozinka keystore-a |
| `NETMON_KEY_ALIAS` | `netmon-sms` |
| `NETMON_KEY_PASSWORD` | lozinka ključa (obično ista) |

Fajl `netmon-sms.jks` i lozinke čuvajte na sigurnom (ne u repozitorijumu).
Bez tajni GitHub pravi samo probni APK (`…-proba.apk`) sa privremenim potpisom.

## Razvoj

- `core/` – logika bez Android-a (IMAP, pravila, ograničenje); testovi:
  `gradle -p android :core:test`. Test sa pravim IMAP serverom se pokreće uz
  `NETMON_TEST_IMAP=host:port:nalog:lozinka`.
- `app/` – Android deo (servis, slanje SMS-a, ekran), bez AndroidX biblioteka.
