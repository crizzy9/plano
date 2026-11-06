package app.plano.live

import android.app.AlarmManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.SystemClock
import com.google.firebase.messaging.RemoteMessage
import expo.modules.notifications.service.ExpoFirebaseMessagingService

// The official NotificationCompat wire key also works when compiling with API 36,
// whose framework stub does not yet expose the API 36.1 constant.
private const val EXTRA_REQUEST_PROMOTED_ONGOING = "android.requestPromotedOngoing"
internal const val LEGACY_PROGRESS_EXPIRY_ACTION = "app.plano.live.EXPIRE_FLIGHT_PROGRESS"
private const val FLIGHT_ALERTS_CHANNEL = "flight-alerts"
private const val FLIGHT_PROGRESS_CHANNEL = "flight-progress"
private val notificationFlightId = Regex("[A-Za-z0-9_.-]{1,128}")

internal fun validNotificationFlightId(id: String?) =
  id != null && id != "." && id != ".." && notificationFlightId.matches(id)

private fun notificationDestination(flightId: String?, inboundId: String?): Uri? {
  if (!validNotificationFlightId(flightId)) return null
  if (inboundId == null) return Uri.parse("plano://notification/flight/$flightId")
  if (!validNotificationFlightId(inboundId) || inboundId == flightId) return null
  return Uri.parse("plano://notification/connection/$flightId/$inboundId")
}

class PlanoMessagingService : ExpoFirebaseMessagingService() {
  private val manager by lazy { requireNotNull(getSystemService(NotificationManager::class.java)) }

  override fun onMessageReceived(remoteMessage: RemoteMessage) {
    val data = remoteMessage.data
    val binding = data["binding"]
    AndroidPushSession.accept(this, binding, remoteMessage.sentTime) {
      when (data["type"]) {
        "alert" -> showAlert(data, remoteMessage.messageId, remoteMessage.sentTime)
        "live" -> if (binding != null) showProgress(data, binding, remoteMessage.sentTime)
      }
    }
  }

