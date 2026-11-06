package app.plano.live

import expo.modules.kotlin.modules.Module
import expo.modules.kotlin.modules.ModuleDefinition
import java.util.concurrent.CopyOnWriteArraySet

internal object FlightAlertEvents {
  const val NAME = "onFlightAlert"
  val listeners = CopyOnWriteArraySet<(Map<String, String>) -> Unit>()

  fun emit(origin: String, flightId: String?, inboundId: String?) {
    if (listeners.isEmpty() || origin.isEmpty()) return
    val flight = flightId?.takeIf(::validNotificationFlightId) ?: return
    if (inboundId != null && (!validNotificationFlightId(inboundId) || inboundId == flight)) return
    val hint = buildMap {
      put("origin", origin)
      put("flightId", flight)
      if (inboundId != null) put("connectionInboundId", inboundId)
    }
    // Expo schedules delivery on the JS runtime. A tearing-down runtime
    // must not interrupt the service's notification handling.
    listeners.forEach { listener -> runCatching { listener(hint) } }
  }
}

class PlanoLiveModule : Module() {
  private val flightAlertListener: (Map<String, String>) -> Unit = { hint ->
    sendEvent(FlightAlertEvents.NAME, hint)
  }

  override fun definition() = ModuleDefinition {
    Name("PlanoLive")
    Events(FlightAlertEvents.NAME)
    OnStartObserving(FlightAlertEvents.NAME) { FlightAlertEvents.listeners.add(flightAlertListener) }
    OnStopObserving(FlightAlertEvents.NAME) { FlightAlertEvents.listeners.remove(flightAlertListener) }
    OnDestroy { FlightAlertEvents.listeners.remove(flightAlertListener) }

    AsyncFunction("selectAndroidPushOrigin") { origin: String ->
      appContext.reactContext?.let { AndroidPushSession.selectOrigin(it, origin) }
    }

    AsyncFunction("androidPushStatus") { origin: String ->
      appContext.reactContext?.let { AndroidPushSession.status(it, origin) }
    }

    AsyncFunction("enableAndroidPush") { origin: String, generation: String, explicit: Boolean ->
      val context = appContext.reactContext
      context != null && AndroidPushSession.enable(context, origin, generation, explicit)
    }

    AsyncFunction("disableAndroidPush") { origin: String, expected: String?, suppress: Boolean ->
      appContext.reactContext?.let { AndroidPushSession.disable(it, origin, expected, suppress) }
    }

    AsyncFunction("androidWidgetDetailsEnabled") {
      val context = checkNotNull(appContext.reactContext)
      PlanoFlightWidget.detailsEnabled(context)
    }

    AsyncFunction("resolveWidgetFlight") { origin: String, key: String ->
      appContext.reactContext?.let { AndroidPushSession.resolveWidgetFlight(it, origin, key) }
    }

    AsyncFunction("androidWidgetFlightPinned") { origin: String, flightId: String ->
      appContext.reactContext?.let { AndroidPushSession.widgetFlightPinned(it, origin, flightId) }
    }

    AsyncFunction("setAndroidWidgetFlightPinned") { origin: String, flightId: String, pinned: Boolean ->
      appContext.reactContext?.let { AndroidPushSession.setWidgetFlightPinned(it, origin, flightId, pinned) } ?: false
    }

    AsyncFunction("setAndroidWidgetDetailsEnabled") { enabled: Boolean ->
      val context = checkNotNull(appContext.reactContext)
      AndroidPushSession.setWidgetDetailsEnabled(context, enabled)
    }

    AsyncFunction("stop") { flightId: String ->
      appContext.reactContext?.let { AndroidPushSession.stop(it, flightId) }
      Unit
    }

    AsyncFunction("stopLiveActivityInSession") { origin: String, binding: String, flightId: String ->
      appContext.reactContext?.let { AndroidPushSession.stopInSession(it, origin, binding, flightId) }
      Unit
    }

    AsyncFunction("clear") {
      appContext.reactContext?.let { AndroidPushSession.clear(it) }
      Unit
    }
  }
}
