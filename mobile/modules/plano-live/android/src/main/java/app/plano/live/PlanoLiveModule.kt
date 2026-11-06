package app.plano.live

import expo.modules.kotlin.modules.Module
import expo.modules.kotlin.modules.ModuleDefinition

class PlanoLiveModule : Module() {
  override fun definition() = ModuleDefinition {
    Name("PlanoLive")

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

    AsyncFunction("setAndroidWidgetDetailsEnabled") { enabled: Boolean ->
      val context = checkNotNull(appContext.reactContext)
      AndroidPushSession.setWidgetDetailsEnabled(context, enabled)
    }

    AsyncFunction("stop") { flightId: String ->
      appContext.reactContext?.let { AndroidPushSession.stop(it, flightId) }
      Unit
    }

    AsyncFunction("clear") {
      appContext.reactContext?.let { AndroidPushSession.clear(it) }
      Unit
    }
  }
}
