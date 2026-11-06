import Foundation

enum ActivityTokenSlot: Hashable {
  case start
  case update(activityID: String, flightID: String)
}

@MainActor
final class ActivityTokenUploads {
  typealias Sender = @MainActor (Data, ActivityTokenSlot) async -> Bool

  private struct Upload {
    let token: Data
    let send: Sender
    var task: Task<Void, Never>?
  }

  private var pending: [ActivityTokenSlot: Upload] = [:]
  private var settled: [ActivityTokenSlot: Data] = [:]

  func submit(_ token: Data, for slot: ActivityTokenSlot, send: @escaping Sender) {
    if settled[slot] == token { return }
    if pending[slot]?.token != token {
      remove(slot)
      pending[slot] = Upload(token: token, send: send)
    }
    start(slot)
  }

  func retryPending() {
    for slot in Array(pending.keys) { start(slot) }
  }

  func remove(_ slot: ActivityTokenSlot) {
    pending.removeValue(forKey: slot)?.task?.cancel()
    settled.removeValue(forKey: slot)
  }

  func reset() {
    for upload in pending.values { upload.task?.cancel() }
    pending.removeAll()
    settled.removeAll()
  }

  private func start(_ slot: ActivityTokenSlot) {
    guard let upload = pending[slot], upload.task == nil else { return }
    pending[slot]?.task = Task { [weak self] in
      guard !Task.isCancelled else { return }
      let finished = await upload.send(upload.token, slot)
      // Superseded attempts can finish even after cancellation. Their results
      // must not affect a later submission, even if the token rotates back.
      guard !Task.isCancelled, let self else { return }
      if finished {
        self.pending.removeValue(forKey: slot)
        self.settled[slot] = upload.token
      } else {
        self.pending[slot]?.task = nil
      }
    }
  }
}
