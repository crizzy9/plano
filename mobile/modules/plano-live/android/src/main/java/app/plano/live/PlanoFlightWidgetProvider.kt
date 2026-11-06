package app.plano.live

import android.app.AlarmManager
import android.app.PendingIntent
import android.appwidget.AppWidgetManager
import android.appwidget.AppWidgetProvider
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.SystemClock
import android.text.format.DateUtils
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
    when (intent.action) {
      PlanoFlightWidget.REFRESH_ACTION -> AndroidPushSession.refreshWidget(context)
      STOP_PROGRESS_ACTION, DISMISS_PROGRESS_NOTIFICATION_ACTION -> {
        val flightId = intent.getStringExtra("flightId")?.takeIf(::validNotificationFlightId) ?: return
        val binding = intent.getStringExtra("binding") ?: return
        val instance = intent.getStringExtra("instance") ?: return
        val expected = LiveProgressAction(binding, instance)
        runCatching {
          if (intent.action == STOP_PROGRESS_ACTION) AndroidPushSession.stop(context, flightId, expected)
          else AndroidPushSession.dismissProgressNotification(context, flightId, expected)
        }
      }
      LEGACY_PROGRESS_EXPIRY_ACTION -> {
        val flightId = intent.getStringExtra("flightId") ?: return
        val binding = intent.getStringExtra("binding") ?: return
        if (intent.getLongExtra("expiryElapsed", 0) > SystemClock.elapsedRealtime()) return
        val observation = LiveObservation(intent.getLongExtra("observedEpoch", 0), intent.getLongExtra("observedNs", 0))
        AndroidPushSession.expireProgress(context, flightId, binding, observation)
      }
      else -> super.onReceive(context, intent)
    }
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
  private val hashKeyPattern = Regex("[0-9a-f]{64}")
  private var detailsBlocked = false

  private data class Cache(
    val detailsEnabled: Boolean = false,
    val flights: MutableMap<String, WidgetFlightProgress> = mutableMapOf(),
    val stopped: MutableMap<String, Long> = mutableMapOf(),
    var pinnedFlightKey: String? = null,
    var needsRewrite: Boolean = false,
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
    observation: LiveObservation,
    sentAt: Long,
    data: Map<String, String>,
    hiddenKeys: Set<String>,
  ): Boolean {
    val now = System.currentTimeMillis()
    val cache = read(context, now, binding, hiddenKeys)
    val key = hashPushIdentity(flightId)

    if (ended) {
      cache.removeFlight(key)
      try {
        write(context, cache, binding)
      } finally {
        render(context, cache, now, binding)
      }
      return true
    }
    if (progress == null || !progress.isFresh(now)) return false
    if (!acceptsLiveAfterStop(cache.stopped[key] ?: 0, started, observation, sentAt)) return false
    cache.stopped.remove(key)
    val details = if (cache.detailsEnabled) WidgetFlightDetails.fromFlight(progress.phase, data) else null
    cache.flights[key] = progress.copy(details = details, flightId = flightId.takeIf(::validNotificationFlightId))
    write(context, cache, binding)
    render(context, cache, now, binding)
    return true
  }

  @Synchronized
  fun stop(context: Context, flightId: String, binding: String, hiddenKeys: Set<String>, now: Long) {
    val cache = read(context, now, binding, hiddenKeys)
    val key = hashPushIdentity(flightId)
    cache.removeFlight(key)
    cache.stopped[key] = now
    try {
      write(context, cache, binding)
    } finally {
      render(context, cache, now, binding)
    }
  }

  @Synchronized
  fun clear(context: Context) {
    val now = System.currentTimeMillis()
    val cache = read(context, now, null)
    try {
      write(context, cache, null)
    } finally {
      render(context, cache, now, null)
    }
  }

  @Synchronized
  fun detailsEnabled(context: Context): Boolean = read(context, System.currentTimeMillis(), null).detailsEnabled

  @Synchronized
  fun isPinned(context: Context, binding: String, flightId: String, hiddenKeys: Set<String>): Boolean {
    val cache = read(context, System.currentTimeMillis(), binding, hiddenKeys)
    return cache.pinnedFlightKey == hashPushIdentity(flightId)
  }

  @Synchronized
  fun setPinned(context: Context, binding: String, flightId: String, pinned: Boolean, hiddenKeys: Set<String>): Boolean {
    if (!validNotificationFlightId(flightId)) return false
    val now = System.currentTimeMillis()
    val cache = read(context, now, binding, hiddenKeys)
    val key = hashPushIdentity(flightId)
    if (pinned && (key in hiddenKeys || key in cache.stopped)) return false
    if (!pinned && cache.pinnedFlightKey != null && cache.pinnedFlightKey != key) return false

    val nextKey = key.takeIf { pinned }
    if (cache.pinnedFlightKey != nextKey || cache.needsRewrite) {
      cache.pinnedFlightKey = nextKey
      write(context, cache, binding, verify = true)
    }
    render(context, cache, now, binding)
    return true
  }

  private fun Cache.removeFlight(key: String) {
    flights.remove(key)
    if (pinnedFlightKey == key) pinnedFlightKey = null
  }

  @Synchronized
  fun setDetailsEnabled(context: Context, enabled: Boolean, binding: String?, hiddenKeys: Set<String>) {
    val now = System.currentTimeMillis()
    val cache = read(context, now, binding, hiddenKeys).copy(detailsEnabled = enabled)
    try {
      write(context, cache, binding, verify = true)
      detailsBlocked = false
    } catch (cause: Exception) {
      detailsBlocked = true
      if (!enabled) cacheFile(context).delete()
      throw cause
    } finally {
      render(context, cache.copy(detailsEnabled = enabled && !detailsBlocked), now, binding)
    }
  }

  @Synchronized
  fun refresh(context: Context, binding: String?, hiddenKeys: Set<String>) {
    val now = System.currentTimeMillis()
    val cache = read(context, now, binding, hiddenKeys)
    if (cache.needsRewrite) runCatching { write(context, cache, binding) }
    render(context, cache, now, binding)
  }

  @Synchronized
  fun resolveTap(context: Context, binding: String, key: String, hiddenKeys: Set<String>): String? {
    if (!hashKeyPattern.matches(key)) return null
    val now = System.currentTimeMillis()
    val cache = read(context, now, binding, hiddenKeys)
    val selected = selectWidgetFlight(cache.flights, now, cache.pinnedFlightKey) ?: return null
    return selected.flightId?.takeIf { selected.tapKey(binding) == key }
  }

  fun cancelRefresh(context: Context) {
    val alarm = context.getSystemService(AlarmManager::class.java) ?: return
    refreshIntent(context, PendingIntent.FLAG_NO_CREATE)?.let(alarm::cancel)
  }

  private fun render(context: Context, cache: Cache, now: Long, binding: String?) {
    val manager = AppWidgetManager.getInstance(context) ?: return
    val ids = manager.getAppWidgetIds(ComponentName(context, PlanoFlightWidgetProvider::class.java))
    if (ids.isEmpty()) return
    val selected = selectWidgetFlight(cache.flights, now, cache.pinnedFlightKey)
    val waitingForPin = selected == null && cache.pinnedFlightKey != null
    val target = selected?.countdownTarget(now)
    val percent = selected?.percent(now)
    val details = selected?.takeIf { cache.detailsEnabled }?.let { detailText(context, it) }.orEmpty()
    val views = RemoteViews(context.packageName, R.layout.plano_flight_widget)
    views.setViewVisibility(R.id.plano_widget_title, if (details.isEmpty()) View.VISIBLE else View.GONE)
    val status = if (waitingForPin) R.string.plano_widget_waiting else statusLabel(selected?.phase)
    views.setTextViewText(R.id.plano_widget_status, context.getString(status))
    views.setTextViewText(R.id.plano_widget_details, details)
    views.setViewVisibility(R.id.plano_widget_details, if (details.isEmpty()) View.GONE else View.VISIBLE)
    views.setViewVisibility(R.id.plano_widget_hint, if (selected == null) View.VISIBLE else View.GONE)
    val hint = if (waitingForPin) R.string.plano_widget_pinned_hint else R.string.plano_widget_hint
    views.setTextViewText(R.id.plano_widget_hint, context.getString(hint))
    views.setViewVisibility(R.id.plano_widget_progress, if (percent == null) View.GONE else View.VISIBLE)
    if (percent != null) views.setProgressBar(R.id.plano_widget_progress, 100, percent, false)
    views.setViewVisibility(R.id.plano_widget_estimate_row, if (target == null) View.GONE else View.VISIBLE)
    if (target != null) {
      val label = if (selected.phase.arrivalCountdown) R.string.plano_widget_estimated_arrival else R.string.plano_widget_estimated_departure
      // A fixed estimate stays meaningful if the widget host delays its next refresh.
      val dayFlags = if (DateUtils.isToday(target)) 0 else DateUtils.FORMAT_SHOW_WEEKDAY or DateUtils.FORMAT_ABBREV_WEEKDAY
      views.setTextViewText(R.id.plano_widget_estimate_label, context.getString(label))
      views.setTextViewText(R.id.plano_widget_estimate_time, DateUtils.formatDateTime(context, target, DateUtils.FORMAT_SHOW_TIME or dayFlags))
    }
    val tapKey = binding?.let { selected?.tapKey(it) }
    openApp(context, tapKey)?.let { views.setOnClickPendingIntent(R.id.plano_widget_root, it) }
    manager.updateAppWidget(ids, views)
    val nextUpdate = selected?.nextRefreshAt(now)
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

  private fun WidgetFlightProgress.tapKey(binding: String): String? =
    flightId?.let { hashPushIdentity("$binding\u0000$it") }

  private fun openApp(context: Context, tapKey: String?): PendingIntent? {
    val intent = context.packageManager.getLaunchIntentForPackage(context.packageName) ?: return null
    if (intent.component == null) return null
    if (tapKey != null) {
      intent.action = Intent.ACTION_VIEW
      intent.data = Uri.parse("plano://notification/widget/$tapKey")
    }
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

  private fun read(context: Context, now: Long, binding: String?, hiddenKeys: Set<String> = emptySet()): Cache = runCatching {
    val data = JSONObject(cacheFile(context).readFully().toString(Charsets.UTF_8))
    val cache = Cache(detailsEnabled = !detailsBlocked && data.opt("detailsEnabled") == true)
    if (binding == null || data.optString("bindingKey") != hashPushIdentity(binding)) return cache
    cache.pinnedFlightKey = data.optString("pinnedFlightKey").takeIf(hashKeyPattern::matches)
    if (cache.pinnedFlightKey?.let { it in hiddenKeys } == true) {
      cache.pinnedFlightKey = null
      cache.needsRewrite = true
    }
    val flights = data.optJSONArray("flights") ?: JSONArray()
    for (index in 0 until minOf(flights.length(), MAX_CANDIDATES)) {
      val entry = flights.getJSONObject(index)
      val key = entry.getString("key")
      // Honor the session fence even when an older widget cache could not be rewritten.
      if (key in hiddenKeys) {
        cache.needsRewrite = true
        continue
      }
      val flightId = (entry.opt("flightId") as? String)?.takeIf {
        validNotificationFlightId(it) && hashPushIdentity(it) == key
      }
      val details = if (cache.detailsEnabled) entry.optJSONObject("details")?.let {
        WidgetFlightDetails.fromFields(it.optString("gate"), it.optString("terminal"), it.optString("seat"))
      } else null
      val progress = WidgetFlightProgress(
        WidgetFlightPhase.valueOf(entry.getString("phase")),
        entry.getLong("observedAt"),
        entry.optLong("departureAt"),
        entry.optLong("arrivalAt"),
        details,
        flightId,
      )
      if (progress.isFresh(now)) cache.flights[key] = progress
      else if (flightId != null) cache.needsRewrite = true
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
      compareByDescending<Map.Entry<String, WidgetFlightProgress>> { it.key == cache.pinnedFlightKey }
        .thenByDescending { it.value.phase.priority }.thenByDescending { it.value.observedAt },
    ).take(MAX_CANDIDATES).forEach { (key, progress) ->
      val entry = JSONObject().put("key", key).put("phase", progress.phase.name)
        .put("observedAt", progress.observedAt).put("departureAt", progress.departureAt).put("arrivalAt", progress.arrivalAt)
      progress.flightId?.let { entry.put("flightId", it) }
      if (cache.detailsEnabled) progress.details?.let { details ->
        entry.put("details", JSONObject().put("gate", details.gate).put("terminal", details.terminal).put("seat", details.seat))
      }
      flights.put(entry)
    }
    val stopped = JSONObject()
    cache.stopped.entries.sortedByDescending { it.value }.take(MAX_STOPPED).forEach { (key, at) -> stopped.put(key, at) }
    val json = JSONObject().put("detailsEnabled", cache.detailsEnabled).put("flights", flights).put("stopped", stopped)
    if (binding != null) {
      json.put("bindingKey", hashPushIdentity(binding))
      cache.pinnedFlightKey?.let { json.put("pinnedFlightKey", it) }
    }
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
    if (verify) check(file.readFully().contentEquals(bytes)) { "Could not save widget setting" }
  }
}
