package app.plano.live

// From the module's android directory, run with a stock Kotlin compiler:
// src=src/main/java/app/plano/live
// kotlinc "$src/PushWire.kt" "$src/FlightAlertOrder.kt" "$src/WidgetFlightProgress.kt" \
//   src/test/java/app/plano/live/FlightAlertOrderFixture.kt -include-runtime -d /tmp/plano-alert-order.jar
// java -jar /tmp/plano-alert-order.jar
fun main() {
  checkAlertMetadata()
  checkAlertOrdering()
  checkTakeoffSlotOrdering()
  checkInboundMilestoneOrdering()
  checkAdvisoryAlertMetadata()
  checkAdvisoryAlertOrdering()
  checkAdvisoryOrderCapacity()
  println("Flight alert metadata and ordering fixtures passed")
}

private fun checkAlertMetadata() {
  val now = 1_760_000_000_000L
  val expires = now + 60_000
  val observed = now * 1_000_000 + 7
  val valid = mapOf(
    "alertExpiresMs" to expires.toString(),
    "alertKind" to "departure_gate",
    "alertObservedNs" to observed.toString(),
  )
  check(parseFlightAlertMetadata(emptyMap(), now, now) == FlightAlertMetadata(null))
  check(parseFlightAlertMetadata(valid - listOf("alertKind", "alertObservedNs"), now, now) == FlightAlertMetadata(expires))
  val expected = FlightAlertMetadata(expires, "departure_gate", observed)
  check(parseFlightAlertMetadata(valid, now, now) == expected)
  check(parseFlightAlertMetadata(valid, 0, now) == expected)
  check(parseFlightAlertMetadata(valid + ("alertPhase" to "boarding"), now, now) == expected.copy(phase = "boarding"))

  val malformed = listOf(
    valid - "alertKind",
    valid - "alertObservedNs",
    valid - "alertExpiresMs",
    valid + ("alertKind" to "Departure gate"),
    valid + ("alertPhase" to "Out of range"),
    valid + ("alertObservedNs" to "0"),
    valid + ("alertObservedNs" to (observed + 40_000_000_000).toString()),
    valid + ("alertExpiresMs" to now.toString()),
    valid + ("alertExpiresMs" to "not-a-timestamp"),
    valid + ("alertExpiresMs" to Long.MAX_VALUE.toString()),
    mapOf("alertExpiresMs" to "0"),
    mapOf("alertExpiresMs" to Long.MAX_VALUE.toString()),
    mapOf("alertExpiresMs" to expires.toString(), "alertPhase" to "landed"),
  )
  for (payload in malformed) check(parseFlightAlertMetadata(payload, now, now) == null)
}

private fun checkAlertOrdering() {
  val gate = requireNotNull(updateFlightAlertOrder(emptyMap(), "departure_gate", 20))
  check(updateFlightAlertOrder(gate.revisions, "departure_gate", 19) == null)
  check(updateFlightAlertOrder(gate.revisions, "departure_gate", 20) == null)
  check(updateFlightAlertOrder(gate.revisions, "departure_gate", 21)?.revisions?.get("departure_gate") == 21L)

  val upcoming = gate.revisions + mapOf("boarding" to 10L, "eta" to 14L, "tail" to 13L)
  val taxi = requireNotNull(updateFlightAlertOrder(upcoming, "taxi_out", 15))
  check(taxi.supersededKinds == listOf("boarding"))
  check(taxi.revisions["departure_gate"] == 20L)
  check(updateFlightAlertOrder(taxi.revisions, "boarding", 15) == null)
  check(updateFlightAlertOrder(taxi.revisions, "check_in", 14) == null)
  check(updateFlightAlertOrder(taxi.revisions, "taxi_out", 11) != null)

  val landed = requireNotNull(updateFlightAlertOrder(taxi.revisions, "landed", 25))
  check(landed.revisions["eta"] == 14L)
  check(landed.revisions["tail"] == 13L)
  val arrived = requireNotNull(updateFlightAlertOrder(landed.revisions, "arrived", 30))
  check("eta" in arrived.supersededKinds)
  check(updateFlightAlertOrder(arrived.revisions, "eta", 29) == null)

  val arrivalState = upcoming + mapOf("arrival_gate" to 18L, "baggage" to 17L)
  val cancelled = requireNotNull(updateFlightAlertOrder(arrivalState, "cancelled", 24))
  check(cancelled.supersededKinds.containsAll(listOf("departure_gate", "eta", "baggage", "arrival_gate", "tail")))
  check(updateFlightAlertOrder(cancelled.revisions, "baggage", 24) == null)
  val diverted = requireNotNull(updateFlightAlertOrder(arrivalState, "diverted", 24))
  check(diverted.supersededKinds.containsAll(listOf("departure_gate", "eta", "baggage", "arrival_gate")))
  check("tail" !in diverted.supersededKinds && diverted.revisions["tail"] == 13L)

  val boarding = requireNotNull(updateFlightAlertOrder(cancelled.revisions, "boarding", 26))
  check("cancelled" in boarding.supersededKinds)
  check(updateFlightAlertOrder(boarding.revisions, "cancelled", 25) == null)
  check(updateFlightAlertOrder(boarding.revisions, "cancelled", 26) != null)
  val divertedArrival = requireNotNull(updateFlightAlertOrder(diverted.revisions, "arrived", 28, "diverted"))
  check("diverted" !in divertedArrival.supersededKinds && divertedArrival.revisions["diverted"] == 24L)
  val legacyArrival = requireNotNull(updateFlightAlertOrder(diverted.revisions, "arrived", 28))
  check("diverted" !in legacyArrival.supersededKinds && legacyArrival.revisions["diverted"] == 24L)
  val recoveredArrival = requireNotNull(updateFlightAlertOrder(diverted.revisions, "arrived", 28, "arrived"))
  check("diverted" in recoveredArrival.supersededKinds)
  check(updateFlightAlertOrder(recoveredArrival.revisions, "diverted", 27) == null)
  check(updateFlightAlertOrder(recoveredArrival.revisions, "diverted", 28) != null)
  val currentDiversion = requireNotNull(updateFlightAlertOrder(diverted.revisions, "arrived", 24, "arrived"))
  check("diverted" !in currentDiversion.supersededKinds && currentDiversion.revisions["diverted"] == 24L)

  val tag = flightStateAlertTag("flight-1", "departure_gate")
  check(tag == flightStateAlertTag("flight-1", "departure_gate"))
  check(tag != flightStateAlertTag("flight-1", "arrival_gate"))
  check(tag != flightStateAlertTag("flight-2", "departure_gate"))
}

