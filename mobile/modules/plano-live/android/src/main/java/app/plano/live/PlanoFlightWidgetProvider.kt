package app.plano.live

import android.app.AlarmManager
import android.app.PendingIntent
import android.appwidget.AppWidgetManager
import android.appwidget.AppWidgetProvider
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.os.SystemClock
import android.util.AtomicFile
import android.view.View
import android.widget.RemoteViews
import org.json.JSONArray
import org.json.JSONObject
import java.io.File

class PlanoFlightWidgetProvider : AppWidgetProvider() {
  override fun onUpdate(context: Context, manager: AppWidgetManager, ids: IntArray) {
    AndroidPushSession.refreshWidget(context)
  }

  override fun onReceive(context: Context, intent: Intent) {
    if (intent.action == PlanoFlightWidget.REFRESH_ACTION) AndroidPushSession.refreshWidget(context)
    else super.onReceive(context, intent)
  }

  override fun onDisabled(context: Context) {
    PlanoFlightWidget.cancelRefresh(context)
  }
}

internal object PlanoFlightWidget {
  const val REFRESH_ACTION = "app.plano.live.REFRESH_FLIGHT_WIDGET"
  private const val MAX_CANDIDATES = 8
  private const val MAX_STOPPED = 16
  private const val REQUEST_CODE = 731
  private var detailsBlocked = false

  private data class Cache(
    val detailsEnabled: Boolean = false,
    val flights: MutableMap<String, WidgetFlightProgress> = mutableMapOf(),
    val stopped: MutableMap<String, Long> = mutableMapOf(),
  )

  // This is called only after the notification service accepts the FCM event's
  // ordering. Keep only phase-appropriate details after a device opt-in.
  @Synchronized
  fun onFlight(
    context: Context,
    flightId: String,
    binding: String,
    started: Boolean,
    ended: Boolean,
    progress: WidgetFlightProgress?,
    sentAt: Long,
    data: Map<String, String>,
  ) {
    val now = System.currentTimeMillis()
    val cache = read(context, now, binding)
    val key = hashPushIdentity(flightId)

    if (ended) {
      cache.flights.remove(key)
    } else {
      val stoppedAt = cache.stopped[key]
      if (stoppedAt != null && (!started || sentAt <= stoppedAt)) return
      if (progress == null || !progress.isFresh(now)) return
      cache.stopped.remove(key)
      val details = if (cache.detailsEnabled) WidgetFlightDetails.fromFlight(progress.phase, data) else null
      cache.flights[key] = progress.copy(details = details)
    }
    write(context, cache, binding)
    render(context, cache, now)
  }

  @Synchronized
  fun stop(context: Context, flightId: String, binding: String) {
    val now = System.currentTimeMillis()
    val cache = read(context, now, binding)
    val key = hashPushIdentity(flightId)
    cache.flights.remove(key)
    cache.stopped[key] = now
    write(context, cache, binding)
    render(context, cache, now)
  }

  @Synchronized
  fun clear(context: Context) {
    val now = System.currentTimeMillis()
    val cache = read(context, now, null)
    try {
      write(context, cache, null)
    } finally {
      render(context, cache, now)
    }
  }

  @Synchronized
  fun detailsEnabled(context: Context): Boolean = read(context, System.currentTimeMillis(), null).detailsEnabled

  @Synchronized
  fun setDetailsEnabled(context: Context, enabled: Boolean, binding: String?) {
    val now = System.currentTimeMillis()
    val cache = read(context, now, binding).copy(detailsEnabled = enabled)
    try {
      write(context, cache, binding, verify = true)
      detailsBlocked = false
    } catch (cause: Exception) {
      detailsBlocked = true
      if (!enabled) cacheFile(context).delete()
      throw cause
    } finally {
      render(context, cache.copy(detailsEnabled = enabled && !detailsBlocked), now)
    }
  }

  @Synchronized
  fun refresh(context: Context, binding: String?) {
    val now = System.currentTimeMillis()
    render(context, read(context, now, binding), now)
  }

  fun cancelRefresh(context: Context) {
    val alarm = context.getSystemService(AlarmManager::class.java) ?: return
    refreshIntent(context, PendingIntent.FLAG_NO_CREATE)?.let(alarm::cancel)
  }

