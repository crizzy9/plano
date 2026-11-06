package app.plano.live

import android.app.NotificationManager
import android.content.Context
import android.util.AtomicFile
import org.json.JSONObject
import java.io.File
import java.security.MessageDigest
import java.util.UUID

private const val PUSH_CLOCK_SKEW_MILLIS = 5 * 60 * 1000L
private const val SOURCE_CLOCK_SKEW_MILLIS = 30_000L
private const val ORDER_TTL_MILLIS = 2 * 24 * 60 * 60 * 1000L
private const val MAX_ORDER_ENTRIES = 64

internal fun acceptsPushBinding(active: String?, supplied: String?, sentAt: Long, now: Long) =
  active != null && supplied == active && sentAt in 0..now + PUSH_CLOCK_SKEW_MILLIS

internal fun validPushObservation(observedAt: Long, sentAt: Long, now: Long): Boolean {
  val observed = epochMillis(observedAt)
  // FCM reports zero when a sent time is unavailable. Use a tighter device-time
  // bound then, because no transport timestamp corroborates the observation.
  if (observed == 0L || sentAt !in 0..now + PUSH_CLOCK_SKEW_MILLIS) return false
  if (sentAt == 0L) return observed <= now + SOURCE_CLOCK_SKEW_MILLIS
  return observed <= now + PUSH_CLOCK_SKEW_MILLIS && observed - sentAt <= SOURCE_CLOCK_SKEW_MILLIS
}

internal fun hashPushIdentity(id: String) =
  MessageDigest.getInstance("SHA-256").digest(id.toByteArray(Charsets.UTF_8)).joinToString("") { "%02x".format(it) }

internal fun liveNotificationTag(flightId: String) = "plano-live-${hashPushIdentity(flightId)}"

internal object AndroidPushSession {
  private data class Order(val observed: Long, val sent: Long, val received: Long)
  private data class State(
    val origin: String = "",
    val generation: String = UUID.randomUUID().toString(),
    val enabled: Boolean = false,
    val suppressed: Boolean = false,
    val order: Map<String, Order> = emptyMap(),
  ) {
    fun retired() = if (enabled) mapOf("origin" to origin, "binding" to generation) else null
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
    PlanoFlightWidget.refresh(context, current.generation.takeIf { current.enabled })
  }

  @Synchronized
  fun resolveWidgetFlight(context: Context, origin: String, key: String): String? {
    val current = state(context)
    if (!current.enabled || origin.isEmpty() || current.origin != origin) return null
    return PlanoFlightWidget.resolveTap(context, current.generation, key)
  }

  @Synchronized
  fun setWidgetDetailsEnabled(context: Context, enabled: Boolean) {
    val current = state(context)
    PlanoFlightWidget.setDetailsEnabled(context, enabled, current.generation.takeIf { current.enabled })
  }

  @Synchronized
  fun stop(context: Context, flightId: String) {
    context.getSystemService(NotificationManager::class.java)?.cancel(liveNotificationTag(flightId), 0)
    val current = state(context)
    if (current.enabled) PlanoFlightWidget.stop(context, flightId, current.generation)
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
  fun advance(context: Context, flightId: String, observedAt: Long, sentAt: Long): Boolean {
    val now = System.currentTimeMillis()
    if (!validPushObservation(observedAt, sentAt, now)) return false
    val current = state(context)
    val key = hashPushIdentity(flightId)
    val old = current.order[key]?.takeIf { it.received >= now - ORDER_TTL_MILLIS }
    if (old != null && (observedAt < old.observed || observedAt == old.observed && sentAt <= old.sent)) return false
    val order = current.order.filterValues { it.received >= now - ORDER_TTL_MILLIS }.toMutableMap()
    order[key] = Order(observedAt, sentAt, now)
    val bounded = order.entries.sortedByDescending { it.value.received }.take(MAX_ORDER_ENTRIES)
      .associate { (id, entry) -> id to entry }
    write(context, current.copy(order = bounded))
    return true
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
        Order(entry.getLong("observed"), entry.getLong("sent"), entry.getLong("received"))
      }
      check(UUID.fromString(generation).toString() == generation)
      State(origin, generation, enabled, json.optBoolean("suppressed"), order)
    }.getOrNull()
    val current = restored ?: State()
    loaded = current
    if (!current.enabled) clearSurfaces(context)
    return current
  }

  private fun write(context: Context, state: State) {
    val order = JSONObject()
    for ((key, value) in state.order) {
      order.put(key, JSONObject().put("observed", value.observed).put("sent", value.sent).put("received", value.received))
    }
    val json = JSONObject().put("origin", state.origin).put("generation", state.generation)
      .put("enabled", state.enabled).put("suppressed", state.suppressed).put("order", order)
    val file = file(context)
    val stream = file.startWrite()
    try {
      stream.write(json.toString().toByteArray(Charsets.UTF_8))
      file.finishWrite(stream)
      loaded = state
    } catch (cause: Exception) {
      file.failWrite(stream)
      throw cause
    }
  }
}
