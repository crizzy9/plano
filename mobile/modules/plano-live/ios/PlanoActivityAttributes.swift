import ActivityKit
import Foundation

struct PlanoActivityAttributes: ActivityAttributes {
  let flightId: String
  let activityBinding: String?

  struct ContentState: Codable, Hashable {
    let flightId: String
    let status: String
    let origin: String
    let destination: String
    let flightNumber: String?
    let departureGate: String?
    let arrivalGate: String?
    let departureTerminal: String?
    let arrivalTerminal: String?
    let departureEpoch: Int64
    let arrivalEpoch: Int64
    let observedEpoch: Int64
    let seat: String?
  }
}