private fun checkTakeoffSlotOrdering() {
  val edct = "faa_edct"
  val ctot = "eurocontrol_ctot"
  val faa = requireNotNull(updateFlightAlertOrder(emptyMap(), edct, 20))
  val both = requireNotNull(updateFlightAlertOrder(faa.revisions, ctot, 15))
  check(both.revisions == mapOf(edct to 20L, ctot to 15L))
  check(flightStateAlertTag("flight-1", edct) != flightStateAlertTag("flight-1", ctot))

  val taxi = requireNotNull(updateFlightAlertOrder(both.revisions, "taxi_out", 25))
  check(taxi.supersededKinds.isEmpty())
  for ((kind, observed) in both.revisions) {
    check(taxi.revisions[kind] == observed)
    check(updateFlightAlertOrder(taxi.revisions, kind, observed - 1) == null)
    check(updateFlightAlertOrder(taxi.revisions, kind, observed) == null)
    check(updateFlightAlertOrder(taxi.revisions, kind, 26)?.revisions?.get(kind) == 26L)
  }

  for (phase in listOf("airborne", "landed", "arrived", "cancelled", "diverted")) {
    val update = requireNotNull(updateFlightAlertOrder(taxi.revisions, phase, 30))
    check(update.supersededKinds.containsAll(listOf(edct, ctot)))
    for (kind in listOf(edct, ctot)) {
      check(update.revisions[kind] == 30L)
      check(updateFlightAlertOrder(update.revisions, kind, 29) == null)
      check(updateFlightAlertOrder(update.revisions, kind, 30) == null)
    }
  }
}

private fun checkInboundMilestoneOrdering() {
  val takeoff = "inbound_takeoff"
  val landing = "inbound_landing"
  val departed = requireNotNull(updateFlightAlertOrder(emptyMap(), takeoff, 20))
  val arrived = requireNotNull(updateFlightAlertOrder(departed.revisions, landing, 25))
  check(arrived.supersededKinds == listOf(takeoff))
  check(arrived.revisions == mapOf(takeoff to 25L, landing to 25L))
  check(flightStateAlertTag("flight-1", takeoff) != flightStateAlertTag("flight-1", landing))
  check(updateFlightAlertOrder(arrived.revisions, takeoff, 24) == null)
  check(updateFlightAlertOrder(arrived.revisions, takeoff, 25) == null)
  check(updateFlightAlertOrder(arrived.revisions, landing, 25) == null)

  val landingFirst = requireNotNull(updateFlightAlertOrder(emptyMap(), landing, 25))
  check(updateFlightAlertOrder(landingFirst.revisions, takeoff, 20) == null)
  val nextTakeoff = requireNotNull(updateFlightAlertOrder(arrived.revisions, takeoff, 27))
  val tail = requireNotNull(updateFlightAlertOrder(nextTakeoff.revisions, "tail", 28))
  check(tail.supersededKinds.isEmpty())
  check(tail.revisions[takeoff] == 27L && tail.revisions[landing] == 25L)

  for (phase in listOf("taxi_out", "airborne", "landed", "arrived", "cancelled", "diverted")) {
    val update = requireNotNull(updateFlightAlertOrder(tail.revisions, phase, 30))
    check(update.supersededKinds.containsAll(listOf(takeoff, landing)))
    for (kind in listOf(takeoff, landing)) {
      check(update.revisions[kind] == 30L)
      check(updateFlightAlertOrder(update.revisions, kind, 29) == null)
      check(updateFlightAlertOrder(update.revisions, kind, 30) == null)
    }
  }
}

