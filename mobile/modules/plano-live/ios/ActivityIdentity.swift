struct ActivityIdentity {
  let id: String
  let flightID: String
  let binding: String?
  let eligible: Bool

  static func retainedIDs(in candidates: [Self], binding: String, observedIDs: Set<String>) -> Set<String> {
    var retained: [String: Self] = [:]
    for candidate in candidates where candidate.eligible && candidate.binding == binding {
      if let current = retained[candidate.flightID] {
        let candidateObserved = observedIDs.contains(candidate.id)
        let currentObserved = observedIDs.contains(current.id)
        let preferred = candidateObserved == currentObserved ? candidate.id < current.id : candidateObserved
        guard preferred else { continue }
      }
      retained[candidate.flightID] = candidate
    }
    return Set(retained.values.map(\.id))
  }
}
