# Airport transfer routes

Plano can calculate connection times from airport routes supplied by the
server operator. Each airport graph describes the terminals, gates, and
checkpoints it covers. Plano selects the shortest configured path in the
direction of travel, adds the airport's buffer, and uses that estimate in
the connection risk score.

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
estimate with its boarding allowance. Directions use reported gates and
terminals.

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
