package app.plano.live

// Compile with WidgetFlightProgress.kt and PushWire.kt using a stock Kotlin compiler.
fun main() {
  val now = 1_760_000_000_000L
  val departure = now + 10_000L
  val scheduled = WidgetFlightProgress(WidgetFlightPhase.SCHEDULED, now, departure, now + 3_600_000L)
  check(scheduled.countdownTarget(now) == departure)
  check(scheduled.displayDeadline(now) == departure)
  check(scheduled.nextRefreshAt(now) == departure)
  check(scheduled.countdownTarget(departure) == null)
  check(scheduled.displayDeadline(departure) == scheduled.expiresAt)

  val later = scheduled.copy(observedAt = departure, departureAt = departure + 900_000L)
  check(later.countdownTarget(departure) == later.departureAt)
  check(later.displayDeadline(departure) == later.departureAt)

  val taxiing = checkNotNull(WidgetFlightProgress.fromEpochs("taxi_out", now / 1000,
    (now - 300_000L) / 1000, (now + 900_000L) / 1000))
  check(taxiing.phase == WidgetFlightPhase.TAXI_OUT)
  check(taxiing.countdownTarget(now) == taxiing.arrivalAt)
  check(taxiing.percent(now) == 25)
  check(taxiing.displayDeadline(now) == taxiing.arrivalAt)
  check(taxiing.nextRefreshAt(now) == now + 60_000L)
  check(taxiing.countdownTarget(taxiing.arrivalAt) == null)
  check(taxiing.percent(taxiing.arrivalAt) == 99)
  check(taxiing.copy(arrivalAt = now + 3_600_000L).displayDeadline(now) == taxiing.expiresAt)
  check(selectWidgetFlight(mapOf("taxiing" to taxiing), taxiing.expiresAt) == null)

  val noDeparture = taxiing.copy(departureAt = 0)
  check(noDeparture.countdownTarget(now) == taxiing.arrivalAt)
  check(noDeparture.percent(now) == null)
  for (invalid in listOf(
    taxiing.copy(arrivalAt = 0),
    taxiing.copy(arrivalAt = -1),
    taxiing.copy(departureAt = now + 600_000L, arrivalAt = now + 300_000L),
    taxiing.copy(departureAt = now - 48 * 60 * 60 * 1000L),
  )) {
    check(invalid.countdownTarget(now) == null)
    check(invalid.percent(now) == null)
    check(invalid.nextRefreshAt(now) == taxiing.expiresAt)
  }

  val airborne = WidgetFlightProgress(WidgetFlightPhase.AIRBORNE, now, now - 900_000L, now + 1_800_000L)
  check(airborne.countdownTarget(now) == airborne.arrivalAt)
  check(airborne.displayDeadline(now) == airborne.expiresAt)
  check(airborne.nextRefreshAt(now) <= airborne.expiresAt)
  check(!airborne.isFresh(airborne.displayDeadline(now)))

  val flights = mapOf("scheduled" to scheduled, "airborne" to airborne)
  check(selectWidgetFlight(flights, now) == airborne)
  check(selectWidgetFlight(flights, now, "scheduled") == scheduled)
  check(selectWidgetFlight(flights, now, "awaiting-first-update") == null)
  val withFreshAirborne = flights + ("airborne" to airborne.copy(observedAt = scheduled.expiresAt))
  check(selectWidgetFlight(withFreshAirborne, scheduled.expiresAt, "scheduled") == null)
  check(selectWidgetFlight(withFreshAirborne, scheduled.expiresAt)?.phase == WidgetFlightPhase.AIRBORNE)

  val landed = airborne.copy(phase = WidgetFlightPhase.LANDED)
  check(landed.countdownTarget(now) == null)
  check(landed.displayDeadline(now) == landed.expiresAt)
  check(landed.percent(now) == 100)

  val diverted = checkNotNull(WidgetFlightProgress.fromEpochs("diverted", (now + 1_000L) / 1000,
    (now - 900_000L) / 1000, (now + 1_800_000L) / 1000))
  check(diverted.phase == WidgetFlightPhase.DIVERTED)
  check(diverted.countdownTarget(now) == null)
  check(diverted.copy(departureAt = now + 10_000L).countdownTarget(now) == null)
  check(diverted.percent(now) == null)
  check(diverted.displayDeadline(now) == diverted.expiresAt)
  check(diverted.nextRefreshAt(now) == diverted.expiresAt)
  val withDiversion = flights + ("diverted" to diverted)
  check(selectWidgetFlight(withDiversion, now) == airborne)
  check(selectWidgetFlight(withDiversion, now, "diverted") == diverted)

  val stoppedAt = now + 3_000L
  val newer = stoppedAt + 1_000L
  check(!acceptsLiveAfterStop(stoppedAt, false, LiveObservation(newer / 1000), newer))
  check(!acceptsLiveAfterStop(stoppedAt, true, LiveObservation(now / 1000), newer))
  check(!acceptsLiveAfterStop(stoppedAt, true, LiveObservation(stoppedAt / 1000), stoppedAt))
  check(acceptsLiveAfterStop(stoppedAt, true, LiveObservation(newer / 1000), newer))
  check(acceptsLiveAfterStop(stoppedAt, true, LiveObservation(newer / 1000), 0))

  val seconds = now / 1000
  val legacyData = mapOf("observedEpoch" to seconds.toString())
  val legacy = checkNotNull(parseLiveObservation(legacyData))
  val early = LiveObservation(seconds, seconds * 1_000_000_000L + 100_000_000)
  val late = LiveObservation(seconds, seconds * 1_000_000_000L + 900_000_000)
  check(parseLiveObservation(legacyData + ("liveObservedNs" to late.nanos.toString())) == late)
  for (invalid in listOf("", "-1", "0", "9223372036854775808", ((seconds + 1) * 1_000_000_000L).toString())) {
    check(parseLiveObservation(legacyData + ("liveObservedNs" to invalid)) == null)
  }
  // The late terminal observation wins even when the early update was sent last.
  check(late.isNewerThan(early, now + 1_000, now + 2_000))
  check(!early.isNewerThan(late, now + 2_000, now + 1_000))
  check(late.isNewerThan(early, 0, now + 2_000))
  check(late.isNewerThan(late, now + 2_000, now + 1_000))
  check(!late.isNewerThan(late, now + 1_000, now + 1_000))
  check(!legacy.isNewerThan(late, now + 3_000, now + 1_000))
  check(late.isNewerThan(legacy, now + 1_000, now + 3_000))
  check(legacy.isNewerThan(legacy, now + 3_000, now + 1_000))
  check(LiveObservation(seconds + 1).isNewerThan(late, 0, now + 2_000))
  check(!acceptsLiveAfterStop(now + 500, true, early, now + 2_000))
  check(acceptsLiveAfterStop(now + 500, true, late, 0))

  val original = checkNotNull(nextLiveProgressNotification(null, started = true, ended = false) { "first" })
  check(!original.dismiss("stale").dismissed)
  val dismissed = original.dismiss("first")
  check(dismissed.dismissed)
  check(nextLiveProgressNotification(dismissed, started = false, ended = false) { error("ordinary update replaced the notice") } == dismissed)
  check(nextLiveProgressNotification(dismissed, started = false, ended = true) { error("end created a notice") } == null)
  val restarted = checkNotNull(nextLiveProgressNotification(dismissed, started = true, ended = false) { "second" })
  check(!restarted.dismiss("first").dismissed)
  println("Flight progress ordering, deadline, selection, stop, and notification dismissal fixtures passed")
}
