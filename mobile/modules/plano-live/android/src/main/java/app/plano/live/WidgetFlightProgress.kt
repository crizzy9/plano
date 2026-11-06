package app.plano.live

internal const val WIDGET_FRESH_MILLIS = 20 * 60 * 1000L
private const val CLOCK_SKEW_MILLIS = 5 * 60 * 1000L
private const val MAX_COUNTDOWN_MILLIS = 48 * 60 * 60 * 1000L

internal enum class WidgetFlightPhase(val priority: Int, val arrivalCountdown: Boolean = false) {
  SCHEDULED(0),
  BOARDING(1),
  TAXI_OUT(2),
  LANDED(3),
  AIRBORNE(4, true),
  DIVERTED(4, true);

  companion object {
    fun fromStatus(status: String?) = when (status) {
      "scheduled" -> SCHEDULED
      "boarding" -> BOARDING
      "taxi_out" -> TAXI_OUT
      "landed" -> LANDED
      "airborne" -> AIRBORNE
      "diverted" -> DIVERTED
      else -> null
    }
  }
}

internal data class WidgetFlightProgress(
  val phase: WidgetFlightPhase,
  val observedAt: Long,
  val departureAt: Long,
  val arrivalAt: Long,
) {
  val expiresAt: Long
    get() = observedAt + WIDGET_FRESH_MILLIS

  fun isFresh(now: Long) = observedAt <= now + CLOCK_SKEW_MILLIS && expiresAt > now

  fun percent(now: Long): Int? = when {
    phase == WidgetFlightPhase.LANDED -> 100
    !phase.arrivalCountdown -> 0
    departureAt <= 0 || arrivalAt <= departureAt -> null
    else -> ((now - departureAt).toDouble() / (arrivalAt - departureAt) * 100).toInt().coerceIn(0, 99)
  }

  fun countdownTarget(now: Long): Long? {
    if (phase == WidgetFlightPhase.LANDED) return null
    val target = if (phase.arrivalCountdown) arrivalAt else departureAt
    return target.takeIf { it > now && it - now <= MAX_COUNTDOWN_MILLIS }
  }

  companion object {
    fun fromEpochs(status: String?, observedEpoch: Long, departureEpoch: Long, arrivalEpoch: Long): WidgetFlightProgress? {
      val phase = WidgetFlightPhase.fromStatus(status) ?: return null
      val observedAt = epochMillis(observedEpoch)
      if (observedAt == 0L) return null
      return WidgetFlightProgress(phase, observedAt, epochMillis(departureEpoch), epochMillis(arrivalEpoch))
    }
  }
}

internal fun epochMillis(seconds: Long) =
  if (seconds in 1..(Long.MAX_VALUE - WIDGET_FRESH_MILLIS) / 1000) seconds * 1000 else 0L

internal fun selectWidgetFlight(states: Collection<WidgetFlightProgress>, now: Long) =
  states.asSequence().filter { it.isFresh(now) }.maxWithOrNull(
    compareBy<WidgetFlightProgress> { it.phase.priority }.thenBy { it.observedAt },
  )
