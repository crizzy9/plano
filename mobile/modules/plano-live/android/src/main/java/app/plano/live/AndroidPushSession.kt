package app.plano.live

import android.app.NotificationManager
import android.content.Context
import android.util.AtomicFile
import org.json.JSONObject
import java.io.File
import java.util.UUID

private const val ORDER_TTL_MILLIS = 2 * 24 * 60 * 60 * 1000L
private const val MAX_ORDER_ENTRIES = 64

internal data class LiveProgressAction(val binding: String, val instance: String)

internal object AndroidPushSession {
  private data class Order(
    val observed: Long,
    val sent: Long,
    val received: Long,
    val stoppedAt: Long = 0,
    val instance: String? = null,
  )

  data class AcceptedProgress(val instance: String?, val stoppedKeys: Set<String>)
  private data class AlertOrder(val received: Long, val revisions: Map<String, Long>)
  private data class State(
    val origin: String = "",
    val generation: String = UUID.randomUUID().toString(),
    val enabled: Boolean = false,
    val suppressed: Boolean = false,
    val order: Map<String, Order> = emptyMap(),
    val alertOrder: Map<String, AlertOrder> = emptyMap(),
  ) {
    fun retired() = if (enabled) mapOf("origin" to origin, "binding" to generation) else null
    fun stoppedKeys() = order.filterValues { it.stoppedAt != 0L }.keys
  }

  private var loaded: State? = null

  @Synchronized
  fun selectOrigin(context: Context, origin: String): Map<String, String>? {
    val current = state(context)
    if (current.origin == origin) return null
    reset(context, State(origin = origin))
    return current.retired()
  }

  @Synchronized
  fun status(context: Context, origin: String): Map<String, Any>? {
    val current = state(context)
    if (current.origin != origin || origin.isEmpty()) return null
    return mapOf("generation" to current.generation, "enabled" to current.enabled, "suppressed" to current.suppressed)
  }

  @Synchronized
  fun enable(context: Context, origin: String, generation: String, explicit: Boolean): Boolean {
    val current = state(context)
    if (current.origin != origin || current.generation != generation || current.suppressed && !explicit) return false
    if (!current.enabled) write(context, current.copy(enabled = true, suppressed = false))
    return true
  }

  @Synchronized
  fun disable(context: Context, origin: String, expected: String?, suppress: Boolean): Map<String, String>? {
    val current = state(context)
    if (current.origin != origin || expected != null && current.generation != expected) return null
    reset(context, State(origin = origin, suppressed = suppress))
    return current.retired()
  }

  @Synchronized
  fun clear(context: Context) {
    reset(context, State(origin = state(context).origin, suppressed = true))
  }

  @Synchronized
  fun refreshWidget(context: Context) {
    val current = state(context)
    PlanoFlightWidget.refresh(context, current.generation.takeIf { current.enabled }, current.stoppedKeys())
  }

  @Synchronized
  fun resolveWidgetFlight(context: Context, origin: String, key: String): String? {
    val current = state(context)
    if (!current.enabled || origin.isEmpty() || current.origin != origin) return null
    return PlanoFlightWidget.resolveTap(context, current.generation, key, current.stoppedKeys())
  }

  @Synchronized
  fun setWidgetDetailsEnabled(context: Context, enabled: Boolean) {
    val current = state(context)
    PlanoFlightWidget.setDetailsEnabled(context, enabled, current.generation.takeIf { current.enabled }, current.stoppedKeys())
  }

  @Synchronized
  fun stop(context: Context, flightId: String, expected: LiveProgressAction? = null) {
    val current = state(context)
    val key = hashPushIdentity(flightId)
    val previous = current.order[key]
    if (expected != null && (!current.enabled || current.generation != expected.binding ||
        previous == null || previous.instance != expected.instance || previous.stoppedAt != 0L)) return
    try {
      if (current.enabled) {
        val now = System.currentTimeMillis()
        val stopped = previous?.copy(received = now, stoppedAt = now) ?: Order(0, 0, now, now)
        val next = current.withOrder(key, stopped)
        try {
          write(context, next, verify = true)
        } catch (cause: Exception) {
          loaded = next
          throw cause
        }
      }
    } finally {
      context.getSystemService(NotificationManager::class.java)?.cancel(liveNotificationTag(flightId), 0)
      if (current.enabled) PlanoFlightWidget.stop(context, flightId, current.generation, state(context).stoppedKeys())
    }
  }

