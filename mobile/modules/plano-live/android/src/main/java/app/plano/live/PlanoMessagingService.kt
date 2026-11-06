package app.plano.live

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.os.Build
import android.os.Bundle
import com.google.firebase.messaging.RemoteMessage
import expo.modules.notifications.service.ExpoFirebaseMessagingService

// The official NotificationCompat wire key also works when compiling with API 36,
// whose framework stub does not yet expose the API 36.1 constant.
private const val EXTRA_REQUEST_PROMOTED_ONGOING = "android.requestPromotedOngoing"

class PlanoMessagingService : ExpoFirebaseMessagingService() {
  private val manager by lazy { requireNotNull(getSystemService(NotificationManager::class.java)) }

  override fun onMessageReceived(remoteMessage: RemoteMessage) {
    val data = remoteMessage.data
    val binding = data["binding"]
    AndroidPushSession.accept(this, binding, remoteMessage.sentTime) {
      when (data["type"]) {
        "alert" -> showAlert(data, remoteMessage.messageId)
        "live" -> if (binding != null) showProgress(data, binding, remoteMessage.sentTime)
      }
    }
  }

  private fun showAlert(data: Map<String, String>, messageId: String?) {
    if (!manager.areNotificationsEnabled()) return
    val title = data["title"] ?: return
    val event = data["eventId"] ?: messageId ?: return
    manager.createNotificationChannel(NotificationChannel("flight-alerts", "Flight alerts", NotificationManager.IMPORTANCE_HIGH))
    val body = data["body"].orEmpty()
    val notification = Notification.Builder(this, "flight-alerts")
      .setSmallIcon(R.drawable.plano_flight)
      .setContentTitle(title)
      .setContentText(body)
      .setStyle(Notification.BigTextStyle().bigText(body))
      .setVisibility(Notification.VISIBILITY_PRIVATE)
      .setAutoCancel(true)
      .setContentIntent(openApp())
      .build()
    manager.notify("plano-alert-${hashPushIdentity(event)}", 0, notification)
  }

  private fun showProgress(data: Map<String, String>, binding: String, sentAt: Long) {
    val flightId = data["flightId"] ?: return
    val observedAt = data["observedEpoch"]?.toLongOrNull() ?: return
    val event = data["event"]
    if (event !in listOf("start", "update", "end")) return
    val status = data["status"].orEmpty()
    val ended = event == "end" || status in listOf("arrived", "cancelled")
    if (!ended && WidgetFlightPhase.fromStatus(status) == null) return
    if (!AndroidPushSession.advance(this, flightId, observedAt, sentAt)) return
    val tag = liveNotificationTag(flightId)
    val departure = data["departureEpoch"]?.toLongOrNull() ?: 0
    val arrival = data["arrivalEpoch"]?.toLongOrNull() ?: 0
    // Keep the opt-in widget independent of notification permission and pass
    // only the fields it can display. A widget host failure must not drop alerts.
    runCatching {
      PlanoFlightWidget.onFlight(
        this, flightId, binding, event == "start", ended,
        WidgetFlightProgress.fromEpochs(status, observedAt, departure, arrival),
        sentAt.takeIf { it > 0 } ?: epochMillis(observedAt),
      )
    }
    if (ended) {
      manager.cancel(tag, 0)
      return
    }

    if (!manager.areNotificationsEnabled()) return
    manager.createNotificationChannel(NotificationChannel("flight-progress", "Live flight progress", NotificationManager.IMPORTANCE_LOW))
    val now = System.currentTimeMillis()
    val percent = when {
      status == "landed" -> 100
      status in listOf("airborne", "diverted") && departure > 0 && arrival > departure ->
        ((now / 1000.0 - departure) / (arrival - departure) * 100).toInt().coerceIn(0, 99)
      else -> 0
    }
    val label = when (status) {
      "boarding" -> "Boarding"
      "taxi_out" -> "Taxiing"
      "airborne" -> "In flight"
      "diverted" -> "Diverted"
      "landed" -> "Landed"
      else -> "Scheduled"
    }
    val details = listOfNotNull(
      label,
      data["departureGate"]?.takeIf { it.isNotBlank() }?.let { "Dep gate $it" },
      data["arrivalGate"]?.takeIf { it.isNotBlank() }?.let { "Arr gate $it" },
      data["departureTerminal"]?.takeIf { it.isNotBlank() }?.let { "Dep terminal $it" },
      data["arrivalTerminal"]?.takeIf { it.isNotBlank() }?.let { "Arr terminal $it" },
      data["seat"]?.takeIf { it.isNotBlank() }?.let { "Seat $it" },
    ).joinToString(" · ")
    val route = "${data["origin"].orEmpty()} to ${data["destination"].orEmpty()}"
    val countdown = (if (status in listOf("airborne", "diverted")) arrival else departure) * 1000
    val builder = Notification.Builder(this, "flight-progress")
      .setSmallIcon(R.drawable.plano_flight)
      .setContentTitle(data["flightNumber"]?.let { "$it · $route" } ?: route)
      .setContentText(details)
      .setVisibility(Notification.VISIBILITY_PRIVATE)
      .setOngoing(true)
      .setOnlyAlertOnce(true)
      .setTimeoutAfter(20 * 60 * 1000L)
      .setContentIntent(openApp())
      .setShowWhen(countdown > now && status != "landed")

    if (countdown > now && status != "landed") {
      builder.setWhen(countdown).setUsesChronometer(true).setChronometerCountDown(true)
    }
    if (Build.VERSION.SDK_INT >= 36) {
      builder.setStyle(Notification.ProgressStyle().setProgress(percent)
        .setProgressSegments(listOf(Notification.ProgressStyle.Segment(100).setColor(0xff57bfa1.toInt()))))
        .addExtras(Bundle().apply { putBoolean(EXTRA_REQUEST_PROMOTED_ONGOING, true) })
    } else {
      builder.setProgress(100, percent, false)
    }
    manager.notify(tag, 0, builder.build())
  }

  private fun openApp(): PendingIntent? {
    val intent = packageManager.getLaunchIntentForPackage(packageName) ?: return null
    intent.addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP)
    return PendingIntent.getActivity(this, 0, intent, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
  }
}