  private fun render(context: Context, cache: Cache, now: Long) {
    val manager = AppWidgetManager.getInstance(context) ?: return
    val ids = manager.getAppWidgetIds(ComponentName(context, PlanoFlightWidgetProvider::class.java))
    if (ids.isEmpty()) return
    val selected = selectWidgetFlight(cache.flights.values, now)
    val countdown = selected?.countdownTarget(now)
    val percent = selected?.percent(now)
    val details = selected?.takeIf { cache.detailsEnabled }?.let { detailText(context, it) }.orEmpty()
    val views = RemoteViews(context.packageName, R.layout.plano_flight_widget)
    views.setViewVisibility(R.id.plano_widget_title, if (details.isEmpty()) View.VISIBLE else View.GONE)
    views.setTextViewText(R.id.plano_widget_status, context.getString(statusLabel(selected?.phase)))
    views.setTextViewText(R.id.plano_widget_details, details)
    views.setViewVisibility(R.id.plano_widget_details, if (details.isEmpty()) View.GONE else View.VISIBLE)
    views.setViewVisibility(R.id.plano_widget_hint, if (selected == null) View.VISIBLE else View.GONE)
    views.setViewVisibility(R.id.plano_widget_progress, if (percent == null) View.GONE else View.VISIBLE)
    if (percent != null) views.setProgressBar(R.id.plano_widget_progress, 100, percent, false)
    views.setViewVisibility(R.id.plano_widget_countdown_row, if (countdown == null) View.GONE else View.VISIBLE)
    if (countdown != null) {
      val label = if (selected.phase.arrivalCountdown) R.string.plano_widget_until_arrival else R.string.plano_widget_until_departure
      views.setTextViewText(R.id.plano_widget_countdown_label, context.getString(label))
      views.setChronometerCountDown(R.id.plano_widget_countdown, true)
      views.setChronometer(R.id.plano_widget_countdown, SystemClock.elapsedRealtime() + countdown - now, null, true)
    } else {
      views.setChronometer(R.id.plano_widget_countdown, SystemClock.elapsedRealtime(), null, false)
    }
    openApp(context)?.let { views.setOnClickPendingIntent(R.id.plano_widget_root, it) }
    manager.updateAppWidget(ids, views)
    val nextUpdate = selected?.expiresAt?.let { expiry -> minOf(expiry, countdown ?: expiry) }
    scheduleRefresh(context, nextUpdate, now)
  }

  private fun detailText(context: Context, flight: WidgetFlightProgress): String {
    val details = flight.details ?: return ""
    val location = listOfNotNull(
      details.gate?.let { context.getString(R.string.plano_widget_gate, it) },
      details.terminal?.let { context.getString(R.string.plano_widget_terminal, it) },
    ).joinToString(" · ")
    val leg = if (flight.phase.departureDetails) R.string.plano_widget_departure_details else R.string.plano_widget_arrival_details
    return listOfNotNull(
      location.takeIf { it.isNotEmpty() }?.let { context.getString(leg, it) },
      details.seat?.let { context.getString(R.string.plano_widget_seat, it) },
    ).joinToString(" · ")
  }

  private fun scheduleRefresh(context: Context, at: Long?, now: Long) {
    if (at == null) {
      cancelRefresh(context)
      return
    }
    val alarm = context.getSystemService(AlarmManager::class.java) ?: return
    val intent = refreshIntent(context, PendingIntent.FLAG_UPDATE_CURRENT) ?: return
    // An inexact, non-wakeup alarm refreshes expiry while the device is awake.
    // The host's periodic update also covers an alarm delayed by idle policy.
    alarm.set(AlarmManager.ELAPSED_REALTIME, SystemClock.elapsedRealtime() + maxOf(1000L, at - now), intent)
  }

  private fun refreshIntent(context: Context, flags: Int): PendingIntent? {
    val intent = Intent(context, PlanoFlightWidgetProvider::class.java).setAction(REFRESH_ACTION)
    return PendingIntent.getBroadcast(context, REQUEST_CODE, intent, flags or PendingIntent.FLAG_IMMUTABLE)
  }

