# Analisi VM MPQUIC ROMARS: detection DHCP su scollegamento e scambio cavi

Destinatario: team ROMARS
Autore: Giuseppe Chiapparo (Telespazio)
Data: 16 luglio 2026
Target analizzato: VM Proxmox ID 300, `satcom@10.10.11.100`, Debian 12, accesso via `ssh -p 3222 satcom@10.202.15.2`
Natura dell'analisi: sola lettura, nessuna modifica applicata alla VM

## Sintesi

I due sintomi che riportate hanno cause diverse e vanno affrontati con rimedi diversi.

Il primo, la mancata detection dell'attacca e stacca dei cavi, dipende dal fatto che le NIC VirtIO della VM non vedono mai cadere il carrier quando il cavo viene mosso sull'host Proxmox. Nessun componente della VM può accorgersene tramite gli eventi di link.

Il secondo, il canale LTE che sembra online ma non passa traffico, dipende dal lease DHCP del modem LTE, che dura 24 ore contro i 5 minuti del lease Starlink. Dopo uno scambio dei cavi la rotta LTE resta installata e apparentemente valida per ore.

Abbiamo già risolto il primo caso con un daemon chiamato `wan-watchdog`. La cosa utile da sapere subito: quel daemon è già presente sulla vostra VM come unit systemd abilitata, ma lo script che dovrebbe eseguire non c'è. Il servizio è in crash loop dal boot e non ha mai fatto nulla. Vedete la sezione "Il watchdog c'è già".

Il secondo caso richiede il watchdog più un intervento di configurazione sul modem LTE, perché una parte del problema sta sul server DHCP e non sul client.

## Cosa ho verificato

Ho raccolto le evidenze con comandi di sola lettura sulla VM: `ip`, `networkctl status`, `systemctl status`, lettura di `/run/systemd/netif/leases/` e dei contatori in `/sys/class/net/`. Non ho modificato configurazioni, non ho riavviato servizi, non ho toccato i modem.

Lo stack di rete della VM è `systemd-networkd`. `NetworkManager`, `networking` e `dhcpcd` sono inattivi.

Le interfacce rilevanti al momento dell'analisi:

| Interfaccia | Ruolo | Indirizzo | Gateway | Metrica |
|---|---|---|---|---|
| `enp7s7` | LTE | 192.168.1.100/24 | 192.168.1.1 | 105 |
| `enp7s8` | Starlink | 9.246.8.61/23 | 9.246.8.1 | 106 |

Nota a margine: i commenti nei vostri file `.network` dicono "WAN5 (Starlink #1)" per `enp7s7` e "WAN6 (Starlink #2)" per `enp7s8`. Sono etichette nostre ereditate dall'immagine di partenza e ora sono fuori allineamento rispetto al cablaggio reale, dove `enp7s7` porta l'LTE. Conviene correggerle per non confondere chi legge.

## Causa 1: il carrier VirtIO non cade mai

Quando spostate un cavo sull'host Proxmox, la VM non riceve alcun evento di link. La NIC della VM è un dispositivo VirtIO attestato su un bridge dell'host, e il tap resta up indipendentemente da cosa succede alla porta fisica. Il client DHCP non ha quindi nessun motivo per rifare un DISCOVER, e si tiene il lease vecchio, cioè quello della rete sbagliata.

L'evidenza sulla vostra VM è netta. Gli slot modem `enp7s3`, `enp7s4`, `enp7s5` e `enp7s6` non hanno nessun modem collegato, eppure riportano tutti `carrier=1` e `operstate=up`:

```
enp7s3: carrier=1 changes=2 operstate=up
enp7s4: carrier=1 changes=2 operstate=up
enp7s5: carrier=1 changes=2 operstate=up
enp7s6: carrier=1 changes=2 operstate=up
enp7s7: carrier=1 changes=2 operstate=up
enp7s8: carrier=1 changes=2 operstate=up
```

Il campo `carrier_changes` vale 2 su tutte le interfacce, e 2 corrisponde alle sole transizioni iniziali di boot. La VM era up da un'ora e mezza e non aveva registrato nemmeno una variazione di carrier. Il segnale è inutilizzabile su questa piattaforma: interfacce senza cavo si dichiarano attive esattamente come quelle collegate.