  private fun showAlert(data: Map<String, String>, messageId: String?, sentAt: Long) {
    val title = data["title"] ?: return
    val event = data["eventId"] ?: messageId ?: return
    val flightId = data["flightId"]
    val metadata = parseFlightAlertMetadata(data, sentAt, System.currentTimeMillis()) ?: return
    val decision = runCatching { AndroidPushSession.advanceAlert(this, flightId, metadata) }.getOrNull() ?: return
    decision.cancelTags.forEach { manager.cancel(it, 0) }
    if (!manager.areNotificationsEnabled()) return
    val remaining = metadata.expiresAt?.minus(System.currentTimeMillis())
    if (remaining != null && remaining <= 0L) return
    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
      manager.createNotificationChannel(NotificationChannel(FLIGHT_ALERTS_CHANNEL, "Flight alerts", NotificationManager.IMPORTANCE_HIGH))
    }
    val body = data["body"].orEmpty()
    val builder = notificationBuilder(FLIGHT_ALERTS_CHANNEL, highPriority = true)
      .setSmallIcon(R.drawable.plano_flight)
      .setContentTitle(title)
      .setContentText(body)
      .setStyle(Notification.BigTextStyle().bigText(body))
      .setVisibility(Notification.VISIBILITY_PRIVATE)
      .setAutoCancel(true)
      .setContentIntent(openApp(flightId, data["connectionInboundId"]))
    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O && decision.notificationTag != null && remaining != null) {
      builder.setTimeoutAfter(remaining)
    }
    manager.notify(decision.notificationTag ?: "plano-alert-${hashPushIdentity(event)}", 0, builder.build())
  }

  private fun showProgress(data: Map<String, String>, binding: String, sentAt: Long) {
    val flightId = data["flightId"]?.takeIf(::validNotificationFlightId) ?: return
    val observedAt = data["observedEpoch"]?.toLongOrNull() ?: return
    val event = data["event"]
    if (event !in listOf("start", "update", "end")) return
    val status = data["status"]
    val ended = event == "end" || status in listOf("arrived", "cancelled")
    val started = event == "start" && !ended
    val progress = if (ended) null else {
      val departure = data["departureEpoch"]?.toLongOrNull() ?: 0
      val arrival = data["arrivalEpoch"]?.toLongOrNull() ?: 0
      val state = WidgetFlightProgress.fromEpochs(status, observedAt, departure, arrival) ?: return
      if (!state.isFresh(System.currentTimeMillis())) return
      state
    }
    if (!AndroidPushSession.advance(this, flightId, observedAt, sentAt, started = started)) return
    val tag = liveNotificationTag(flightId)
    // Keep the widget independent of notification permission. Its own privacy
    // setting controls which details it can keep from accepted flight updates.
    runCatching {
      PlanoFlightWidget.onFlight(
        this, flightId, binding, started, ended, progress,
        sentAt.takeIf { it > 0 } ?: epochMillis(observedAt),
        data,
      )
    }
    if (progress == null) {
      manager.cancel(tag, 0)
      return
    }

    if (!manager.areNotificationsEnabled()) return
    val now = System.currentTimeMillis()
    if (!progress.isFresh(now)) return
    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
      manager.createNotificationChannel(NotificationChannel(
        FLIGHT_PROGRESS_CHANNEL, getString(R.string.plano_live_progress_channel), NotificationManager.IMPORTANCE_LOW,
      ))
    }
    val phase = progress.phase
    val label = getString(when (phase) {
      WidgetFlightPhase.SCHEDULED -> R.string.plano_widget_scheduled
      WidgetFlightPhase.BOARDING -> R.string.plano_widget_boarding
      WidgetFlightPhase.TAXI_OUT -> R.string.plano_widget_taxiing
      WidgetFlightPhase.AIRBORNE -> R.string.plano_widget_airborne
      WidgetFlightPhase.DIVERTED -> R.string.plano_widget_diverted
      WidgetFlightPhase.LANDED -> R.string.plano_widget_landed
    })
    val details = WidgetFlightDetails.fromFlight(phase, data)
    val location = listOfNotNull(
      details?.gate?.let { getString(R.string.plano_widget_gate, it) },
      details?.terminal?.let { getString(R.string.plano_widget_terminal, it) },
    ).joinToString(" · ")
    val locationLabel = if (phase.departureDetails) R.string.plano_widget_departure_details else R.string.plano_widget_arrival_details
    val summary = listOfNotNull(
      label,
      location.takeIf { it.isNotEmpty() }?.let { getString(locationLabel, it) },
      details?.seat?.let { getString(R.string.plano_widget_seat, it) },
    ).joinToString(" · ")
    val origin = data["origin"]?.takeIf { it.isNotBlank() }
    val destination = data["destination"]?.takeIf { it.isNotBlank() }
    val route = if (origin != null && destination != null) getString(R.string.plano_live_route, origin, destination) else null
    val genericTitle = getString(R.string.plano_live_progress_title)
    val title = listOfNotNull(data["flightNumber"]?.takeIf { it.isNotBlank() }, route)
      .joinToString(" · ").ifBlank { genericTitle }
    val publicVersion = progressBuilder(progress, now)
      .setContentTitle(genericTitle)
      .setContentText(label)
      .setVisibility(Notification.VISIBILITY_PUBLIC)
      .build()
    val builder = progressBuilder(progress, now)
      .setContentTitle(title)
      .setContentText(summary)
      .setVisibility(Notification.VISIBILITY_PRIVATE)
      .setPublicVersion(publicVersion)
      .setContentIntent(openApp(flightId))
    if (Build.VERSION.SDK_INT >= 36) {
      builder.addExtras(Bundle().apply { putBoolean(EXTRA_REQUEST_PROMOTED_ONGOING, true) })
    }
    manager.notify(tag, 0, builder.build())
    if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) {
      scheduleLegacyExpiry(flightId, binding, observedAt, progress.expiresAt)
    }
  }

  private fun progressBuilder(progress: WidgetFlightProgress, now: Long): Notification.Builder {
    val countdown = progress.countdownTarget(now)
    val builder = notificationBuilder(FLIGHT_PROGRESS_CHANNEL, highPriority = false)
      .setSmallIcon(R.drawable.plano_flight)
      .setOngoing(true)
      .setOnlyAlertOnce(true)
      .setShowWhen(countdown != null)
    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
      builder.setTimeoutAfter(progress.expiresAt - now)
    }
    if (countdown != null) {
      builder.setWhen(countdown).setUsesChronometer(true).setChronometerCountDown(true)
    }
    progress.percent(now)?.let { percent ->
      if (Build.VERSION.SDK_INT >= 36) {
        builder.setStyle(Notification.ProgressStyle().setProgress(percent)
          .setProgressSegments(listOf(Notification.ProgressStyle.Segment(100).setColor(0xff57bfa1.toInt()))))
      } else {
        builder.setProgress(100, percent, false)
      }
    }
    return builder
  }

  @Suppress("DEPRECATION")
  private fun notificationBuilder(channel: String, highPriority: Boolean): Notification.Builder {
    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) return Notification.Builder(this, channel)
    return Notification.Builder(this).apply {
      setPriority(if (highPriority) Notification.PRIORITY_HIGH else Notification.PRIORITY_LOW)
      if (highPriority) setDefaults(Notification.DEFAULT_SOUND)
    }
  }

  private fun scheduleLegacyExpiry(flightId: String, binding: String, observedEpoch: Long, expiresAt: Long) {
    val alarm = getSystemService(AlarmManager::class.java) ?: return
    val intent = Intent(this, PlanoFlightWidgetProvider::class.java)
      .setAction(LEGACY_PROGRESS_EXPIRY_ACTION)
      .setData(Uri.parse("plano://live-progress-expiry/${hashPushIdentity(flightId)}"))
      .putExtra("flightId", flightId)
      .putExtra("binding", binding)
      .putExtra("observedEpoch", observedEpoch)
    val pending = PendingIntent.getBroadcast(
      this, 0, intent, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
    ) ?: return
    val delay = (expiresAt - System.currentTimeMillis()).coerceAtLeast(0L)
    alarm.set(AlarmManager.ELAPSED_REALTIME, SystemClock.elapsedRealtime() + delay, pending)
  }

  private fun openApp(flightId: String?, inboundId: String? = null): PendingIntent? {
    val intent = packageManager.getLaunchIntentForPackage(packageName) ?: return null
    if (intent.component == null) return null
    notificationDestination(flightId, inboundId)?.let { destination ->
      intent.action = Intent.ACTION_VIEW
      intent.data = destination
    }
    intent.addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP)
    return PendingIntent.getActivity(this, 0, intent, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
  }
}