  private fun openApp(context: Context): PendingIntent? {
    val intent = context.packageManager.getLaunchIntentForPackage(context.packageName) ?: return null
    intent.addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP)
    return PendingIntent.getActivity(context, REQUEST_CODE, intent, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
  }

  private fun statusLabel(phase: WidgetFlightPhase?) = when (phase) {
    null -> R.string.plano_widget_no_flight
    WidgetFlightPhase.SCHEDULED -> R.string.plano_widget_scheduled
    WidgetFlightPhase.BOARDING -> R.string.plano_widget_boarding
    WidgetFlightPhase.TAXI_OUT -> R.string.plano_widget_taxiing
    WidgetFlightPhase.AIRBORNE -> R.string.plano_widget_airborne
    WidgetFlightPhase.DIVERTED -> R.string.plano_widget_diverted
    WidgetFlightPhase.LANDED -> R.string.plano_widget_landed
  }

  // The cache is app-private and excluded from Android backup/restore.
  private fun cacheFile(context: Context) = AtomicFile(File(context.noBackupFilesDir, "plano-flight-widget.json"))

  private fun read(context: Context, now: Long, binding: String?): Cache = runCatching {
    val data = JSONObject(cacheFile(context).readFully().toString(Charsets.UTF_8))
    val cache = Cache(detailsEnabled = !detailsBlocked && data.opt("detailsEnabled") == true)
    if (binding == null || data.optString("bindingKey") != hashPushIdentity(binding)) return cache
    val flights = data.optJSONArray("flights") ?: JSONArray()
    for (index in 0 until minOf(flights.length(), MAX_CANDIDATES)) {
      val entry = flights.getJSONObject(index)
      val details = if (cache.detailsEnabled) entry.optJSONObject("details")?.let {
        WidgetFlightDetails.fromFields(it.optString("gate"), it.optString("terminal"), it.optString("seat"))
      } else null
      val progress = WidgetFlightProgress(
        WidgetFlightPhase.valueOf(entry.getString("phase")),
        entry.getLong("observedAt"),
        entry.optLong("departureAt"),
        entry.optLong("arrivalAt"),
        details,
      )
      if (progress.isFresh(now)) cache.flights[entry.getString("key")] = progress
    }
    val stopped = data.optJSONObject("stopped") ?: JSONObject()
    stopped.keys().asSequence().take(MAX_STOPPED).forEach { key ->
      val at = stopped.optLong(key)
      if (at in now - WIDGET_FRESH_MILLIS..now) cache.stopped[key] = at
    }
    cache
  }.getOrElse { Cache() }

  private fun write(context: Context, cache: Cache, binding: String?, verify: Boolean = false) {
    val flights = JSONArray()
    cache.flights.entries.sortedWith(
      compareByDescending<Map.Entry<String, WidgetFlightProgress>> { it.value.phase.priority }.thenByDescending { it.value.observedAt },
    ).take(MAX_CANDIDATES).forEach { (key, progress) ->
      val entry = JSONObject().put("key", key).put("phase", progress.phase.name)
        .put("observedAt", progress.observedAt).put("departureAt", progress.departureAt).put("arrivalAt", progress.arrivalAt)
      if (cache.detailsEnabled) progress.details?.let { details ->
        entry.put("details", JSONObject().put("gate", details.gate).put("terminal", details.terminal).put("seat", details.seat))
      }
      flights.put(entry)
    }
    val stopped = JSONObject()
    cache.stopped.entries.sortedByDescending { it.value }.take(MAX_STOPPED).forEach { (key, at) -> stopped.put(key, at) }
    val json = JSONObject().put("detailsEnabled", cache.detailsEnabled).put("flights", flights).put("stopped", stopped)
    if (binding != null) json.put("bindingKey", hashPushIdentity(binding))
    val bytes = json.toString().toByteArray(Charsets.UTF_8)
    val file = cacheFile(context)
    val stream = file.startWrite()
    try {
      stream.write(bytes)
      file.finishWrite(stream)
    } catch (cause: Exception) {
      file.failWrite(stream)
      throw cause
    }
    if (verify) check(file.readFully().contentEquals(bytes)) { "Could not save widget privacy setting" }
  }
}
