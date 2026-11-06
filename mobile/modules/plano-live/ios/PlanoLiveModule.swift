import ActivityKit
import CryptoKit
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

    AsyncFunction("nextIOSPushRevision") { () async throws -> String in
      try await PlanoActivityCoordinator.shared.nextPushRevision()
    }

    AsyncFunction("retireIOSPush") { (server: String, token: String) async throws -> String in
      try await PlanoActivityCoordinator.shared.retirePush(server: server, token: token)
    }

    AsyncFunction("start") { (stateJSON: String) async throws in
      try await PlanoActivityCoordinator.shared.start(stateJSON: stateJSON)
    }

    AsyncFunction("stop") { (flightId: String) async in
      await PlanoActivityCoordinator.shared.stop(flightId: flightId)
    }

    AsyncFunction("iosLiveActivityBinding") { (origin: String) async -> String? in
      await PlanoActivityCoordinator.shared.liveBinding(origin: origin)
    }

    AsyncFunction("stopLiveActivityInSession") { (origin: String, binding: String, flightId: String) async in
      await PlanoActivityCoordinator.shared.stop(flightId: flightId, origin: origin, binding: binding)
    }
  }
}

public final class PlanoLiveAppDelegateSubscriber: ExpoAppDelegateSubscriber {
  public func application(_ application: UIApplication, didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
    Task { @MainActor in PlanoActivityCoordinator.shared.restore() }
    return true
  }

  public func applicationDidBecomeActive(_ application: UIApplication) {
    Task { @MainActor in PlanoActivityCoordinator.shared.resume() }
  }
}

private struct ActivityRegistration: Codable {
  let server: String
  let token: String
  let binding: String?

  init(server: String, token: String, binding: String? = nil) {
    self.server = server
    self.token = token
    self.binding = binding
  }

  func matches(_ other: ActivityRegistration?) -> Bool {
    guard let other else { return false }
    return server == other.server && token == other.token
  }

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
  let liveSlot: String
  let liveRevision: Int64
  let binding: String
}

private struct DeviceRegistrationFailure: Decodable {
  let error: String
}

private enum RegistrationError: LocalizedError {
  case invalidServer, keychain, disabled, notConfigured, inactive, superseded

  var errorDescription: String? {
    switch self {
    case .invalidServer: return "Enter a valid Plano server address."
    case .keychain: return "Plano couldn’t save its Live Activity registration."
    case .disabled: return "Allow Live Activities for Plano in your device settings."
    case .notConfigured: return "Enable flight alerts in Plano Settings before starting a Live Activity."
    case .inactive: return "This flight has already finished."
    case .superseded: return "The Live Activity settings changed. Try again."
    }
  }
}

private final class LiveRegistrationRedirectPolicy: NSObject, URLSessionTaskDelegate {
  func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
                  newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) {
    completionHandler(nil)
  }
}

private enum LiveKeychain {
  static let query: [String: Any] = [
    kSecClass as String: kSecClassGenericPassword,
    kSecAttrService as String: "PlanoLiveRegistration",
    kSecAttrAccount as String: "active-server",
  ]
  static let revisionQuery = query.merging([kSecAttrAccount as String: "token-revision"]) { _, new in new }

  static func save(_ registration: ActivityRegistration) throws {
    let data = try JSONEncoder().encode(registration)
    try save(data, matching: query)
  }

  private static func save(_ data: Data, matching query: [String: Any],
                           accessibility: CFString = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly) throws {
    let values: [String: Any] = [
      kSecValueData as String: data,
      kSecAttrAccessible as String: accessibility,
    ]
    var status = SecItemUpdate(query as CFDictionary, values as CFDictionary)
    if status == errSecItemNotFound {
      status = SecItemAdd(query.merging(values) { _, new in new } as CFDictionary, nil)
    }
    if status != errSecSuccess { throw RegistrationError.keychain }
  }

  static func load() -> ActivityRegistration? {
    try? loadChecked()
  }

