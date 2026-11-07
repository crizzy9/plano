package app.plano.live

private const val NANOS_PER_MILLISECOND = 1_000_000L
private const val NANOS_PER_SECOND = 1_000_000_000L
private const val MAX_ALERT_EXPIRY_LEAD_MILLIS = 28 * 24 * 60 * 60 * 1000L + PUSH_CLOCK_SKEW_MILLIS
internal const val ADVISORY_ORDER_TTL_MILLIS = 2 * 60 * 60 * 1000L + PUSH_CLOCK_SKEW_MILLIS
internal const val MAX_ADVISORY_ORDER_ENTRIES = 256
internal val advisoryFamilyPattern = Regex("[0-9a-f]{64}")
private val advisoryRevisionPattern = Regex("[1-9][0-9]{0,18}:[1-9][0-9]{0,18}")
private val alertKindPattern = Regex("[a-z][a-z0-9_]{0,31}")
private val departureAlertKinds = setOf(
  "boarding", "check_in", "departure_gate", "etd", "delay_prediction", "inbound_takeoff", "inbound_landing",
)
private val preTakeoffAlertKinds = departureAlertKinds + setOf("faa_edct", "eurocontrol_ctot")
private val arrivalAlertKinds = setOf("arrival_gate", "eta", "baggage")
private val terminalAlertKinds = setOf("cancelled", "diverted")
private val movementPhases = setOf("taxi_out", "airborne", "landed", "arrived")
internal val flightStateAlertKinds = preTakeoffAlertKinds + arrivalAlertKinds + terminalAlertKinds + "tail"

internal data class FlightAlertMetadata(
  val expiresAt: Long?,
  val kind: String? = null,
  val observedNs: Long? = null,
  val phase: String? = null,
  val advisoryRevision: AdvisoryAlertRevision? = null,
)

internal data class FlightAlertDecision(
  val notificationTag: String? = null,
  val cancelTags: List<String> = emptyList(),
)

internal data class AdvisoryAlertRevision(val observedNs: Long, val outboxId: Long) {
  fun encode() = "$observedNs:$outboxId"

  fun isAfter(previous: AdvisoryAlertRevision) =
    observedNs > previous.observedNs || observedNs == previous.observedNs && outboxId > previous.outboxId

  companion object {
    fun parse(encoded: String): AdvisoryAlertRevision? {
      if (!advisoryRevisionPattern.matches(encoded)) return null
      val observed = encoded.substringBefore(':').toLongOrNull() ?: return null
      val id = encoded.substringAfter(':').toLongOrNull() ?: return null
      return AdvisoryAlertRevision(observed, id)
    }
  }
}

internal data class AdvisoryAlertOrder(val received: Long, val revision: AdvisoryAlertRevision)

internal fun updateAdvisoryAlertOrder(
  order: Map<String, AdvisoryAlertOrder>, eventId: String?, revision: AdvisoryAlertRevision?, now: Long,
): Map<String, AdvisoryAlertOrder>? {
  val key = eventId?.takeIf(advisoryFamilyPattern::matches)?.let(::hashPushIdentity)
    ?: return order.takeIf { revision == null }
  val cutoff = now - ADVISORY_ORDER_TTL_MILLIS
  val previous = order[key]?.takeIf { it.received > cutoff }
  if (revision == null) return order.takeIf { previous == null }
  if (previous != null && !revision.isAfter(previous.revision)) return null
  val recent = order.filterValues { it.received > cutoff }
  // A still-live entry prevents a delayed revision from replacing the current
  // alert. Preserve those entries if a burst reaches the bound.
  if (previous == null && recent.size >= MAX_ADVISORY_ORDER_ENTRIES) return null
  return recent + (key to AdvisoryAlertOrder(now, revision))
}

internal fun parseFlightAlertMetadata(data: Map<String, String>, sentAt: Long, now: Long): FlightAlertMetadata? {
  val expiresAt = data["alertExpiresMs"]?.let { value ->
    value.toLongOrNull()?.takeIf { it > now && it - now in 1..MAX_ALERT_EXPIRY_LEAD_MILLIS } ?: return null
  }
  val kind = data["alertKind"]
  val observed = data["alertObservedNs"]
  val phase = data["alertPhase"]
  data["advisoryRevision"]?.let { encoded ->
    val revision = AdvisoryAlertRevision.parse(encoded) ?: return null
    val eventId = data["eventId"] ?: return null
    val sourceTime = revision.observedNs / NANOS_PER_MILLISECOND
    if (kind != null || observed != null || phase != null || data["connectionInboundId"] != null ||
      !advisoryFamilyPattern.matches(eventId) || expiresAt == null || expiresAt <= sourceTime ||
      expiresAt - now > ADVISORY_ORDER_TTL_MILLIS || !validPushObservationMillis(sourceTime, sentAt, now)
    ) return null
    return FlightAlertMetadata(expiresAt, advisoryRevision = revision)
  }
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
    "inbound_landing" -> setOf("inbound_takeoff")
    "taxi_out" -> departureAlertKinds
    "airborne", "landed" -> preTakeoffAlertKinds
    "arrived" -> preTakeoffAlertKinds + "eta"
    "cancelled" -> flightStateAlertKinds - kind
    "diverted" -> preTakeoffAlertKinds + arrivalAlertKinds + "cancelled"
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
