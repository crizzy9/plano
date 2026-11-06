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