  static func loadChecked() throws -> ActivityRegistration? {
    guard let data = try read(matching: query) else { return nil }
    return try JSONDecoder().decode(ActivityRegistration.self, from: data)
  }

  static func nextTokenRevision() throws -> Int64 {
    let current: Int64
    if let data = try read(matching: revisionQuery) { current = try JSONDecoder().decode(Int64.self, from: data) }
    else { current = 0 }
    guard current >= 0, current < Int64.max else { throw RegistrationError.keychain }
    let next = current + 1
    // The counter may migrate with the installation credential. It contains
    // no token and remains readable while locked after the first unlock.
    try save(JSONEncoder().encode(next), matching: revisionQuery, accessibility: kSecAttrAccessibleAfterFirstUnlock)
    return next
  }

  private static func read(matching query: [String: Any]) throws -> Data? {
    let readQuery = query.merging([kSecReturnData as String: true, kSecMatchLimit as String: kSecMatchLimitOne]) { _, new in new }
    var item: CFTypeRef?
    let status = SecItemCopyMatching(readQuery as CFDictionary, &item)
    if status == errSecItemNotFound { return nil }
    guard status == errSecSuccess, let data = item as? Data else { throw RegistrationError.keychain }
    return data
  }

  static func clear() throws {
    let status = SecItemDelete(query as CFDictionary)
    guard status == errSecSuccess || status == errSecItemNotFound else { throw RegistrationError.keychain }
  }
}

@MainActor
private final class PlanoActivityCoordinator {
  static let shared = PlanoActivityCoordinator()
  private struct ActivityObservers {
    let activity: Activity<PlanoActivityAttributes>
    let tokens: Task<Void, Never>
    let states: Task<Void, Never>
  }

  private var registration: ActivityRegistration?
  private var configuring: ActivityRegistration?
  private var mayRestore = true
  private var generation = UUID()
  private var startTokens: Task<Void, Never>?
  private var activities: Task<Void, Never>?
  private var observers: [String: ActivityObservers] = [:]
  private var locallyEnding: [String: Activity<PlanoActivityAttributes>] = [:]
  private var retirement: Task<Void, Never>?
  private let uploads = ActivityTokenUploads()
  private let transport: URLSession = {
    let configuration = URLSessionConfiguration.ephemeral
    configuration.timeoutIntervalForResource = 12
    return URLSession(configuration: configuration, delegate: LiveRegistrationRedirectPolicy(), delegateQueue: nil)
  }()

  func restore() {
    guard mayRestore, registration == nil, retirement == nil, let saved = LiveKeychain.load(),
          saved.baseURL != nil, saved.binding != nil else { return }
    install(saved)
  }

  func resume() {
    restore()
    guard let registration else { return }
    reconcileActivities(using: registration, generation: generation)
    uploads.retryPending()
  }

  func configure(server: String, token: String) async throws {
    let target = ActivityRegistration(server: server, token: token)
    guard target.baseURL != nil, !token.isEmpty else { throw RegistrationError.invalidServer }
    if target.matches(registration) {
      resume()
      return
    }
    if mayRestore, registration == nil, retirement == nil, let saved = LiveKeychain.load(),
       target.matches(saved), saved.binding != nil {
      install(saved)
      return
    }

    let next = ActivityRegistration(server: server, token: token, binding: UUID().uuidString.lowercased())
    let oldActivities = knownActivities()
    let change = invalidateRegistration()
    try? LiveKeychain.clear()
    configuring = next
    await retireActivities(oldActivities)
    if target.matches(registration) { return }
    guard change == generation else { throw RegistrationError.superseded }
    configuring = nil
    try LiveKeychain.save(next)
    install(next)
  }