private fun checkAdvisoryAlertMetadata() {
  val now = 1_760_000_000_000L
  val expires = now + 60_000
  val observedNs = (now - 10_000) * 1_000_000 + 7
  val revision = AdvisoryAlertRevision(observedNs, 42)
  val family = "a".repeat(64)
  val valid = mapOf(
    "alertExpiresMs" to expires.toString(),
    "eventId" to family,
    "advisoryRevision" to "$observedNs:42",
  )
  check(parseFlightAlertMetadata(valid, now, now) == FlightAlertMetadata(expires, advisoryRevision = revision))
  check(parseFlightAlertMetadata(valid - "advisoryRevision", now, now) == FlightAlertMetadata(expires))
  check(revision.encode() == valid["advisoryRevision"])
  check(AdvisoryAlertRevision.parse(revision.encode()) == revision)

  val malformed = listOf(
    valid - "eventId",
    valid - "alertExpiresMs",
    valid + ("eventId" to family.uppercase()),
    valid + ("eventId" to family.dropLast(1)),
    valid + ("advisoryRevision" to "$observedNs:0"),
    valid + ("advisoryRevision" to "0:42"),
    valid + ("advisoryRevision" to "$observedNs:42:7"),
    valid + ("advisoryRevision" to "$observedNs:9223372036854775808"),
    valid + ("advisoryRevision" to "${(now + 40_000) * 1_000_000}:42"),
    valid + ("alertExpiresMs" to now.toString()),
    valid + ("alertExpiresMs" to (now + ADVISORY_ORDER_TTL_MILLIS + 1).toString()),
    valid + mapOf("alertKind" to "departure_gate", "alertObservedNs" to observedNs.toString()),
    valid + ("connectionInboundId" to "flight-2"),
  )
  for (payload in malformed) {
    check(parseFlightAlertMetadata(payload, now, now) == null) { "Accepted invalid advisory payload $payload" }
  }
}

private fun checkAdvisoryAlertOrdering() {
  val now = 1_760_000_000_000L
  val family = "a".repeat(64)
  val other = "b".repeat(64)
  val revision = AdvisoryAlertRevision(now * 1_000_000, 42)
  check(updateAdvisoryAlertOrder(emptyMap(), family, null, now) == emptyMap<String, AdvisoryAlertOrder>())
  val first = requireNotNull(updateAdvisoryAlertOrder(emptyMap(), family, revision, now))
  val restored = first.mapValues { (_, entry) ->
    entry.copy(revision = requireNotNull(AdvisoryAlertRevision.parse(entry.revision.encode())))
  }
  check(updateAdvisoryAlertOrder(restored, family, revision, now + 1) == null)
  check(updateAdvisoryAlertOrder(restored, family, revision.copy(outboxId = 41), now + 1) == null)
  check(updateAdvisoryAlertOrder(restored, family, AdvisoryAlertRevision(revision.observedNs - 1, 99), now + 1) == null)
  check(updateAdvisoryAlertOrder(restored, family, null, now + 1) == null)
  check(updateAdvisoryAlertOrder(restored, other, null, now + 1) == restored)
  check(updateAdvisoryAlertOrder(restored, other, revision, now + 1)?.size == 2)

  val recurrence = revision.copy(outboxId = 43)
  val updated = requireNotNull(updateAdvisoryAlertOrder(restored, family, recurrence, now + 1))
  check(updated.values.single() == AdvisoryAlertOrder(now + 1, recurrence))
  val laterObservation = AdvisoryAlertRevision(revision.observedNs + 1, 1)
  check(updateAdvisoryAlertOrder(updated, family, laterObservation, now + 2)?.values?.single()?.revision == laterObservation)
}

private fun checkAdvisoryOrderCapacity() {
  val now = 1_760_000_000_000L
  val oldFamily = "0".repeat(64)
  val newFamily = "f".repeat(64)
  val revision = AdvisoryAlertRevision(now * 1_000_000, 42)
  val firstReceived = now - ADVISORY_ORDER_TTL_MILLIS + 1
  var order = requireNotNull(updateAdvisoryAlertOrder(emptyMap(), oldFamily, revision, firstReceived))
  for (index in 1 until MAX_ADVISORY_ORDER_ENTRIES) {
    val family = index.toString(16).padStart(64, '0')
    order = requireNotNull(updateAdvisoryAlertOrder(order, family, revision, now))
  }
  check(order.size == MAX_ADVISORY_ORDER_ENTRIES)
  check(updateAdvisoryAlertOrder(order, newFamily, revision, now) == null)
  check(updateAdvisoryAlertOrder(order, oldFamily, revision, now) == null)
  check(updateAdvisoryAlertOrder(order, oldFamily, revision.copy(outboxId = 43), now)?.size == MAX_ADVISORY_ORDER_ENTRIES)

  val afterExpiry = requireNotNull(updateAdvisoryAlertOrder(order, newFamily, revision, now + 2))
  check(afterExpiry.size == MAX_ADVISORY_ORDER_ENTRIES)
  check(updateAdvisoryAlertOrder(afterExpiry, oldFamily, null, now + 2) == afterExpiry)
}
