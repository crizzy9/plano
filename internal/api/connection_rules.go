package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const maxConnectionRulesBytes = 1 << 20

var (
	connectionAirportCode  = regexp.MustCompile(`^[A-Z0-9]{3,4}$`)
	connectionCheckpointID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	connectionLocationCode = regexp.MustCompile(`^[A-Z0-9][A-Z0-9 ._-]{0,31}$`)
)

// ConnectionRules contains immutable operator-provided airport transfer graphs.
type ConnectionRules struct {
	byCode map[string]*connectionGraph
}

// ConnectionRoute is a configured path and its level of endpoint precision.
type ConnectionRoute struct {
	TransferMinutes int
	Instructions    []string
	MapURL          string
	Precision       string
}

type connectionAirport struct {
	Codes         []string               `json:"codes"`
	MapURL        string                 `json:"mapUrl,omitempty"`
	BufferMinutes int                    `json:"bufferMinutes,omitempty"`
	Nodes         []connectionCheckpoint `json:"nodes,omitempty"`
	Edges         []connectionEdge       `json:"edges,omitempty"`
}

type connectionCheckpoint struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Terminal string `json:"terminal,omitempty"`
	Gate     string `json:"gate,omitempty"`
}

type connectionEdge struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Minutes     int    `json:"minutes"`
	Instruction string `json:"instruction,omitempty"`
}

type connectionArc struct {
	to          int
	minutes     int
	instruction string
}

type connectionGraph struct {
	mapURL        string
	bufferMinutes int
	nodes         []connectionCheckpoint
	arcs          [][]connectionArc
	terminals     map[string]int
	gates         map[string]map[string]int
}