  private func install(_ registration: ActivityRegistration) {
    cancelObservers()
    generation = UUID()
    let current = generation
    self.registration = registration
    mayRestore = true
    if #available(iOS 17.2, *) {
      startTokens = Task {
        if let token = Activity<PlanoActivityAttributes>.pushToStartToken {
          submit(token, for: .start, using: registration, generation: current)
        }
        for await token in Activity<PlanoActivityAttributes>.pushToStartTokenUpdates {
          guard isCurrent(current) else { break }
          submit(token, for: .start, using: registration, generation: current)
        }
      }
    }
    reconcileActivities(using: registration, generation: current)
    activities = Task {
      for await activity in Activity<PlanoActivityAttributes>.activityUpdates {
        guard isCurrent(current) else { break }
        reconcileActivities(including: activity, using: registration, generation: current)
      }
    }
  }

  private func cancelObservers() {
    startTokens?.cancel()
    activities?.cancel()
    startTokens = nil
    activities = nil
    for observer in observers.values {
      observer.tokens.cancel()
      observer.states.cancel()
    }
    observers.removeAll()
    uploads.reset()
  }

  private func invalidateRegistration() -> UUID {
    cancelObservers()
    generation = UUID()
    registration = nil
    configuring = nil
    mayRestore = false
    return generation
  }

  private func retireActivities(_ oldActivities: [Activity<PlanoActivityAttributes>]) async {
    if let retirement {
      await retirement.value
      return
    }
    guard !oldActivities.isEmpty else { return }
    let ending = Task {
      for activity in oldActivities { await activity.end(nil, dismissalPolicy: .immediate) }
    }
    retirement = ending
    await ending.value
    retirement = nil
  }

  func clear() async {
    let oldActivities = knownActivities()
    _ = invalidateRegistration()
    try? LiveKeychain.clear()
    await retireActivities(oldActivities)
  }

  func nextPushRevision() throws -> String {
    String(try LiveKeychain.nextTokenRevision())
  }

  func retirePush(server: String, token: String) async throws -> String {
    let target = ActivityRegistration(server: server, token: token)
    guard !server.isEmpty, !token.isEmpty else { throw RegistrationError.invalidServer }

    let saved = Result { try LiveKeychain.loadChecked() }
    let ownsActivities = target.matches(registration) || target.matches(configuring) ||
      (registration == nil && configuring == nil && target.matches(try? saved.get()))
    let oldActivities = ownsActivities ? knownActivities() : []
    if ownsActivities { _ = invalidateRegistration() }

    // Allocate the floor in the same actor turn that stops matching uploads.
    // ActivityKit teardown can suspend and let a later enable start work.
    let floor = Result {
      if target.matches(try saved.get()) { try LiveKeychain.clear() }
      return try nextPushRevision()
    }
    if ownsActivities { await retireActivities(oldActivities) }
    return try floor.get()
  }

  func start(stateJSON: String) throws {
    guard let registration, let binding = registration.binding else { throw RegistrationError.notConfigured }
    guard ActivityAuthorizationInfo().areActivitiesEnabled else { throw RegistrationError.disabled }
    let state = try JSONDecoder().decode(PlanoActivityAttributes.ContentState.self, from: Data(stateJSON.utf8))
    guard !["arrived", "cancelled"].contains(state.status) else { throw RegistrationError.inactive }
    if knownActivities().contains(where: {
      $0.attributes.flightId == state.flightId && $0.attributes.activityBinding == binding && isLive($0)
    }) {
      reconcileActivities(using: registration, generation: generation)
      return
    }
    let now = Date.now
    let observedAt = Date(timeIntervalSince1970: TimeInterval(state.observedEpoch))
    let staleDate = state.observedEpoch > 0 ? min(observedAt, now).addingTimeInterval(600) : now
    let activity = try Activity.request(
      attributes: PlanoActivityAttributes(flightId: state.flightId, activityBinding: binding),
      content: ActivityContent(state: state, staleDate: staleDate),
      pushType: .token
    )
    reconcileActivities(including: activity, using: registration, generation: generation)
  }

  func stop(flightId: String) async {
    await stopActivities { $0.attributes.flightId == flightId }
  }

  func liveBinding(origin: String) -> String? {
    guard registration?.server == origin else { return nil }
    return registration?.binding
  }

  func stop(flightId: String, origin: String, binding: String) async {
    guard liveBinding(origin: origin) == binding else { return }
    await stopActivities {
      $0.attributes.flightId == flightId && $0.attributes.activityBinding == binding
    }
  }

  private func stopActivities(where matches: (Activity<PlanoActivityAttributes>) -> Bool) async {
    let endings = knownActivities().filter(matches).compactMap { beginEnding($0) }
    for ending in endings { await ending.value }
  }

  private func endActivity(_ activity: Activity<PlanoActivityAttributes>) async {
    if let ending = beginEnding(activity) { await ending.value }
  }

  private func beginEnding(_ activity: Activity<PlanoActivityAttributes>, generation: UUID? = nil) -> Task<Void, Never>? {
    guard locallyEnding[activity.id] == nil else { return nil }
    locallyEnding[activity.id] = activity
    removeObservation(for: activity)
    // Removing the observation can cancel the upload task that requested dismissal.
    return Task {
      defer { locallyEnding[activity.id] = nil }
      if let generation, !isCurrent(generation) { return }
      await activity.end(nil, dismissalPolicy: .immediate)
    }
  }

  private func isCurrent(_ current: UUID) -> Bool {
    !Task.isCancelled && generation == current
  }

  private func isLive(_ activity: Activity<PlanoActivityAttributes>) -> Bool {
    locallyEnding[activity.id] == nil && activity.activityState != .ended && activity.activityState != .dismissed
  }

  private func knownActivities(including incoming: Activity<PlanoActivityAttributes>? = nil) -> [Activity<PlanoActivityAttributes>] {
    var candidates = Activity<PlanoActivityAttributes>.activities
    var knownIDs = Set(candidates.map(\.id))
    // A requested or streamed activity can lead ActivityKit's static snapshot.
    for observer in observers.values where knownIDs.insert(observer.activity.id).inserted {
      candidates.append(observer.activity)
    }
    // Retirement still owns handles removed from observation during dismissal.
    for activity in locallyEnding.values where knownIDs.insert(activity.id).inserted {
      candidates.append(activity)
    }
    if let incoming, knownIDs.insert(incoming.id).inserted {
      candidates.append(incoming)
    }
    return candidates
  }

  private func reconcileActivities(including incoming: Activity<PlanoActivityAttributes>? = nil,
                                   using registration: ActivityRegistration, generation: UUID) {
    guard isCurrent(generation), let binding = registration.binding else { return }
    let candidates = knownActivities(including: incoming)
    let identities = candidates.map {
      ActivityIdentity(id: $0.id, flightID: $0.attributes.flightId, binding: $0.attributes.activityBinding, eligible: isLive($0))
    }
    let retained = ActivityIdentity.retainedIDs(in: identities, binding: binding, observedIDs: Set(observers.keys))
    for activity in candidates where isLive(activity) {
      if retained.contains(activity.id) {
        observe(activity, using: registration, generation: generation)
      } else {
        // Duplicates and retired bindings must stop uploading before dismissal.
        _ = beginEnding(activity, generation: generation)
      }
    }
  }

  private func observe(_ activity: Activity<PlanoActivityAttributes>, using registration: ActivityRegistration, generation: UUID) {
    let id = activity.id
    guard isCurrent(generation), observers[id] == nil, isLive(activity) else { return }
    let slot = ActivityTokenSlot.update(activityID: id, flightID: activity.attributes.flightId)
    let tokens = Task {
      if let token = activity.pushToken {
        submit(token, for: slot, using: registration, generation: generation)
      }
      for await token in activity.pushTokenUpdates {
        guard isCurrent(generation) else { break }
        submit(token, for: slot, using: registration, generation: generation)
      }
    }
    let states = Task {
      if activity.activityState == .ended || activity.activityState == .dismissed {
        if isCurrent(generation) { removeObservation(for: activity) }
        return
      }
      for await state in activity.activityStateUpdates {
        guard isCurrent(generation) else { break }
        if state == .ended || state == .dismissed {
          removeObservation(for: activity)
          break
        }
      }
    }
    observers[id] = ActivityObservers(activity: activity, tokens: tokens, states: states)
  }

  private func removeObservation(for activity: Activity<PlanoActivityAttributes>) {
    let observer = observers.removeValue(forKey: activity.id)
    observer?.tokens.cancel()
    observer?.states.cancel()
    uploads.remove(.update(activityID: activity.id, flightID: activity.attributes.flightId))
  }

  private func submit(_ token: Data, for slot: ActivityTokenSlot, using registration: ActivityRegistration, generation: UUID) {
    guard isCurrent(generation), !token.isEmpty else { return }
    // Each queued value keeps one revision across retries. Persist the counter
    // before sending so delayed requests can be ordered even after relaunch.
    var revision: Int64?
    uploads.submit(token, for: slot) { [weak self] token, slot in
      guard let self, self.isCurrent(generation) else { return true }
      if revision == nil { revision = try? LiveKeychain.nextTokenRevision() }
      guard let revision else { return false }
      return await self.register(data: token, slot: slot, revision: revision, using: registration, generation: generation)
    }
  }

  private func register(data: Data, slot: ActivityTokenSlot, revision: Int64, using registration: ActivityRegistration, generation: UUID) async -> Bool {
    guard let base = registration.baseURL, let binding = registration.binding else { return true }
    let token = data.map { String(format: "%02x", $0) }.joined()
    let payload: DeviceRegistration
    switch slot {
    case .start:
      let requestUpdateToken: Bool?
      if #available(iOS 18.0, *) { requestUpdateToken = true }
      else { requestUpdateToken = nil }
      payload = DeviceRegistration(token: token, kind: "live_start", flightId: nil, requestUpdateToken: requestUpdateToken,
                                   liveSlot: "start", liveRevision: revision, binding: binding)
    case let .update(activityID, flightID):
      let digest = SHA256.hash(data: Data(activityID.utf8)).map { String(format: "%02x", $0) }.joined()
      payload = DeviceRegistration(token: token, kind: "live_update", flightId: flightID, requestUpdateToken: nil,
                                   liveSlot: "activity:\(digest)", liveRevision: revision, binding: binding)
    }
    guard let body = try? JSONEncoder().encode(payload) else { return true }
    var request = URLRequest(url: base.appendingPathComponent("v1").appendingPathComponent("devices"), timeoutInterval: 8)
    request.httpMethod = "POST"
    request.setValue("application/json", forHTTPHeaderField: "Content-Type")
    request.setValue("Bearer \(registration.token)", forHTTPHeaderField: "Authorization")
    request.httpBody = body
    for attempt in 0..<3 {
      guard isCurrent(generation) else { return true }
      if let (data, response) = try? await transport.data(for: request),
         let http = response as? HTTPURLResponse {
        // Both a registration change and a replacement token cancel old uploads.
        guard isCurrent(generation) else { return true }
        if http.statusCode == 404, http.mimeType == "application/json",
           (try? JSONDecoder().decode(DeviceRegistrationFailure.self, from: data))?.error == "record not found",
           case let .update(activityID, flightID) = slot,
           let activity = Activity<PlanoActivityAttributes>.activities.first(where: {
             $0.id == activityID && $0.attributes.flightId == flightID && $0.attributes.activityBinding == binding
           }) {
          await endActivity(activity)
        }
        if (200..<500).contains(http.statusCode) && http.statusCode != 408 && http.statusCode != 429 { return true }
      }
      if attempt < 2 {
        do { try await Task.sleep(nanoseconds: UInt64(1 << attempt) * 1_000_000_000) }
        catch { return true }
      }
    }
    return false
  }
}
