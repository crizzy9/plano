package app.plano.live

import java.security.MessageDigest

internal const val PUSH_CLOCK_SKEW_MILLIS = 5 * 60 * 1000L
private const val SOURCE_CLOCK_SKEW_MILLIS = 30_000L
private const val NANOS_PER_SECOND = 1_000_000_000L
private const val NANOS_PER_MILLI = 1_000_000L

internal fun acceptsPushBinding(active: String?, supplied: String?, sentAt: Long, now: Long) =
  active != null && supplied == active && sentAt in 0..now + PUSH_CLOCK_SKEW_MILLIS

internal fun validPushObservation(observedAt: Long, sentAt: Long, now: Long) =
  validPushObservationMillis(epochMillis(observedAt), sentAt, now)

internal fun validPushObservationMillis(observed: Long, sentAt: Long, now: Long): Boolean {
  // FCM reports zero when a sent time is unavailable. Use a tighter device-time
  // bound then, because no transport timestamp corroborates the observation.
  if (observed == 0L || sentAt !in 0..now + PUSH_CLOCK_SKEW_MILLIS) return false
  if (sentAt == 0L) return observed <= now + SOURCE_CLOCK_SKEW_MILLIS
  return observed <= now + PUSH_CLOCK_SKEW_MILLIS && observed - sentAt <= SOURCE_CLOCK_SKEW_MILLIS
}

internal data class LiveObservation(val seconds: Long, val nanos: Long = 0L) {
  val millis: Long
    get() = if (nanos == 0L) epochMillis(seconds) else nanos / NANOS_PER_MILLI

  fun isNewerThan(previous: LiveObservation, sentAt: Long, previousSentAt: Long) = when {
    seconds != previous.seconds -> seconds > previous.seconds
    // Within a second, a precise observation outranks legacy data from an old outbox row.
    nanos != previous.nanos -> nanos > previous.nanos
    else -> sentAt > previousSentAt
  }
}

internal fun parseLiveObservation(data: Map<String, String>): LiveObservation? {
  val seconds = data["observedEpoch"]?.toLongOrNull()?.takeIf { epochMillis(it) > 0L } ?: return null
  val encoded = data["liveObservedNs"] ?: return LiveObservation(seconds)
  val nanos = encoded.toLongOrNull()?.takeIf { it > 0 && it / NANOS_PER_SECOND == seconds } ?: return null
  return LiveObservation(seconds, nanos)
}

internal fun acceptsLiveAfterStop(stoppedAt: Long, started: Boolean, observation: LiveObservation, sentAt: Long): Boolean {
  if (stoppedAt == 0L) return true
  val delivery = sentAt.takeIf { it > 0 } ?: observation.millis
  val observedAfterStop = if (observation.nanos == 0L) observation.seconds >= stoppedAt / 1000
    else observation.millis >= stoppedAt
  return started && delivery > stoppedAt && observedAfterStop
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