// LoadConnectionRules validates and compiles one versioned JSON configuration.
func LoadConnectionRules(r io.Reader) (*ConnectionRules, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxConnectionRulesBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read airport transfer rules: %w", err)
	}
	if len(raw) > maxConnectionRulesBytes {
		return nil, errors.New("airport transfer rules exceed 1 MiB")
	}
	var input struct {
		Version  int                 `json:"version"`
		Airports []connectionAirport `json:"airports"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return nil, fmt.Errorf("decode airport transfer rules: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("airport transfer rules must contain one JSON document")
	}
	if input.Version != 1 || input.Airports == nil || len(input.Airports) > 256 {
		return nil, errors.New("airport transfer rules require version 1 and at most 256 airports")
	}

	rules := &ConnectionRules{byCode: make(map[string]*connectionGraph)}
	for _, airport := range input.Airports {
		if len(airport.Codes) == 0 || len(airport.Codes) > 8 {
			return nil, errors.New("airport transfer rules require one to eight codes per airport")
		}
		graph, err := compileConnectionGraph(airport)
		if err != nil {
			return nil, fmt.Errorf("airport transfer graph for %q: %w", airport.Codes[0], err)
		}
		for _, code := range airport.Codes {
			code = connectionCode(code)
			if !connectionAirportCode.MatchString(code) || rules.byCode[code] != nil {
				return nil, fmt.Errorf("invalid or duplicate airport code %q", code)
			}
			rules.byCode[code] = graph
		}
	}
	return rules, nil
}

func compileConnectionGraph(airport connectionAirport) (*connectionGraph, error) {
	if len(airport.Nodes) > 256 || len(airport.Edges) > 2048 || airport.BufferMinutes < 0 || airport.BufferMinutes > 120 {
		return nil, errors.New("graph limits are 256 checkpoints, 2048 edges, and a 0 to 120 minute buffer")
	}
	if len(airport.Nodes) == 0 && airport.MapURL == "" {
		return nil, errors.New("configure checkpoints or an airport map URL")
	}
	if airport.MapURL != "" && !connectionMapURL(airport.MapURL) {
		return nil, errors.New("airport map URL must be a complete HTTPS URL without credentials")
	}
	graph := &connectionGraph{
		mapURL:        airport.MapURL,
		bufferMinutes: airport.BufferMinutes,
		nodes:         airport.Nodes,
		arcs:          make([][]connectionArc, len(airport.Nodes)),
		terminals:     make(map[string]int),
		gates:         make(map[string]map[string]int),
	}
	ids := make(map[string]int)
	for i, node := range airport.Nodes {
		if _, exists := ids[node.ID]; exists || !connectionCheckpointID.MatchString(node.ID) || !connectionText(node.Label, 128) {
			return nil, fmt.Errorf("invalid or duplicate checkpoint %q", node.ID)
		}
		ids[node.ID] = i
		terminal, gate := connectionCode(node.Terminal), connectionCode(node.Gate)
		if terminal == "" && gate == "" {
			continue
		}
		if !connectionLocationCode.MatchString(terminal) {
			return nil, fmt.Errorf("checkpoint %q requires a valid terminal code", node.ID)
		}
		if gate == "" {
			if _, exists := graph.terminals[terminal]; exists {
				return nil, fmt.Errorf("duplicate checkpoint for terminal %q", terminal)
			}
			graph.terminals[terminal] = i
			continue
		}
		if !connectionLocationCode.MatchString(gate) {
			return nil, fmt.Errorf("checkpoint %q requires a valid gate code", node.ID)
		}
		if graph.gates[gate] == nil {
			graph.gates[gate] = make(map[string]int)
		}
		if _, exists := graph.gates[gate][terminal]; exists {
			return nil, fmt.Errorf("duplicate checkpoint for terminal %q gate %q", terminal, gate)
		}
		graph.gates[gate][terminal] = i
	}
	seenArcs := make(map[[2]int]bool)
	for _, edge := range airport.Edges {
		from, fromOK := ids[edge.From]
		to, toOK := ids[edge.To]
		key := [2]int{from, to}
		if !fromOK || !toOK || from == to || seenArcs[key] || edge.Minutes < 1 || edge.Minutes > 240 {
			return nil, fmt.Errorf("invalid or duplicate edge from %q to %q", edge.From, edge.To)
		}
		instruction := edge.Instruction
		if instruction == "" {
			instruction = "Continue to " + graph.nodes[to].Label + "."
		} else if !connectionText(instruction, 256) {
			return nil, fmt.Errorf("invalid instruction from %q to %q", edge.From, edge.To)
		}
		seenArcs[key] = true
		graph.arcs[from] = append(graph.arcs[from], connectionArc{to, edge.Minutes, instruction})
	}
	return graph, nil
}

func connectionCode(value string) string { return strings.ToUpper(strings.TrimSpace(value)) }

func connectionText(value string, limit int) bool {
	return len(value) <= limit && strings.TrimSpace(value) != "" && strings.IndexFunc(value, unicode.IsControl) == -1
}

func connectionMapURL(value string) bool {
	parsed, err := url.Parse(value)
	return len(value) <= 2048 && err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil
}

func (g *connectionGraph) endpoint(terminal, gate string) (node int, isGate, found bool) {
	terminal, gate = connectionCode(terminal), connectionCode(gate)
	gates := g.gates[gate]
	if node, ok := gates[terminal]; ok {
		return node, true, true
	}
	if terminal == "" && len(gates) == 1 {
		for _, node := range gates {
			return node, true, true
		}
	}
	node, ok := g.terminals[terminal]
	return node, false, ok
}

// Route uses the most precise identifiable checkpoints. Even if no configured
// path exists, MapURL may identify the airport's own map.
func (r *ConnectionRules) Route(airport, alternateAirport, arrivalTerminal, arrivalGate, departureTerminal, departureGate string) (ConnectionRoute, bool) {
	if r == nil {
		return ConnectionRoute{}, false
	}
	graph := r.byCode[connectionCode(airport)]
	if graph == nil {
		graph = r.byCode[connectionCode(alternateAirport)]
	}
	if graph == nil {
		return ConnectionRoute{}, false
	}
	result := ConnectionRoute{MapURL: graph.mapURL}
	from, fromGate, fromOK := graph.endpoint(arrivalTerminal, arrivalGate)
	to, toGate, toOK := graph.endpoint(departureTerminal, departureGate)
	if !fromOK || !toOK || (from == to && !fromGate) {
		return result, false
	}
	minutes, directions, ok := graph.shortestPath(from, to)
	if !ok {
		return result, false
	}
	result.TransferMinutes = minutes + graph.bufferMinutes
	result.Instructions = directions
	switch {
	case fromGate && toGate:
		result.Precision = "gates"
	case fromGate || toGate:
		result.Precision = "mixed"
	default:
		result.Precision = "terminals"
	}
	return result, true
}

// The graph is limited to 256 checkpoints. A scan per step keeps Dijkstra's
// algorithm compact, and declaration order resolves equal-cost routes.
func (g *connectionGraph) shortestPath(from, to int) (int, []string, bool) {
	const unreachable = int(^uint(0) >> 1)
	distances := make([]int, len(g.nodes))
	visited := make([]bool, len(g.nodes))
	previous := make([]struct {
		node        int
		instruction string
	}, len(g.nodes))
	for i := range distances {
		distances[i] = unreachable
	}
	distances[from] = 0

	for {
		next := -1
		for i, distance := range distances {
			if !visited[i] && distance != unreachable && (next == -1 || distance < distances[next]) {
				next = i
			}
		}
		if next == -1 {
			return 0, nil, false
		}
		if next == to {
			break
		}
		visited[next] = true
		for _, arc := range g.arcs[next] {
			if distance := distances[next] + arc.minutes; distance < distances[arc.to] {
				distances[arc.to] = distance
				previous[arc.to].node = next
				previous[arc.to].instruction = arc.instruction
			}
		}
	}
	var directions []string
	for node := to; node != from; node = previous[node].node {
		directions = append(directions, previous[node].instruction)
	}
	if len(directions) == 0 {
		directions = append(directions, "Your next flight uses "+g.nodes[to].Label+".")
	}
	slices.Reverse(directions)
	return distances[to], directions, true
}