Da qui la conseguenza pratica: qualunque logica basata su link up e link down non funzionerà mai su questa VM. Serve un probe attivo verso la rete.

## Causa 2: lease LTE da 24 ore contro lease Starlink da 5 minuti

Questo spiega il sintomo dello scambio dei cavi e l'asimmetria fra i due canali. Ecco i due lease letti da `/run/systemd/netif/leases/`.

LTE, `enp7s7`:

```
ADDRESS=192.168.1.100
SERVER_ADDRESS=192.168.1.1
T1=43200        # primo tentativo di rinnovo: 12 ore
T2=75600        # rebind: 21 ore
LIFETIME=86400  # scadenza: 24 ore
```

Starlink, `enp7s8`:

```
ADDRESS=9.246.8.61
SERVER_ADDRESS=9.246.8.1
T1=150          # primo tentativo di rinnovo: 2 minuti e 30
T2=262          # rebind: 4 minuti e 22
LIFETIME=300    # scadenza: 5 minuti
```

Quando scambiate i cavi Starlink e LTE, i due lati si comportano in modo molto diverso.

Il lato Starlink si riprende quasi da solo. Il client tenta il rinnovo dopo 150 secondi, il rinnovo fallisce perché il server 9.246.8.1 non è più su quel link, parte il rebind in broadcast a 262 secondi e l'indirizzo viene comunque abbandonato dopo 300 secondi. Nel giro di cinque minuti la situazione si sistema.

Il lato LTE invece resta congelato. Il client prova il primo rinnovo solo dopo 43200 secondi, cioè dodici ore. Per tutto quel tempo l'indirizzo 192.168.1.100 resta configurato, la rotta di default con metrica 105 resta installata, `networkctl` dichiara l'interfaccia `routable (configured)` e ogni controllo di stato locale risponde che il canale è sano. Non lo è: i pacchetti vengono instradati verso un gateway che su quel cavo non esiste più. L'indirizzo viene rilasciato davvero solo alla scadenza dei 24 ore.

È esattamente il "canale che sembra online ma non lo è" che descrivete, e la finestra di buco nero può durare mezza giornata.

### Perché `SendRelease=yes` non vi salva

Nei file `.network` c'è `SendRelease=yes`, e in condizioni normali serve: `systemd-networkd` manda un DHCPRELEASE quando l'interfaccia viene abbassata in modo pulito. Sullo scambio di cavo però non aiuta, per due motivi che si sommano.

Il primo è che non esiste nessun evento di link, come visto nella causa 1, quindi networkd non ha nessuna occasione per iniziare un release. Il secondo è più sottile: anche forzando a mano una riconfigurazione, il DHCPRELEASE è un pacchetto unicast diretto al server del lease, cioè 192.168.1.1. Dopo lo scambio quel server non è più raggiungibile su quel cavo, quindi il pacchetto si perde e il modem non lo riceve mai.

La conseguenza è che il binding lato modem sopravvive comunque, e sopravvive per le sue 24 ore, qualunque cosa faccia il client. Questa parte del problema non è risolvibile dalla VM. Va affrontata sul modem, come descritto più avanti.

## Il watchdog c'è già, manca lo script

La vostra VM ha già `/etc/systemd/system/wan-watchdog.service`, datato 11 marzo, abilitato all'avvio. Il file è identico al nostro, `Documentation=https://github.com/peppechiapparo/mpquic` compreso. Anche i file in `/etc/systemd/network/` sono i nostri, byte per byte.

Quello che manca è lo script che l'unit deve lanciare. `/usr/local/bin/` è vuoto, e il servizio è così:

```
Active: activating (auto-restart) (Result: exit-code)
Process: 2869 ExecStart=/usr/local/bin/wan-watchdog.sh (code=exited, status=203/EXEC)
```

`203/EXEC` significa file non trovato. Con `Restart=always` e `RestartSec=10` il servizio ci riprova ogni dieci secondi, fallisce ogni volta, e non ha mai eseguito una sola riga dal boot.

Questo spiega perché avete gli stessi sintomi che avevamo noi: la nostra correzione è formalmente presente sulla vostra macchina, ma non è mai stata in funzione. Probabilmente la snapshot da cui è nata la VM 300 è stata presa mentre i file di configurazione erano già stati installati e lo script no, oppure `/usr/local/bin` è stato ripulito in un secondo momento.

