import ActivityKit
import Foundation
import SwiftUI
import WidgetKit

@main
struct PlanoActivityWidget: Widget {
  var body: some WidgetConfiguration {
    ActivityConfiguration(for: PlanoActivityAttributes.self) { context in
      VStack(alignment: .leading, spacing: 9) {
        HStack {
          Label(context.state.flightNumber ?? "Plano", systemImage: "airplane")
            .font(.headline)
          Spacer()
          Text(context.state.statusLabel)
            .font(.subheadline.weight(.medium))
        }
        HStack {
          AirportGate(code: context.state.origin, gate: context.state.departureGate,
                      terminal: context.state.departureTerminal)
          Spacer()
          FlightTimer(state: context.state, stale: context.isStale)
          Spacer()
          AirportGate(code: context.state.destination, gate: context.state.arrivalGateForDisplay,
                      terminal: context.state.arrivalTerminalForDisplay)
        }
        FlightProgress(state: context.state, stale: context.isStale)
        FlightSeat(seat: context.state.seat)
      }
      .padding(16)
      .activityBackgroundTint(Color(red: 0.07, green: 0.20, blue: 0.17))
      .activitySystemActionForegroundColor(.white)
      .foregroundStyle(.white)
      .widgetURL(flightDestination(context.attributes.flightId))
    } dynamicIsland: { context in
      DynamicIsland {
        DynamicIslandExpandedRegion(.leading) {
          AirportGate(code: context.state.origin, gate: context.state.departureGate,
                      terminal: context.state.departureTerminal)
        }
        DynamicIslandExpandedRegion(.trailing) {
          AirportGate(code: context.state.destination, gate: context.state.arrivalGateForDisplay,
                      terminal: context.state.arrivalTerminalForDisplay)
        }
        DynamicIslandExpandedRegion(.center) {
          FlightTimer(state: context.state, stale: context.isStale)
        }
        DynamicIslandExpandedRegion(.bottom) {
          VStack(spacing: 6) {
            HStack {
              Text(context.state.statusLabel).font(.caption.weight(.medium))
              Spacer()
              FlightSeat(seat: context.state.seat)
            }
            FlightProgress(state: context.state, stale: context.isStale)
          }
        }
      } compactLeading: {
        Image(systemName: "airplane").foregroundStyle(.mint)
          .accessibilityLabel("Plano flight")
      } compactTrailing: {
        CompactFlightTiming(state: context.state, stale: context.isStale)
      } minimal: {
        FlightPhaseIcon(state: context.state, stale: context.isStale)
      }
      .keylineTint(.mint)
      .widgetURL(flightDestination(context.attributes.flightId))
    }
  }
}