  @Synchronized
  fun expireProgress(context: Context, flightId: String, binding: String, observedAt: Long) {
    if (!validNotificationFlightId(flightId) || epochMillis(observedAt) == 0L) return
    val current = state(context)
    if (!current.enabled || current.generation != binding) return
    val order = current.order[hashPushIdentity(flightId)]
    if (order != null && (order.observed != observedAt || order.stoppedAt != 0L)) return
    context.getSystemService(NotificationManager::class.java)?.cancel(liveNotificationTag(flightId), 0)
  }

  // Hold the same lock as disable/switch through rendering. A message is either
  // rejected after revocation, or its already-rendered surfaces get cleared.
  @Synchronized
  fun accept(context: Context, binding: String?, sentAt: Long, render: () -> Unit) {
    val current = state(context)
    val active = current.generation.takeIf { current.enabled }
    if (acceptsPushBinding(active, binding, sentAt, System.currentTimeMillis())) render()
  }

  @Synchronized
  fun advanceAlert(context: Context, flightId: String?, metadata: FlightAlertMetadata): FlightAlertDecision? {
    val current = state(context)
    val now = System.currentTimeMillis()
    if (!current.enabled || metadata.expiresAt?.let { it <= now } == true) return null
    val kind = metadata.kind ?: return FlightAlertDecision()
    val observedNs = metadata.observedNs ?: return null
    val id = flightId?.takeIf(::validNotificationFlightId) ?: return null
    val key = hashPushIdentity(id)
    val recent = current.alertOrder.filterValues { it.received >= now - ORDER_TTL_MILLIS }
    val previous = recent[key]?.revisions.orEmpty()
    val update = updateFlightAlertOrder(previous, kind, observedNs, metadata.phase) ?: return null
    if (update.revisions != previous) {
      val ordered = recent.toMutableMap().apply { put(key, AlertOrder(now, update.revisions)) }
      val bounded = ordered.entries.sortedByDescending { it.value.received }.take(MAX_ORDER_ENTRIES)
        .associate { it.key to it.value }
      if (runCatching { write(context, current.copy(alertOrder = bounded)) }.isFailure) return null
    }
    return FlightAlertDecision(
      notificationTag = if (kind in flightStateAlertKinds) flightStateAlertTag(id, kind) else null,
      cancelTags = update.supersededKinds.map { flightStateAlertTag(id, it) },
    )
  }

  @Synchronized
  fun advance(context: Context, flightId: String, observedAt: Long, sentAt: Long, started: Boolean, ended: Boolean): AcceptedProgress? {
    val now = System.currentTimeMillis()
    if (!validPushObservation(observedAt, sentAt, now)) return null
    val current = state(context)
    val key = hashPushIdentity(flightId)
    val old = current.order[key]?.takeIf { it.received >= now - ORDER_TTL_MILLIS }
    if (!acceptsLiveAfterStop(old?.stoppedAt ?: 0, started, observedAt, sentAt)) return null
    if (old != null && (observedAt < old.observed || observedAt == old.observed && sentAt <= old.sent)) return null
    val instance = if (ended) null else old?.instance?.takeUnless { started } ?: UUID.randomUUID().toString()
    val next = current.withOrder(key, Order(observedAt, sentAt, now, instance = instance))
    write(context, next)
    return AcceptedProgress(instance, next.stoppedKeys())
  }

  private fun State.withOrder(key: String, entry: Order): State {
    val order = order.filterValues { it.received >= entry.received - ORDER_TTL_MILLIS }.toMutableMap()
    order[key] = entry
    val bounded = order.entries.sortedByDescending { it.value.received }.take(MAX_ORDER_ENTRIES)
      .associate { (id, entry) -> id to entry }
    return copy(order = bounded)
  }

