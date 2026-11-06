package api

import "fmt"

const seaShuttleFallback = "If a train is unavailable, follow airport signs and staff directions for shuttle buses."

// seaConnectionDirections describes the secure-side network published by the
// Port of Seattle. The route carries no walking or train time estimate.
func seaConnectionDirections(arrivalGate, departureGate string) []string {
	arrival, departure := gateConcourse(arrivalGate, "ABCDNS"), gateConcourse(departureGate, "ABCDNS")
	if arrival == "" || departure == "" {
		return nil
	}
	from, to := arrival[0], departure[0]
	if from == to {
		return []string{fmt.Sprintf("Follow signs within the %c gates.", to)}
	}
	fromSatellite, toSatellite := from == 'N' || from == 'S', to == 'N' || to == 'S'
	if !fromSatellite && !toSatellite {
		return []string{seaWalkStep(to)}
	}
	fromStation, fromLine := seaLoop(from)
	toStation, toLine := seaLoop(to)
	if fromStation == toStation {
		return []string{seaTrainStep(fromLine, from, to), seaShuttleFallback}
	}
	var steps []string
	if fromSatellite {
		steps = append(steps, seaTrainStep(fromLine, from, fromStation))
	} else if from != fromStation {
		steps = append(steps, seaWalkStep(fromStation))
	}
	steps = append(steps, seaTrainStep("Yellow", fromStation, toStation))
	if toSatellite {
		steps = append(steps, seaTrainStep(toLine, toStation, to))
	} else if to != toStation {
		steps = append(steps, seaWalkStep(to))
	}
	return append(steps, seaShuttleFallback)
}

func seaLoop(concourse byte) (station byte, line string) {
	switch concourse {
	case 'A', 'B', 'S':
		return 'A', "Blue"
	default:
		return 'D', "Green"
	}
}

func seaTrainStep(line string, from, to byte) string {
	return fmt.Sprintf("Take the SEA Underground %s Line from the %c gates to the %c gates.", line, from, to)
}

func seaWalkStep(to byte) string {
	return fmt.Sprintf("Follow the walkways and signs to the %c gates.", to)
}
