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
  check(selectWidgetFlight(listOf(taxiing), taxiing.expiresAt) == null)

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

  val landed = airborne.copy(phase = WidgetFlightPhase.LANDED)
  check(landed.countdownTarget(now) == null)
  check(landed.displayDeadline(now) == landed.expiresAt)
  check(landed.percent(now) == 100)

  val stoppedAt = now + 3_000L
  val newer = stoppedAt + 1_000L
  check(!acceptsLiveAfterStop(stoppedAt, false, newer / 1000, newer))
  check(!acceptsLiveAfterStop(stoppedAt, true, now / 1000, newer))
  check(!acceptsLiveAfterStop(stoppedAt, true, stoppedAt / 1000, stoppedAt))
  check(acceptsLiveAfterStop(stoppedAt, true, newer / 1000, newer))
  check(acceptsLiveAfterStop(stoppedAt, true, newer / 1000, 0))
  println("Flight progress deadline and stop fixtures passed")
}