Vale la pena aggiungere un dettaglio sulle date, perché ha un effetto pratico. La vostra unit è datata 11 marzo alle 08:37. La nostra correzione al bug del `pipefail` sullo script è del commit `b498c63`, 11 marzo alle 08:54, diciassette minuti dopo. Se recuperate lo script da una copia coeva alla vostra immagine vi portate dietro anche quel bug, che faceva uscire lo script quando `grep` non trovava corrispondenze. Prendete la versione corrente, non quella dello snapshot.

## Come funziona il nostro watchdog

L'idea è semplice, e nasce proprio dal fatto che sul carrier non si può contare. Invece di aspettare un evento che non arriverà mai, il daemon verifica attivamente che il gateway del lease sia ancora quello giusto.

Ogni 15 secondi, per ogni WAN che ha un lease DHCP attivo, il daemon estrae il gateway corrente da `networkctl status` e lo pinga vincolando il ping a quella interfaccia con `ping -I`. Il vincolo sull'interfaccia è la parte importante: senza, il pacchetto potrebbe uscire da un'altra WAN e il test direbbe che va tutto bene quando non è vero.

Se il gateway risponde, il contatore dei fallimenti si azzera. Se non risponde per quattro controlli consecutivi, cioè circa 60 secondi, il daemon considera il lease morto e reagisce: fa `ip addr flush` sull'interfaccia e poi `networkctl reconfigure`. Il flush serve a togliere subito la rotta che sta facendo da buco nero, il reconfigure fa ripartire il client DHCP che manda un DISCOVER nuovo e prende l'indirizzo corretto dal modem effettivamente collegato in quel momento.

Ci sono tre protezioni che nella pratica si sono rivelate necessarie.

C'è un cooldown di 120 secondi per interfaccia, che impedisce al daemon di rientrare in loop di reconfigure quando un modem è davvero giù e il gateway resta irraggiungibile a prescindere. Senza, il daemon martella l'interfaccia ogni minuto per ore.

C'è la gestione del lease perso: se un'interfaccia aveva un gateway e improvvisamente non ce l'ha più, il daemon forza comunque il reconfigure invece di limitarsi a constatare l'assenza.

E c'è il fatto che ogni WAN ha stato indipendente, con contatore di fallimenti, timestamp dell'ultimo reconfigure e ultimo gateway noto tenuti in array associativi separati. Le sei WAN vengono valutate una per una senza interferire fra loro.

Tutto finisce nel journal con `SyslogIdentifier=wan-watchdog`, quindi il comportamento è ispezionabile a posteriori:

```
wan-watchdog: enp7s7: gateway 192.168.1.1 UNREACHABLE (1/4)
wan-watchdog: enp7s7: gateway 192.168.1.1 UNREACHABLE (2/4)
...
wan-watchdog: enp7s7: *** RECONFIGURE *** reason: gateway 192.168.1.1 unreachable for 60s
wan-watchdog: enp7s7: new gateway=9.246.8.1, new addr=9.246.8.47
```

L'effetto sul vostro problema numero 2 è diretto: il recupero passa da un massimo di dodici ore a circa 60 o 70 secondi, e il tempo non dipende più dalla durata del lease.

## Installazione

Lo script canonico è `scripts/wan-watchdog.sh` nel nostro repo, ed è allegato a questo documento. L'unit ce l'avete già e non va toccata.

```bash
sudo cp wan-watchdog.sh /usr/local/bin/
sudo chmod +x /usr/local/bin/wan-watchdog.sh
sudo systemctl restart wan-watchdog.service
systemctl status wan-watchdog.service
journalctl -u wan-watchdog.service -f
```

Non serve `daemon-reload` né `enable`, perché l'unit è già installata e già abilitata. Basta lo script e un restart.

Se volete restringere il monitoraggio alle sole due WAN realmente cablate, l'unit ha già le righe pronte da decommentare:

```ini
Environment=WAN_INTERFACES=enp7s7 enp7s8
Environment=CHECK_INTERVAL=15
Environment=FAIL_THRESHOLD=4
Environment=COOLDOWN=120
```

