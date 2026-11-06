package app.plano.live

private const val NANOS_PER_MILLISECOND = 1_000_000L
private const val NANOS_PER_SECOND = 1_000_000_000L
private const val MAX_ALERT_EXPIRY_LEAD_MILLIS = 28 * 24 * 60 * 60 * 1000L + PUSH_CLOCK_SKEW_MILLIS
private val alertKindPattern = Regex("[a-z][a-z0-9_]{0,31}")
private val departureAlertKinds = setOf("boarding", "check_in", "departure_gate", "etd", "delay_prediction")
private val arrivalAlertKinds = setOf("arrival_gate", "eta", "baggage")
private val terminalAlertKinds = setOf("cancelled", "diverted")
private val movementPhases = setOf("taxi_out", "airborne", "landed", "arrived")
internal val flightStateAlertKinds = departureAlertKinds + arrivalAlertKinds + terminalAlertKinds + "tail"

internal data class FlightAlertMetadata(
  val expiresAt: Long?,
  val kind: String? = null,
  val observedNs: Long? = null,
  val phase: String? = null,
)

internal data class FlightAlertDecision(
  val notificationTag: String? = null,
  val cancelTags: List<String> = emptyList(),
)

internal fun parseFlightAlertMetadata(data: Map<String, String>, sentAt: Long, now: Long): FlightAlertMetadata? {
  val expiresAt = data["alertExpiresMs"]?.let { value ->
    value.toLongOrNull()?.takeIf { it > now && it - now in 1..MAX_ALERT_EXPIRY_LEAD_MILLIS } ?: return null
  }
  val kind = data["alertKind"]
  val observed = data["alertObservedNs"]
  val phase = data["alertPhase"]
  // A legacy sender may omit all metadata. New generic alerts carry only expiry.
  if (kind == null && observed == null && phase == null) return FlightAlertMetadata(expiresAt)
  val observedNs = observed?.toLongOrNull() ?: return null
  if (kind == null || !alertKindPattern.matches(kind) || expiresAt == null ||
    phase != null && !alertKindPattern.matches(phase) ||
    expiresAt <= observedNs / NANOS_PER_MILLISECOND ||
    !validPushObservation(observedNs / NANOS_PER_SECOND, sentAt, now)
  ) return null
  return FlightAlertMetadata(expiresAt, kind, observedNs, phase)
}

internal data class FlightAlertOrderUpdate(
  val revisions: Map<String, Long>,
  val supersededKinds: List<String>,
)

internal fun updateFlightAlertOrder(
  revisions: Map<String, Long>, kind: String, observedNs: Long, phase: String? = null,
): FlightAlertOrderUpdate? {
  val stateAlert = kind in flightStateAlertKinds
  if (stateAlert && observedNs <= (revisions[kind] ?: 0L)) return null

  val invalidated = when (kind) {
    "taxi_out", "airborne", "landed" -> departureAlertKinds
    "arrived" -> departureAlertKinds + "eta"
    "cancelled" -> flightStateAlertKinds - kind
    "diverted" -> departureAlertKinds + arrivalAlertKinds + "cancelled"
    else -> emptySet()
  }
  // OOOI facts can accompany a diversion or cancellation. Clear terminal
  // alerts only on an explicit recovery, and keep any same-observation alert.
  val recovered = if (kind == "boarding" || kind in movementPhases && phase != null && phase in movementPhases) {
    terminalAlertKinds
  } else emptySet()
  if (!stateAlert && invalidated.isEmpty()) return FlightAlertOrderUpdate(revisions, emptyList())

  val next = revisions.toMutableMap()
  val superseded = mutableListOf<String>()
  if (stateAlert) next[kind] = observedNs
  for (priorKind in invalidated + recovered) {
    val cutoff = if (priorKind in recovered) observedNs - 1 else observedNs
    val prior = revisions[priorKind]
    if (prior != null && prior > cutoff) continue
    next[priorKind] = cutoff
    if (prior != null) superseded.add(priorKind)
  }
  return FlightAlertOrderUpdate(next, superseded)
}

internal fun flightStateAlertTag(flightId: String, kind: String) =
  "plano-alert-${hashPushIdentity("$flightId\u0000$kind")}"
