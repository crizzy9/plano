@main
struct ActivityIdentityFixture {
  static func main() {
    let binding = "current-session"
    let existing = ActivityIdentity(id: "z-existing", flightID: "UA123", binding: binding, eligible: true)
    let delayed = ActivityIdentity(id: "a-delayed", flightID: "UA123", binding: binding, eligible: true)
    let secondFlight = ActivityIdentity(id: "second", flightID: "AA456", binding: binding, eligible: true)
    let oldSession = ActivityIdentity(id: "old", flightID: "UA123", binding: "old-session", eligible: true)
    let unbound = ActivityIdentity(id: "unbound", flightID: "UA123", binding: nil, eligible: true)
    let ending = ActivityIdentity(id: "ending", flightID: "UA123", binding: binding, eligible: false)
    let candidates = [existing, oldSession, ending, secondFlight, unbound, delayed]

    let observed = Set([existing.id, oldSession.id, ending.id])
    precondition(ActivityIdentity.retainedIDs(in: candidates, binding: binding, observedIDs: observed) ==
      Set([existing.id, secondFlight.id]), "An eligible observed activity should survive a delayed duplicate.")
    precondition(ActivityIdentity.retainedIDs(in: Array(candidates.reversed()), binding: binding, observedIDs: []) ==
      Set([delayed.id, secondFlight.id]), "Restoration should choose the same survivor regardless of snapshot order.")
    precondition(ActivityIdentity.retainedIDs(in: candidates, binding: binding, observedIDs: [existing.id, delayed.id]) ==
      Set([delayed.id, secondFlight.id]), "Multiple already-observed activities should resolve deterministically.")
    precondition(ActivityIdentity.retainedIDs(in: [oldSession, unbound, ending], binding: binding, observedIDs: observed).isEmpty,
      "Retired, unbound, and ending activities should not survive.")
  }
}