Sui default: 15 secondi di intervallo e soglia 4 danno una detection in circa un minuto, che sul nostro banco è risultato un buon compromesso fra reattività e falsi positivi su un LTE con RTT ballerino. Se vi serve più aggressivo, scendete sulla soglia prima che sull'intervallo.

### Verifica sul campo

Il test che usiamo noi è questo. Scambiate i due cavi, poi guardate il journal in tempo reale. Entro una ventina di secondi dovreste vedere il contatore salire su entrambe le interfacce, e intorno al minuto i due reconfigure con i nuovi indirizzi. Confermate con `ip route` che le due rotte di default puntino ai gateway giusti e che le metriche 105 e 106 siano rimaste coerenti.

## Raccomandazione sul modem LTE

Il watchdog risolve il lato client, ma sul modem resta un residuo che vale la pena sistemare.

Come spiegato sopra, il DHCPRELEASE non arriva mai a destinazione dopo uno scambio, quindi il modem LTE si tiene il binding fra il MAC vecchio e 192.168.1.100 per tutte le sue 24 ore. In pratica significa che dopo lo scambio la nuova interfaccia riceve un indirizzo diverso dal pool, cosa in sé innocua, ma che su ripetuti cicli di scambio può portare a consumare il pool se il modem ne ha uno piccolo, come capita spesso su questa classe di apparati.

Il rimedio è abbassare il lease time del server DHCP del modem LTE. Starlink usa 300 secondi e si comporta bene proprio per questo. Portare l'LTE su un valore dello stesso ordine, fra 120 e 300 secondi, allinea i due canali e fa scadere il binding vecchio in pochi minuti invece che in un giorno.

Il modem risponde su 192.168.1.1 con una web UI a sessione Java, quindi il parametro dovrebbe essere raggiungibile dall'interfaccia di amministrazione, tipicamente sotto le impostazioni DHCP della LAN. Il MAC del gateway è `60:32:b1:5b:d8:0a`, per identificarlo con certezza dalla vostra parte.

Le due misure sono complementari e conviene applicarle entrambe. Il watchdog vi copre comunque, anche con un lease lungo, perché non dipende dai timer DHCP. Il lease corto riduce il problema all'origine e limita il lavoro del watchdog ai casi veri.

## Limiti noti

Preferisco dirveli, così non vi sorprendono in collaudo.

Il watchdog rileva "gateway irraggiungibile", non "modem senza connettività a monte". Se il modem LTE risponde a 192.168.1.1 ma la SIM non ha copertura, il gateway pinga e il daemon considera la WAN sana. Quel caso è coperto dalla path liveness dello strato MPQUIC, non da qui.

Se due modem diversi servono la stessa sottorete, per esempio due apparati che danno entrambi 192.168.1.0/24 con gateway 192.168.1.1, lo scambio dei cavi non viene rilevato, perché il ping al gateway continua a rispondere. È il modem sbagliato, ma il test non lo distingue. Nella vostra configurazione attuale non succede, dato che le due reti sono diverse, però tenetelo presente se aggiungete WAN.

Il daemon si appoggia all'output testuale di `networkctl status`. È una scelta pragmatica che ci è costata già una correzione, il commit `b498c63` citato sopra. Su un aggiornamento maggiore di systemd conviene ricontrollare che il parsing regga.

Infine, il ping al gateway genera traffico, poco ma continuo: due pacchetti ogni 15 secondi per WAN. Su LTE a consumo il conto annuo è trascurabile, ma se la vostra tariffa è particolarmente aggressiva alzate `CHECK_INTERVAL`.

## Riepilogo delle azioni proposte

Sul lato vostro, in ordine di priorità: copiare `wan-watchdog.sh` in `/usr/local/bin/`, renderlo eseguibile e riavviare il servizio già presente, usando la versione corrente dello script e non quella coeva alla vostra immagine. Poi abbassare il lease time del modem LTE da 86400 a un valore fra 120 e 300 secondi. Infine, se volete tenere pulita la documentazione, correggere i commenti in `14-wan5.network` e `15-wan6.network`, che chiamano Starlink un'interfaccia che oggi porta l'LTE.

Restiamo a disposizione per il test di collaudo dello scambio cavi, se volete farlo insieme.
