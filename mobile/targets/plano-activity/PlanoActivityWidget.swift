import ActivityKit
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
          AirportGate(code: context.state.destination, gate: context.state.arrivalGate,
                      terminal: context.state.arrivalTerminal)
        }
        FlightProgress(state: context.state)
        if let seat = context.state.seat, !seat.isEmpty {
          Text("Seat \(seat)").font(.caption)
        }
      }
      .padding(16)
      .activityBackgroundTint(Color(red: 0.07, green: 0.20, blue: 0.17))
      .activitySystemActionForegroundColor(.white)
      .foregroundStyle(.white)
    } dynamicIsland: { context in
      DynamicIsland {
        DynamicIslandExpandedRegion(.leading) {
          AirportGate(code: context.state.origin, gate: context.state.departureGate,
                      terminal: context.state.departureTerminal)
        }
        DynamicIslandExpandedRegion(.trailing) {
          AirportGate(code: context.state.destination, gate: context.state.arrivalGate,
                      terminal: context.state.arrivalTerminal)
        }
        DynamicIslandExpandedRegion(.center) {
          FlightTimer(state: context.state, stale: context.isStale)
        }
        DynamicIslandExpandedRegion(.bottom) {
          FlightProgress(state: context.state)
        }
      } compactLeading: {
        Image(systemName: "airplane").foregroundStyle(.mint)
          .accessibilityLabel("Plano flight")
      } compactTrailing: {
        Text(context.state.destination).font(.caption.weight(.semibold))
      } minimal: {
        Image(systemName: "airplane").foregroundStyle(.mint)
          .accessibilityLabel("Plano flight")
      }
      .keylineTint(.mint)
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
        Text(state.isInFlight ? "Until arrival" : "Until departure").font(.caption2)
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

  var body: some View {
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

private extension PlanoActivityAttributes.ContentState {
  var isInFlight: Bool { ["airborne", "diverted"].contains(status) }

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
