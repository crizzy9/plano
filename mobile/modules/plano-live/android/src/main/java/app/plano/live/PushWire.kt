package app.plano.live

import java.security.MessageDigest

internal const val PUSH_CLOCK_SKEW_MILLIS = 5 * 60 * 1000L
private const val SOURCE_CLOCK_SKEW_MILLIS = 30_000L

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

internal fun acceptsLiveAfterStop(stoppedAt: Long, started: Boolean, observedAt: Long, sentAt: Long): Boolean {
  val delivery = sentAt.takeIf { it > 0 } ?: epochMillis(observedAt)
  return stoppedAt == 0L || started && delivery > stoppedAt && observedAt >= stoppedAt / 1000
}

internal data class LiveProgressNotification(val instance: String, val dismissed: Boolean = false) {
  fun dismiss(expectedInstance: String): LiveProgressNotification =
    if (instance == expectedInstance) copy(dismissed = true) else this
}

internal fun nextLiveProgressNotification(
  previous: LiveProgressNotification?,
  started: Boolean,
  ended: Boolean,
  newInstance: () -> String,
): LiveProgressNotification? = when {
  ended -> null
  !started && previous != null -> previous
  else -> LiveProgressNotification(newInstance())
}

internal fun hashPushIdentity(id: String) =
  MessageDigest.getInstance("SHA-256").digest(id.toByteArray(Charsets.UTF_8)).joinToString("") { "%02x".format(it) }

internal fun liveNotificationTag(flightId: String) = "plano-live-${hashPushIdentity(flightId)}"
