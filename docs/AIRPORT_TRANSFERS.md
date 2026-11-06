# Airport transfer routes

Plano can calculate connection times from airport routes supplied by the
server operator. Each airport graph describes the terminals, gates, and
checkpoints it covers. Plano selects the shortest configured path in the
direction of travel, adds the airport's buffer, and uses that estimate in
the connection risk score.

## Official maps and connection directions

Plano links to official terminal maps by default at SEA, DFW, ATL, SFO, EWR,
JFK, LAX, ORD, DEN, and LHR. The map opens on the airport's website. When
reported gates or terminals identify a supported path, Plano also shows
directions. Secure routes apply when the traveler can stay past security.

- SEA directions use walkways and the Blue, Green, and Yellow train lines.
  Follow airport staff and signs for shuttle buses during train interruptions.
  Travelers who need U.S. entry processing should follow International
  Arrivals and use the terminal map after screening. [SEA Underground](https://www.portseattle.org/blog/all-aboard-sea-underground) · [International connections](https://www.portseattle.org/page/international-connections) · [Construction impacts](https://www.portseattle.org/page/construction-impacts)
- DFW directions use Skylink between terminals A through E. Check platform
  signs for the train serving your departure terminal. [DFW transfer guide](https://www.dfwairport.com/explore/plan/connect/)
- ATL directions use Plane Train or the pedestrian walkway between concourses
  T and A through F. [ATL concourse connections](https://www.atl.com/maps-3/)
- SFO directions use the secure walkways between gate areas A through G.
  A and G occupy opposite ends of the walkway even though both serve the
  International Terminal. Follow posted detours near Terminal 3. Travelers
  who need U.S. entry processing should complete customs, any bag recheck,
  and screening before following onward gate signs. [SFO connections](https://www.flysfo.com/connecting-flights-faq) · [Official maps](https://www.flysfo.com/maps/static-maps) · [Terminal 3 construction](https://www.flysfo.com/about/construction/terminal-3-construction)
- EWR public transfers use free AirTrain or a posted replacement service.
  Terminal A has a separate covered walk or public shuttle to its AirTrain
  station. Terminal B has separate checkpoints for gates B40–B49, B51–B57,
  and B60–B68. United can confirm eligibility for its secure shuttle between
  A28 and C71. [EWR connections](https://www.newarkairport.com/flights/connections-guide) · [AirTrain Newark](https://www.newarkairport.com/transportation/airtrain) · [Terminal B](https://www.newarkairport.com/explore-ewr/terminals/terminal-b)
- JFK public transfers between Terminals 1, 4, 5, 7, and 8 use free AirTrain
  and screening at the departure terminal. The secure directions include the
  Terminal 4 concourse junction and the Terminal 8 underground connector when
  the reported gates identify those paths. Follow the current signs during
  construction. [JFK connections](https://www.jfkairport.com/flights/connections-guide) · [AirTrain JFK](https://www.jfkairport.com/transportation/airtrain) · [JFK service advisories](https://www.jfkairport.com/alerts-advisories)
- LAX directions use the secure terminal connectors and the passenger tunnel
  between Terminal B and its West Gates. Terminal 5 construction closes the
  through tunnel. For transfers between Terminals 4 and 6, confirm the secure
  bus with airport staff and follow posted signs. Trips involving unknown
  gates or remote gates in the 50s use the official map and general guidance.
  [LAX connector map](https://www.flylax.com/sites/lax/files/2025-12/Terminal%20Connections%20Pedestrian%20Path%2025-1222.pdf) · [LAWA construction notice](https://www.lawa.org/sites/lawa/files/2025-10/Construction%20Hot%20Topics_10.27.25.pdf) · [Terminal 5 status](https://www.flylax.com/terminals/terminal5)
- ORD has secure walkways between Terminals 1, 2, and 3. For Terminal 5
  transfers, Plano plans for the public Airport Transit System and screening.
  Passengers who can stay past security may qualify for the Terminal Transfer
  Bus between Terminal 5 and Terminal 1 or 3. Check current service and carry
  your same-day onward boarding pass. [O’Hare connections](https://www.flychicago.com/ohare/tofrom/connecting/Pages/default.aspx) · [Terminal transportation](https://www.flychicago.com/ohare/ServicesAmenities/services/Pages/tbt.aspx)
- DEN directions use the secure Train to the Gates between concourses A, B,
  and C. Travelers arriving at A who need U.S. entry processing should follow
  signs for customs in Jeppesen Terminal, recheck bags as directed, and clear
  screening before taking the train to the onward concourse. [DEN facilities](https://www.flydenver.com/about-den/facilities/#train) · [International transfers](https://www.flydenver.com/international-travelers/)
- LHR requires screening for every connection, including same-terminal
  transfers. For flights on the same ticket, follow the purple Flight
  Connections signs and use the free shuttle bus to change terminals. On
  separate tickets, follow Arrivals and allow time for any immigration, bag
  collection, check-in, and security. [Heathrow connections](https://www.heathrow.com/connecting-flights) · [Airport maps](https://www.heathrow.com/at-the-airport/airport-maps)

These directions use general estimates for risk scoring. Built-in routes that
plan for another checkpoint use the 65-minute terminal-transfer estimate plus
the usual boarding reserve. The official map remains available
when location information is incomplete or a connection is disrupted. Follow
airport staff for passport control, baggage recheck, and current screening
requirements.

Operator maps take precedence over the default map. An operator graph with
checkpoints also takes precedence over built-in directions, including when
its checkpoints have no path for the reported gates.

## Configure an airport

Set `PLANO_AIRPORT_TRANSFERS_FILE` to a version 1 JSON file and restart
Plano. The file is read at startup. With Compose, mount the file read-only,
set the variable to the path inside the container, and make the file
readable by the container's Plano user. The
[self-hosting guide](SELF_HOSTING.md) describes the read-only bind mount
pattern.

This example is synthetic. Its locations, steps, and times are illustrative.

```json
{
  "version": 1,
  "airports": [
    {
      "codes": ["XZZ", "KXZZ"],
      "mapUrl": "https://airport.example/terminal-maps",
      "bufferMinutes": 15,
      "nodes": [
        {"id": "a1", "label": "gate A1", "terminal": "A", "gate": "A1"},
        {"id": "b7", "label": "gate B7", "terminal": "B", "gate": "B7"},
        {"id": "terminal-a", "label": "terminal A connections", "terminal": "A"},
        {"id": "terminal-b", "label": "terminal B connections", "terminal": "B"},
        {"id": "security", "label": "transfer security"}
      ],
      "edges": [
        {"from": "a1", "to": "security", "minutes": 6, "instruction": "Follow signs to transfer security."},
        {"from": "security", "to": "b7", "minutes": 12, "instruction": "After screening, follow signs for gate B7."},
        {"from": "terminal-a", "to": "terminal-b", "minutes": 24}
      ]
    }
  ]
}
```

The configured A1-to-B7 estimate is 33 minutes, including the 15-minute
buffer. Edges are one-way. Define a separate edge for the reverse direction
where it is a valid transfer. If an edge omits its instruction, Plano uses
the destination checkpoint label. Supply times and a buffer that reflect
boarding deadlines, screening, transit waits, and the scope of the route
you mapped. Passenger-specific requirements and current restrictions can
change the actual transfer.

Each airport can list up to eight codes, including aliases such as its IATA
and ICAO identifiers. Use the same terminal and gate designators as the
flight feed. Case and leading or trailing spaces are ignored. A node with a
terminal alone represents a terminal checkpoint. A node with a gate also
requires a terminal. Nodes without a terminal or gate represent
intermediate checkpoints. Each edge takes 1 to 240 minutes and the optional
buffer takes 0 to 120 minutes. The optional `mapUrl` should link to the
airport's official HTTPS map. A map-only entry may omit nodes and edges.

The file can contain up to 256 airports and occupy up to 1 MiB. Each airport
can define up to 256 checkpoints and 2,048 directed edges. Unknown fields,
duplicate airport codes, duplicate locations, invalid map URLs, and invalid
edges fail configuration validation.

## How Plano handles missing routes

Plano uses reported gates when both can be identified. A gate without a
reported terminal is identifiable when its gate code is unique in the
configured airport. If a gate is unknown and its terminal has a checkpoint,
Plano can use that checkpoint. The result distinguishes gate, mixed, and
terminal routing precision.

Plano uses its general transfer estimate when the airport has no graph,
when the graph has no path between the available checkpoints, or when both
endpoints reduce to the same terminal checkpoint. An airport map link can
still be shown when a map URL is configured. Transfer routes depend on
operator data and reported gate or terminal assignments. Airline and
airport instructions remain the source for current operational guidance.

## How Plano estimates connection risk

When using a general transfer estimate, Plano allows an estimated 15 minutes
before the onward departure for boarding. Configured paths already include
the operator's buffer. Use the airline's published gate-close time for the
actual deadline. An upcoming connection is **Risky** when the available time
falls short of the transfer and boarding estimates or the departure estimate
has passed without an onward departure report. It is **Tight** when it meets
the estimates with less than 25 minutes to spare, and **Relaxed** otherwise.
If the estimated arrival time has passed without a recorded gate arrival,
the upcoming estimate starts from the current time.

Once the onward flight has departed, Plano can still estimate whether the
connection was feasible using the available flight times. It reports
**Missed** if the inbound gate arrival was unrecorded or the elapsed time fell
short of the estimates. A cancellation, inbound diversion, or an elapsed
connection window also results in **Missed**.

When comparable flights suggest a gate and that gate identifies a more
precise airport path, Plano can show its duration including the configured
buffer. The risk score uses the longer of that duration and the main
estimate with its boarding allowance. Gate history counts each verified
flight instance once and uses the reported terminal when available.
Directions use reported gates and terminals.

## Monitor a connection

In Connection Assistant, open a suggested pair or select your arriving
and onward tracked flights. Check the connection and choose **Monitor
connection**. Both flights must connect at the same airport, with a scheduled
transfer window of up to 24 hours and an upcoming onward departure. You can
save up to 32 active pairs per installation. Enable flight notifications on
a device to receive alerts.
Saved pairs are private to that installation.

Plano checks both legs as new observations arrive and as time runs short.
The current risk is the starting point. A change to a worse risk level
queues an alert. If the risk improves, that becomes the starting point for
any later warning. Tap a warning to open Connection Assistant when both
legs are still tracked and connect at the same airport. If the connection
is no longer available, the tap opens the onward flight if it is still
tracked. To stop warnings, choose **Stop monitoring** or untrack either leg.
Monitoring ends after the onward flight leaves, the connection is disrupted
by a cancellation or inbound diversion, or the last known onward departure
time has passed by two hours without a recorded departure.
