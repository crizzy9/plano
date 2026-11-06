package app.plano.live

internal const val WIDGET_FRESH_MILLIS = 20 * 60 * 1000L
private const val CLOCK_SKEW_MILLIS = 5 * 60 * 1000L
private const val MAX_COUNTDOWN_MILLIS = 48 * 60 * 60 * 1000L
private const val MIN_PROGRESS_REFRESH_MILLIS = 60_000L

internal enum class WidgetFlightPhase(val priority: Int, val arrivalCountdown: Boolean = false) {
  SCHEDULED(0),
  BOARDING(1),
  TAXI_OUT(2, true),
  LANDED(3),
  AIRBORNE(4, true),
  DIVERTED(4, true);

  val departureDetails: Boolean
    get() = this == SCHEDULED || this == BOARDING

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

internal data class WidgetFlightDetails(val gate: String?, val terminal: String?, val seat: String?) {
  companion object {
    private val seatCode = Regex("[0-9]{1,3}[A-Z]{1,2}")

    fun fromFlight(phase: WidgetFlightPhase, data: Map<String, String>): WidgetFlightDetails? {
      // A diversion update can still report the originally scheduled gate.
      if (phase == WidgetFlightPhase.DIVERTED) return fromFields(null, null, data["seat"])
      val leg = if (phase.departureDetails) "departure" else "arrival"
      return fromFields(data["${leg}Gate"], data["${leg}Terminal"], data["seat"])
    }

    fun fromFields(gate: String?, terminal: String?, seat: String?): WidgetFlightDetails? {
      val details = WidgetFlightDetails(location(gate), location(terminal), seat?.takeIf(seatCode::matches))
      return details.takeUnless { it.gate == null && it.terminal == null && it.seat == null }
    }

    private fun location(value: String?): String? = value?.takeIf { raw ->
      raw.length in 1..18 && raw.none { char ->
        char.isISOControl() || when (Character.getType(char)) {
          Character.FORMAT.toInt(), Character.LINE_SEPARATOR.toInt(), Character.PARAGRAPH_SEPARATOR.toInt() -> true
          else -> false
        }
      }
    }?.trim()?.takeIf { it.isNotEmpty() }
  }
}

internal data class WidgetFlightProgress(
  val phase: WidgetFlightPhase,
  val observedAt: Long,
  val departureAt: Long,
  val arrivalAt: Long,
  val details: WidgetFlightDetails? = null,
  val flightId: String? = null,
) {
  val expiresAt: Long
    get() = observedAt + WIDGET_FRESH_MILLIS

  fun isFresh(now: Long) = observedAt <= now + CLOCK_SKEW_MILLIS && expiresAt > now

  private val hasValidFlightInterval: Boolean
    get() = departureAt > 0 && arrivalAt > departureAt && arrivalAt - departureAt <= MAX_COUNTDOWN_MILLIS

  fun percent(now: Long): Int? = when {
    phase == WidgetFlightPhase.LANDED -> 100
    !phase.arrivalCountdown -> 0
    !hasValidFlightInterval -> null
    else -> ((now - departureAt).toDouble() / (arrivalAt - departureAt) * 100).toInt().coerceIn(0, 99)
  }

  fun countdownTarget(now: Long): Long? {
    if (phase == WidgetFlightPhase.LANDED) return null
    if (phase.arrivalCountdown && departureAt > 0 && !hasValidFlightInterval) return null
    val target = if (phase.arrivalCountdown) arrivalAt else departureAt
    return target.takeIf { it > 0 && it > now && it - now <= MAX_COUNTDOWN_MILLIS }
  }

  fun displayDeadline(now: Long): Long = minOf(expiresAt, countdownTarget(now) ?: expiresAt)

  fun nextRefreshAt(now: Long): Long {
    val deadline = displayDeadline(now)
    val percent = percent(now)
    if (!phase.arrivalCountdown || percent == null || percent >= 99) return deadline
    val duration = arrivalAt - departureAt
    val next = percent + 1
    // Calculate the next whole-percent step without multiplying the epoch-sized duration.
    val step = duration / 100 * next + (duration % 100 * next + 99) / 100
    return minOf(deadline, maxOf(now + MIN_PROGRESS_REFRESH_MILLIS, departureAt + step))
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
