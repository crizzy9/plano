import ActivityKit
import ExpoModulesCore
import Foundation
import Security
import UIKit

public final class PlanoLiveModule: Module {
  public func definition() -> ModuleDefinition {
    Name("PlanoLive")

    AsyncFunction("configure") { (server: String, token: String) async throws in
      try await PlanoActivityCoordinator.shared.configure(server: server, token: token)
    }

    AsyncFunction("clear") { () async in
      await PlanoActivityCoordinator.shared.clear()
    }

    AsyncFunction("start") { (stateJSON: String) async throws in
      try await PlanoActivityCoordinator.shared.start(stateJSON: stateJSON)
    }

    AsyncFunction("stop") { (flightId: String) async in
      await PlanoActivityCoordinator.shared.stop(flightId: flightId)
    }
  }
}

public final class PlanoLiveAppDelegateSubscriber: ExpoAppDelegateSubscriber {
  public func application(_ application: UIApplication, didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
    Task { @MainActor in PlanoActivityCoordinator.shared.restore() }
    return true
  }
}

private struct ActivityRegistration: Codable, Equatable {
  let server: String
  let token: String

  var baseURL: URL? {
    guard let components = URLComponents(string: server),
          let scheme = components.scheme, let host = components.host,
          components.user == nil, components.password == nil,
          components.path.isEmpty || components.path == "/",
          components.query == nil, components.fragment == nil else { return nil }
    if scheme == "https" { return components.url }
    #if DEBUG
    if scheme == "http", ["localhost", "127.0.0.1", "::1", "[::1]"].contains(host) { return components.url }
    #endif
    return nil
  }
}

private struct DeviceRegistration: Encodable {
  let platform = "ios"
  let token: String
  let kind: String
  let flightId: String?
  let requestUpdateToken: Bool?
}

private enum RegistrationError: LocalizedError {
  case invalidServer, keychain, disabled, notConfigured, inactive

  var errorDescription: String? {
    switch self {
    case .invalidServer: return "Enter a valid Plano server address."
    case .keychain: return "Plano couldn’t save its Live Activity registration."
    case .disabled: return "Allow Live Activities for Plano in your device settings."
    case .notConfigured: return "Enable flight alerts in Plano Settings before starting a Live Activity."
    case .inactive: return "This flight has already finished."
    }
  }
}

private enum LiveKeychain {
  static let query: [String: Any] = [
    kSecClass as String: kSecClassGenericPassword,
    kSecAttrService as String: "PlanoLiveRegistration",
    kSecAttrAccount as String: "active-server",
  ]

  static func save(_ registration: ActivityRegistration) throws {
    let data = try JSONEncoder().encode(registration)
    let values: [String: Any] = [
      kSecValueData as String: data,
      kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
    ]
    var status = SecItemUpdate(query as CFDictionary, values as CFDictionary)
    if status == errSecItemNotFound {
      status = SecItemAdd(query.merging(values) { _, new in new } as CFDictionary, nil)
    }
    if status != errSecSuccess { throw RegistrationError.keychain }
  }

  static func load() -> ActivityRegistration? {
    let readQuery = query.merging([kSecReturnData as String: true, kSecMatchLimit as String: kSecMatchLimitOne]) { _, new in new }
    var item: CFTypeRef?
    guard SecItemCopyMatching(readQuery as CFDictionary, &item) == errSecSuccess,
          let data = item as? Data else { return nil }
    return try? JSONDecoder().decode(ActivityRegistration.self, from: data)
  }

  static func clear() {
    SecItemDelete(query as CFDictionary)
  }
}

@MainActor
private final class PlanoActivityCoordinator {
  static let shared = PlanoActivityCoordinator()
  private var registration: ActivityRegistration?
  private var startTokens: Task<Void, Never>?
  private var activities: Task<Void, Never>?
  private var tokenTasks: [String: Task<Void, Never>] = [:]

  func restore() {
    guard registration == nil, let saved = LiveKeychain.load(), saved.baseURL != nil else { return }
    observe(registration: saved)
  }

  func configure(server: String, token: String) async throws {
    let next = ActivityRegistration(server: server, token: token)
    guard next.baseURL != nil, !token.isEmpty else { throw RegistrationError.invalidServer }
    if next == registration { return }
    if let previous = registration, previous.server != server { await clear() }
    try LiveKeychain.save(next)
    observe(registration: next)
  }

