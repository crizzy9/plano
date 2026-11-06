package app.plano.live

import android.app.NotificationManager
import expo.modules.kotlin.modules.Module
import expo.modules.kotlin.modules.ModuleDefinition

class PlanoLiveModule : Module() {
  override fun definition() = ModuleDefinition {
    Name("PlanoLive")

    AsyncFunction("stop") { flightId: String ->
      val manager = appContext.reactContext?.getSystemService(NotificationManager::class.java)
      manager?.cancel("plano-live-$flightId", 0)
    }

    AsyncFunction("clear") {
      val manager = appContext.reactContext?.getSystemService(NotificationManager::class.java)
      manager?.activeNotifications?.filter { it.tag?.startsWith("plano-") == true }?.forEach {
        manager.cancel(it.tag, it.id)
      }
    }
  }
}