  private fun reset(context: Context, next: State) {
    try {
      write(context, next)
    } catch (cause: Exception) {
      loaded = next
      file(context).delete()
      throw cause
    } finally {
      clearSurfaces(context)
    }
  }

  private fun clearSurfaces(context: Context) {
    val manager = context.getSystemService(NotificationManager::class.java)
    manager?.activeNotifications?.filter { it.tag?.startsWith("plano-") == true }?.forEach {
      manager.cancel(it.tag, it.id)
    }
    runCatching { PlanoFlightWidget.clear(context) }
  }

  private fun file(context: Context) = AtomicFile(File(context.noBackupFilesDir, "plano-push-session.json"))

  private fun state(context: Context): State {
    loaded?.let { return it }
    context.deleteSharedPreferences("plano-live-order")
    val restored = runCatching {
      val json = JSONObject(file(context).readFully().toString(Charsets.UTF_8))
      val origin = json.getString("origin")
      val generation = json.getString("generation")
      val enabled = json.optBoolean("enabled") && origin.isNotEmpty()
      val entries = json.optJSONObject("order") ?: JSONObject()
      val order = entries.keys().asSequence().take(MAX_ORDER_ENTRIES).associateWith { id ->
        val entry = entries.getJSONObject(id)
        Order(entry.getLong("observed"), entry.getLong("sent"), entry.getLong("received"), entry.optLong("stoppedAt"),
          (entry.opt("instance") as? String)?.takeIf(String::isNotEmpty))
      }
      val alertEntries = json.optJSONObject("alertOrder") ?: JSONObject()
      val alertOrder = alertEntries.keys().asSequence().take(MAX_ORDER_ENTRIES).mapNotNull { id ->
        val entry = alertEntries.optJSONObject(id) ?: return@mapNotNull null
        val received = entry.optLong("received").takeIf { it > 0L } ?: return@mapNotNull null
        val values = entry.optJSONObject("revisions") ?: return@mapNotNull null
        val revisions = flightStateAlertKinds.mapNotNull { kind ->
          values.optLong(kind).takeIf { it > 0L }?.let { kind to it }
        }.toMap()
        id to AlertOrder(received, revisions)
      }.toMap()
      check(UUID.fromString(generation).toString() == generation)
      State(origin, generation, enabled, json.optBoolean("suppressed"), order, alertOrder)
    }.getOrNull()
    val current = restored ?: State()
    loaded = current
    if (!current.enabled) clearSurfaces(context)
    return current
  }

  private fun write(context: Context, state: State, verify: Boolean = false) {
    val order = JSONObject()
    for ((key, value) in state.order) {
      val entry = JSONObject().put("observed", value.observed).put("sent", value.sent).put("received", value.received)
      if (value.stoppedAt != 0L) entry.put("stoppedAt", value.stoppedAt)
      value.instance?.let { entry.put("instance", it) }
      order.put(key, entry)
    }
    val alertOrder = JSONObject()
    for ((key, value) in state.alertOrder) {
      val revisions = JSONObject()
      for ((kind, revision) in value.revisions) revisions.put(kind, revision)
      alertOrder.put(key, JSONObject().put("received", value.received).put("revisions", revisions))
    }
    val json = JSONObject().put("origin", state.origin).put("generation", state.generation)
      .put("enabled", state.enabled).put("suppressed", state.suppressed).put("order", order)
      .put("alertOrder", alertOrder)
    val bytes = json.toString().toByteArray(Charsets.UTF_8)
    val file = file(context)
    val stream = file.startWrite()
    try {
      stream.write(bytes)
      file.finishWrite(stream)
    } catch (cause: Exception) {
      file.failWrite(stream)
      throw cause
    }
    if (verify) check(file.readFully().contentEquals(bytes)) { "Could not save live progress stop" }
    loaded = state
  }
}