  private func observe(registration: ActivityRegistration) {
    cancelObservers()
    self.registration = registration
    if #available(iOS 17.2, *) {
      startTokens = Task {
        if let token = Activity<PlanoActivityAttributes>.pushToStartToken {
          await register(data: token, kind: "live_start", flightId: nil, using: registration)
        }
        for await token in Activity<PlanoActivityAttributes>.pushToStartTokenUpdates {
          await register(data: token, kind: "live_start", flightId: nil, using: registration)
        }
      }
    }
    for activity in Activity<PlanoActivityAttributes>.activities { observe(activity, using: registration) }
    activities = Task {
      for await activity in Activity<PlanoActivityAttributes>.activityUpdates {
        observe(activity, using: registration)
      }
    }
  }

  private func cancelObservers() {
    startTokens?.cancel()
    activities?.cancel()
    for task in tokenTasks.values { task.cancel() }
    tokenTasks.removeAll()
  }

  func clear() async {
    cancelObservers()
    registration = nil
    LiveKeychain.clear()
    for activity in Activity<PlanoActivityAttributes>.activities {
      await activity.end(nil, dismissalPolicy: .immediate)
    }
  }

  func start(stateJSON: String) throws {
    guard let registration else { throw RegistrationError.notConfigured }
    guard ActivityAuthorizationInfo().areActivitiesEnabled else { throw RegistrationError.disabled }
    let state = try JSONDecoder().decode(PlanoActivityAttributes.ContentState.self, from: Data(stateJSON.utf8))
    guard !["arrived", "cancelled"].contains(state.status) else { throw RegistrationError.inactive }
    if Activity<PlanoActivityAttributes>.activities.contains(where: { $0.attributes.flightId == state.flightId }) { return }
    let activity = try Activity.request(
      attributes: PlanoActivityAttributes(flightId: state.flightId),
      content: ActivityContent(state: state, staleDate: .now.addingTimeInterval(600)),
      pushType: .token
    )
    observe(activity, using: registration)
  }

  func stop(flightId: String) async {
    for activity in Activity<PlanoActivityAttributes>.activities where activity.attributes.flightId == flightId {
      tokenTasks.removeValue(forKey: activity.id)?.cancel()
      await activity.end(nil, dismissalPolicy: .immediate)
    }
  }

  private func observe(_ activity: Activity<PlanoActivityAttributes>, using registration: ActivityRegistration) {
    let id = activity.id
    guard tokenTasks[id] == nil else { return }
    tokenTasks[id] = Task {
      if let token = activity.pushToken {
        await register(data: token, kind: "live_update", flightId: activity.attributes.flightId, using: registration)
      }
      for await token in activity.pushTokenUpdates {
        await register(data: token, kind: "live_update", flightId: activity.attributes.flightId, using: registration)
      }
    }
  }

  private func register(data: Data, kind: String, flightId: String?, using registration: ActivityRegistration) async {
    guard let base = registration.baseURL else { return }
    let token = data.map { String(format: "%02x", $0) }.joined()
    let requestUpdateToken: Bool?
    if #available(iOS 18.0, *), kind == "live_start" {
      requestUpdateToken = true
    } else {
      requestUpdateToken = nil
    }
    let payload = DeviceRegistration(token: token, kind: kind, flightId: flightId, requestUpdateToken: requestUpdateToken)
    guard let body = try? JSONEncoder().encode(payload) else { return }
    var request = URLRequest(url: base.appendingPathComponent("v1").appendingPathComponent("devices"), timeoutInterval: 8)
    request.httpMethod = "POST"
    request.setValue("application/json", forHTTPHeaderField: "Content-Type")
    request.setValue("Bearer \(registration.token)", forHTTPHeaderField: "Authorization")
    request.httpBody = body
    for attempt in 0..<3 {
      guard !Task.isCancelled, self.registration == registration else { return }
      if let (_, response) = try? await URLSession.shared.data(for: request),
         let http = response as? HTTPURLResponse {
        if (200..<300).contains(http.statusCode) || (400..<500).contains(http.statusCode) && http.statusCode != 429 { return }
      }
      try? await Task.sleep(nanoseconds: UInt64(1 << attempt) * 1_000_000_000)
    }
  }
}