private func flightDestination(_ flightId: String) -> URL? {
  guard flightId != ".", flightId != "..",
        flightId.range(of: #"\A[A-Za-z0-9_.-]{1,128}\z"#, options: .regularExpression) != nil else { return nil }
  return URL(string: "plano://notification/flight/\(flightId)")
}

private struct CompactFlightTiming: View {
  let state: PlanoActivityAttributes.ContentState
  let stale: Bool

  var body: some View {
    let now = Date.now
    Group {
      if stale {
        FlightPhaseIcon(state: state, stale: true)
      } else if let end = state.countdownEnd, end > now {
        let countdown = Text(timerInterval: now...end, countsDown: true)
        ViewThatFits(in: .horizontal) {
          countdown
            .monospacedDigit()
            .lineLimit(1)
            .accessibilityLabel(Text("\(Text(state.countdownLabel)) \(countdown)"))
          FlightPhaseIcon(state: state, stale: false)
        }
      } else {
        let airport = state.isArrivalPhase ? state.destination : state.origin
        if airport.isEmpty {
          FlightPhaseIcon(state: state, stale: false)
        } else {
          Text(airport).lineLimit(1).minimumScaleFactor(0.8)
        }
      }
    }
    .font(.caption2.weight(.semibold))
    .multilineTextAlignment(.trailing)
    .frame(maxWidth: 56, alignment: .trailing)
  }
}

private struct FlightPhaseIcon: View {
  let state: PlanoActivityAttributes.ContentState
  let stale: Bool

  var body: some View {
    Image(systemName: stale ? "clock" : state.phaseIcon)
      .foregroundStyle(.mint)
      .accessibilityLabel(stale ? Text("Plano flight awaiting update") : Text("Plano flight, \(state.statusLabel)"))
  }
}

private struct FlightSeat: View {
  let seat: String?

  var body: some View {
    if let seat, !seat.isEmpty {
      Text("Seat \(seat)").font(.caption).privacySensitive()
    }
  }
}

private struct AirportGate: View {
  let code: String
  let gate: String?
  let terminal: String?

  var body: some View {
    VStack(alignment: .leading, spacing: 2) {
      Text(code).font(.title2.bold()).lineLimit(1).minimumScaleFactor(0.75)
      if let gate, !gate.isEmpty {
        Text("Gate \(gate)").font(.caption)
      }
      if let terminal, !terminal.isEmpty {
        Text("Terminal \(terminal)").font(.caption2)
      }
    }
  }
}

private struct FlightTimer: View {
  let state: PlanoActivityAttributes.ContentState
  let stale: Bool

  var body: some View {
    let now = Date.now
    if stale {
      Text("Awaiting update").font(.caption)
    } else if let end = state.countdownEnd, end > now {
      VStack(spacing: 2) {
        Text(state.countdownLabel).font(.caption2)
        Text(timerInterval: now...end, countsDown: true)
          .font(.callout.monospacedDigit().weight(.semibold))
          .multilineTextAlignment(.center)
      }
    } else {
      Text(state.statusLabel).font(.caption)
    }
  }
}

private struct FlightProgress: View {
  let state: PlanoActivityAttributes.ContentState
  let stale: Bool

  var body: some View {
    if !stale {
      if state.isInFlight, state.departureEpoch > 0, state.arrivalEpoch > state.departureEpoch {
        ProgressView(timerInterval: Date(timeIntervalSince1970: Double(state.departureEpoch))...Date(timeIntervalSince1970: Double(state.arrivalEpoch)),
                     countsDown: false)
          .labelsHidden().tint(.mint)
          .accessibilityLabel("Estimated flight progress")
      } else {
        ProgressView(value: ["landed", "arrived"].contains(state.status) ? 1 : 0)
          .tint(.mint).accessibilityLabel("Flight progress")
      }
    }
  }
}

private extension PlanoActivityAttributes.ContentState {
  var arrivalGateForDisplay: String? { status == "diverted" ? nil : arrivalGate }
  var arrivalTerminalForDisplay: String? { status == "diverted" ? nil : arrivalTerminal }
  var isInFlight: Bool { ["airborne", "diverted"].contains(status) }
  var isArrivalPhase: Bool { isInFlight || ["landed", "arrived"].contains(status) }
  var countdownLabel: String { isInFlight ? "Until arrival" : "Until departure" }

  var phaseIcon: String {
    switch status {
    case "cancelled": return "xmark.circle"
    case "landed", "arrived": return "airplane.arrival"
    case "airborne", "diverted": return "airplane"
    default: return "airplane.departure"
    }
  }

  var countdownEnd: Date? {
    if ["landed", "arrived", "cancelled"].contains(status) { return nil }
    let epoch = isInFlight ? arrivalEpoch : departureEpoch
    return epoch > 0 ? Date(timeIntervalSince1970: Double(epoch)) : nil
  }

  var statusLabel: String {
    switch status {
    case "boarding": return "Boarding"
    case "taxi_out": return "Taxiing"
    case "airborne": return "In flight"
    case "landed": return "Landed"
    case "arrived": return "At the gate"
    case "cancelled": return "Cancelled"
    case "diverted": return "Diverted"
    default: return "Scheduled"
    }
  }
}
