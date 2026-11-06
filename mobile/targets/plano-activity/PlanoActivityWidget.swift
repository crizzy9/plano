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
          Text(context.state.displayedStatus(stale: context.isStale))
            .font(.subheadline.weight(.medium))
        }
        HStack {
          AirportGate(code: context.state.origin, gate: context.state.departureGate,
                      terminal: context.state.departureTerminal)
          Spacer()
          FlightTimer(state: context.state, stale: context.isStale)
          Spacer()
          AirportGate(code: context.state.destination, gate: context.state.arrivalGateForDisplay,
                      terminal: context.state.arrivalTerminalForDisplay, planned: context.state.isDiverted)
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
                      terminal: context.state.arrivalTerminalForDisplay, planned: context.state.isDiverted)
        }
        DynamicIslandExpandedRegion(.center) {
          FlightTimer(state: context.state, stale: context.isStale)
        }
        DynamicIslandExpandedRegion(.bottom) {
          VStack(spacing: 6) {
            HStack {
              Text(context.state.displayedStatus(stale: context.isStale)).font(.caption.weight(.medium))
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
        MinimalFlightProgress(state: context.state, stale: context.isStale)
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

private let maximumFlightInterval: TimeInterval = 48 * 60 * 60

private struct CompactFlightTiming: View {
  let state: PlanoActivityAttributes.ContentState
  let stale: Bool

  var body: some View {
    let now = Date.now
    Group {
      if stale || state.isDiverted {
        FlightPhaseIcon(state: state, stale: stale)
      } else if let end = state.countdownEnd(at: now, stale: false) {
        let countdown = Text(timerInterval: now...end, pauseTime: end, countsDown: true)
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

private struct MinimalFlightProgress: View {
  let state: PlanoActivityAttributes.ContentState
  let stale: Bool

  var body: some View {
    if !stale, let interval = state.arrivalProgress(at: .now) {
      ProgressView(timerInterval: interval, countsDown: false)
        .progressViewStyle(.circular)
        .labelsHidden()
        .tint(.mint)
        .accessibilityLabel("Estimated flight progress")
    } else {
      FlightPhaseIcon(state: state, stale: stale)
    }
  }
}

private struct FlightPhaseIcon: View {
  let state: PlanoActivityAttributes.ContentState
  let stale: Bool

  var body: some View {
    let awaitingUpdate = stale && !state.isDiverted
    Image(systemName: awaitingUpdate ? "clock" : state.phaseIcon)
      .foregroundStyle(.mint)
      .accessibilityLabel(awaitingUpdate ? Text("Plano flight awaiting update") : Text("Plano flight, \(state.displayedStatus(stale: stale))"))
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
  var planned = false

  var body: some View {
    VStack(alignment: .leading, spacing: 2) {
      Text(code).font(.title2.bold()).lineLimit(1).minimumScaleFactor(0.75)
      if planned {
        Text("Planned").font(.caption2).accessibilityLabel("Planned destination")
      }
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
    if let end = state.countdownEnd(at: now, stale: stale) {
      VStack(spacing: 2) {
        Text(stale ? "Last arrival estimate" : state.countdownLabel).font(.caption2)
        Text(timerInterval: now...end, pauseTime: end, countsDown: true)
          .font(.callout.monospacedDigit().weight(.semibold))
      }
      .multilineTextAlignment(.center)
    } else {
      Text(stale ? "Awaiting update" : state.statusLabel).font(.caption)
    }
  }
}

private struct FlightProgress: View {
  let state: PlanoActivityAttributes.ContentState
  let stale: Bool

  var body: some View {
    if let interval = state.arrivalProgress(at: .now) {
      ProgressView(timerInterval: interval, countsDown: false)
        .labelsHidden().tint(.mint)
        .accessibilityLabel(stale ? Text("Last estimated flight progress") : Text("Estimated flight progress"))
    } else if !stale, !state.hasPushedBack, state.status != "cancelled" {
      ProgressView(value: ["landed", "arrived"].contains(state.status) ? 1 : 0)
        .tint(.mint).accessibilityLabel("Flight progress")
    }
  }
}

private extension PlanoActivityAttributes.ContentState {
  var isDiverted: Bool { status == "diverted" }
  var arrivalGateForDisplay: String? { isDiverted ? nil : arrivalGate }
  var arrivalTerminalForDisplay: String? { isDiverted ? nil : arrivalTerminal }
  var hasPushedBack: Bool { ["taxi_out", "airborne", "diverted"].contains(status) }
  var isArrivalPhase: Bool { hasPushedBack || ["landed", "arrived"].contains(status) }
  var countdownLabel: String { hasPushedBack ? "Arrival estimate" : "Until departure" }

  var phaseIcon: String {
    switch status {
    case "cancelled": return "xmark.circle"
    case "landed", "arrived": return "airplane.arrival"
    case "airborne": return "airplane"
    case "diverted": return "exclamationmark.triangle"
    default: return "airplane.departure"
    }
  }

  func countdownEnd(at now: Date, stale: Bool) -> Date? {
    if hasPushedBack {
      guard showsArrivalEstimate else { return nil }
      if departureEpoch > 0 { return arrivalProgress(at: now)?.upperBound }
      // A valid ETA can be available before the provider reports a start time.
      guard arrivalEpoch > 0 else { return nil }
      let arrival = Date(timeIntervalSince1970: Double(arrivalEpoch))
      return arrival > now && arrival.timeIntervalSince(now) <= maximumFlightInterval ? arrival : nil
    }
    guard !stale, !["landed", "arrived", "cancelled"].contains(status), departureEpoch > 0 else { return nil }
    let departure = Date(timeIntervalSince1970: Double(departureEpoch))
    return departure > now ? departure : nil
  }

  func arrivalProgress(at now: Date) -> ClosedRange<Date>? {
    guard showsArrivalEstimate, departureEpoch > 0, arrivalEpoch > departureEpoch else { return nil }
    let departure = Date(timeIntervalSince1970: Double(departureEpoch))
    let arrival = Date(timeIntervalSince1970: Double(arrivalEpoch))
    guard departure <= now, arrival > now, arrival.timeIntervalSince(departure) <= maximumFlightInterval else { return nil }
    return departure...arrival
  }

  private var showsArrivalEstimate: Bool {
    // A diversion can carry the original destination's scheduled arrival.
    hasPushedBack && !isDiverted
  }

  func displayedStatus(stale: Bool) -> String {
    stale ? "Last status · \(statusLabel)" : statusLabel
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
