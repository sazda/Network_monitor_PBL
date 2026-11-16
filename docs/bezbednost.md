# Bezbednost i korisnici

## Bezbednost

- **Agenti izvršavaju samo četiri fiksne komande** (latency, speedtest,
  lan_server, lan_client). Ni neko ko preuzme konzolu ne može preko agenta da
  pokrene proizvoljan program na računaru. Agent sam ograničava trajanje
  testova (najviše 30 s) i broj konekcija (16).
- **Šta agenti sami šalju:** agent za računare – naziv računara, mrežne
  kartice i adrese, gateway i saobraćaj kartica (na 10 s); agent za servere –
  uptime, CPU, RAM, diskove i do 5 procesa sa najvećim zauzećem procesora
  (naziv, PID, CPU, RAM; **bez komandne linije**). Nikakvi korisnički podaci ni
  sadržaj fajlova.
- Saobraćaj je šifrovan TLS-om sa sertifikatom iz **internog CA (AD CS)**.
  Računari u domenu već veruju tom CA, pa agentu ne treba dodatno podešavanje.
  Do otpremanja tog sertifikata konzola koristi privremeni (self-signed), kome
  agenti namerno **ne veruju**.
- Agenti se autentifikuju **zajedničkim tajnim tokenom**. Na računarima se
  token čuva u registry ključu `HKLM\SOFTWARE\NetworkMonitor\Agent`, koji mogu
  da čitaju samo SYSTEM i administratori. Ograničenje: ko ima token može da
  poveže lažnog agenta i pokreće testove brzine prema konzoli; **ne može** da
  upravlja računarima korisnika.
- **Prijava na konzolu:** lozinke se čuvaju kao **bcrypt hash**; uloge
  **administrator** i **operater** (operater ne menja podešavanja, ne vidi
  token, nema Održavanje). Dok lozinka nije postavljena, interfejs je dostupan
  **samo sa samog servera**. Posle 10 neuspešnih prijava u 15 minuta IP adresa
  se privremeno blokira.
- **Lozinke u konfiguraciji:** SNMP podaci (community, v3 lozinke) i SMTP/IMAP
  lozinke su u `C:\ProgramData\NetworkMonitor\console.json` – **nešifrovane**,
  zaštićene pravima pristupa (samo SYSTEM i administratori), i ne vraćaju se
  kroz web interfejs. Za SNMP koristite nalog samo za čitanje (v3 sa
  šifrovanjem, view ograničen na čitanje).
- **Nalozi koji se ne čuvaju:** administratorski nalog za instalaciju agenata
  (WinRM) i domenski nalog za Održavanje switch-eva koriste se samo za tu radnju
  i ne upisuju se ni u fajl ni u log. Za Održavanje konzola prvo proverava
  nalog u domenu (Windows `LogonUser`), pa pogrešna lozinka ne pokušava prijavu
  na switch-eve i ne zaključava nalog. SSH ključ switch-a pamti se pri prvoj
  prijavi; promenjen ključ se odbija dok ga administrator ne obriše. U logu
  konzole beleži se ko je, kada i sa kog switch-a šta preuzeo.
- **Syslog** (UDP 5514) nema autentifikaciju: konzola prihvata samo poruke sa
  adresa uređaja upisanih u konzolu i iz njih koristi samo poruku o restartu,
  ali se adresa pošiljaoca UDP paketa može lažirati. Posledica bi bila pogrešan
  podatak „ko je pokrenuo restart” u obaveštenju, ne pristup uređajima.
- **SMS:** brojevi primalaca su samo u aplikaciji na telefonu (nikad se ne
  čitaju iz e-maila); aplikacija prihvata samo poruke sa adrese konzole i
  odgovara na upite samo brojevima sa svog spiska.
- LAN test otvara privremeni port samo dok test traje i prihvata samo
  konekcije sa jednokratnim tokenom; firewall pravila agenta (LAN test, ping)
  važe samo za **Domain** profil.

## Korisnici i uloge

Glavni administrator (nalog postavljen pri prvom pokretanju) u
**Podešavanja → Korisnici** dodaje druge naloge, sa jednom od dve uloge:

| Uloga | Može | Ne može |
|---|---|---|
| **Operater** | da vidi računare i rezultate, pokreće Latenciju, Gateway, Brzinu i LAN test, menja svoju lozinku | podešavanja, token za agente, sertifikat, korisnici, uklanjanje agenata |
| **Administrator** | sve | da obriše glavnog administratora ili sopstveni nalog |

Uz svaki rezultat se beleži ko je pokrenuo merenje (kolona *Pokrenuo*), a
dodavanje, izmena i brisanje naloga beleže se u `console.log`. Korisnici se
čuvaju u `console.json` (lozinke kao bcrypt hash).

Prijava je preko stranice za prijavu; dugme **Odjava** je gore desno. Sesija
ističe posle 4 sata neaktivnosti (najduže 12 sati), a prekida se i kada se
korisniku promeni lozinka ili se nalog obriše. Promena uloge važi odmah.
Skripte i REST API mogu i dalje da koriste HTTP Basic prijavu.
