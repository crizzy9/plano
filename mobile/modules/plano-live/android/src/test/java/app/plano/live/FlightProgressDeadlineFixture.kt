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
