package deej

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// pipewireNode represents the minimal structure we need from pw-dump output
type pipewireNode struct {
	ID   int    `json:"id"`
	Type string `json:"type"`
	Info struct {
		Props map[string]interface{} `json:"props"`
	} `json:"info"`
}

type pipewireSessionFinder struct {
	logger        *zap.SugaredLogger
	sessionLogger *zap.SugaredLogger

	mu            sync.RWMutex
	cachedSessions []Session
	lastRefresh   time.Time
	needsRefresh  bool

	monitorCmd    *exec.Cmd
	monitorCancel chan struct{}
	debounceTimer *time.Timer
}

const pipewireMonitorDebounce = 2 * time.Second

func newSessionFinderPipewire(logger *zap.SugaredLogger) (SessionFinder, error) {
	sf := &pipewireSessionFinder{
		logger:        logger.Named("session_finder"),
		sessionLogger: logger.Named("sessions"),
		needsRefresh:  true,
		monitorCancel: make(chan struct{}),
	}

	sf.logger.Debug("Created PipeWire session finder instance")

	// Start background monitor for real-time updates
	go sf.startMonitor()

	return sf, nil
}

// startMonitor runs pw-dump --monitor in the background and triggers
// refreshes whenever PipeWire's graph changes.
func (sf *pipewireSessionFinder) startMonitor() {
	sf.monitorCmd = exec.Command("pw-dump", "--monitor")
	stdout, err := sf.monitorCmd.StdoutPipe()
	if err != nil {
		sf.logger.Warnw("Failed to create pw-dump stdout pipe", "error", err)
		return
	}

	if err := sf.monitorCmd.Start(); err != nil {
		sf.logger.Warnw("Failed to start pw-dump --monitor", "error", err)
		return
	}

	defer func() {
		if sf.monitorCmd.Process != nil {
			sf.monitorCmd.Process.Kill()
		}
	}()

	scanner := bufio.NewScanner(stdout)
	var buffer strings.Builder

	for scanner.Scan() {
		select {
		case <-sf.monitorCancel:
			return
		default:
		}

		line := scanner.Text()
		buffer.WriteString(line)
		buffer.WriteString("\n")

		// pw-dump --monitor outputs a full JSON array on each change
		if strings.TrimSpace(line) == "]" {
			data := buffer.String()
			buffer.Reset()

			var nodes []pipewireNode
			if err := json.Unmarshal([]byte(data), &nodes); err != nil {
				continue // partial/corrupt output, skip
			}

			sf.handleGraphChange(nodes)
		}
	}
}

func (sf *pipewireSessionFinder) handleGraphChange(nodes []pipewireNode) {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	// Cancel existing timer if running
	if sf.debounceTimer != nil {
		sf.debounceTimer.Stop()
	}

	// Start a new debounce timer
	sf.debounceTimer = time.AfterFunc(pipewireMonitorDebounce, func() {
		sf.mu.Lock()
		sf.needsRefresh = true
		sf.mu.Unlock()
		sf.logger.Debug("PipeWire graph change debounced, marking sessions for refresh")
	})
}

func (sf *pipewireSessionFinder) GetAllSessions() ([]Session, error) {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	// Return cached sessions if fresh enough and no refresh needed
	if !sf.needsRefresh && len(sf.cachedSessions) > 0 &&
		sf.lastRefresh.Add(minTimeBetweenSessionRefreshes).After(time.Now()) {
		return sf.cachedSessions, nil
	}

	sessions := []Session{}

	// Get master output session
	masterOut, err := sf.getMasterSession(true)
	if err == nil {
		sessions = append(sessions, masterOut)
	} else {
		sf.logger.Warnw("Failed to get master output session", "error", err)
	}

	// Get master input session
	masterIn, err := sf.getMasterSession(false)
	if err == nil {
		sessions = append(sessions, masterIn)
	} else {
		sf.logger.Warnw("Failed to get master input session", "error", err)
	}

	// Enumerate all devices (sinks + sources) for device-specific control
	if err := sf.enumerateAndAddDevices(&sessions); err != nil {
		sf.logger.Warnw("Failed to enumerate audio devices", "error", err)
	}

	// Enumerate audio stream nodes (output + input)
	streamNodes, err := nodesFromPwDump()
	if err != nil {
		sf.logger.Warnw("Failed to get nodes from pw-dump", "error", err)
		return nil, fmt.Errorf("get nodes from pw-dump: %w", err)
	}
	if err := sf.enumerateAndAddStreams(streamNodes, &sessions); err != nil {
		sf.logger.Warnw("Failed to enumerate audio streams", "error", err)
		return nil, fmt.Errorf("enumerate audio streams: %w", err)
	}

	sf.cachedSessions = sessions
	sf.lastRefresh = time.Now()
	sf.needsRefresh = false

	sf.logger.Infow("Refreshed PipeWire audio sessions", "count", len(sessions))
	return sessions, nil
}

func (sf *pipewireSessionFinder) Release() error {
	close(sf.monitorCancel)
	if sf.monitorCmd != nil && sf.monitorCmd.Process != nil {
		sf.monitorCmd.Process.Kill()
	}
	if sf.debounceTimer != nil {
		sf.debounceTimer.Stop()
	}
	sf.logger.Debug("Released PipeWire session finder instance")
	return nil
}

func (sf *pipewireSessionFinder) getMasterSession(isOutput bool) (Session, error) {
	var key string
	if isOutput {
		key = masterSessionName
	} else {
		key = inputSessionName
	}

	// Get the default node name from wpctl status
	cmd := exec.Command("wpctl", "status")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("get default node: %w", err)
	}

	nodeName := sf.parseDefaultNode(string(out), isOutput)

	// Try to get the node ID from pw-dump
	nodeID := sf.findNodeIDByName(nodeName)

	master := newPipewireMasterSession(sf.sessionLogger, nodeName, nodeID, isOutput, key)
	return master, nil
}

// enumerateAndAddDevices adds every Audio/Sink and Audio/Source as a
// bindable "device" session, addressable by its friendly name.
// If the same name exists for both input and output, a suffix is added.
func (sf *pipewireSessionFinder) enumerateAndAddDevices(sessions *[]Session) error {
	nodes, err := nodesFromPwDump()
	if err != nil {
		return err
	}

	// First pass: collect all device names and check for conflicts
	type deviceInfo struct {
		nodeID   int
		nodeName string
		desc     string
		isOutput bool
	}

	devices := []deviceInfo{}
	nameCounts := make(map[string]int)

	for _, node := range nodes {
		if node.Type != "PipeWire:Interface:Node" {
			continue
		}

		props := node.Info.Props
		if props == nil {
			continue
		}

		mediaClass, _ := props["media.class"].(string)
		if mediaClass != "Audio/Sink" && mediaClass != "Audio/Source" {
			continue
		}

		nodeName, _ := props["node.name"].(string)
		desc, _ := props["device.description"].(string)
		if desc == "" {
			desc, _ = props["node.description"].(string)
		}
		if desc == "" {
			desc = nodeName
		}
		if desc == "" {
			continue
		}

		isOutput := mediaClass == "Audio/Sink"
		devices = append(devices, deviceInfo{node.ID, nodeName, desc, isOutput})
		nameCounts[desc]++
	}

	// Second pass: create sessions, disambiguating names if needed
	for _, dev := range devices {
		key := dev.desc
		// If this name appears more than once, add a direction suffix
		if nameCounts[dev.desc] > 1 {
			if dev.isOutput {
				key = dev.desc + " [Out]"
			} else {
				key = dev.desc + " [In]"
			}
		}

		deviceSession := newPipewireMasterSession(
			sf.sessionLogger,
			dev.nodeName,
			dev.nodeID,
			dev.isOutput,
			key,
		)
		*sessions = append(*sessions, deviceSession)
	}

	return nil
}

func (sf *pipewireSessionFinder) enumerateAndAddStreams(nodes []pipewireNode, sessions *[]Session) error {
	type streamInfo struct {
		nodeID   int
		binary   string
		nodeName string
		isOutput bool
	}

	streams := []streamInfo{}
	nameCounts := make(map[string]int)

	// First pass: collect streams and count process names
	for _, node := range nodes {
		if node.Type != "PipeWire:Interface:Node" {
			continue
		}

		props := node.Info.Props
		if props == nil {
			continue
		}

		mediaClass, _ := props["media.class"].(string)
		if mediaClass != "Stream/Output/Audio" && mediaClass != "Stream/Input/Audio" {
			continue
		}

		binary, _ := props["application.process.binary"].(string)
		nodeName, _ := props["node.name"].(string)
		
		// If no process binary, check for virtual streams like loopbacks
		if binary == "" {
			if strings.Contains(nodeName, "loopback") {
				// Use a stable name for loopbacks based on their target
				// The output loopback goes to FiiO, name it accordingly
				binary = "fiioloopback"
			} else {
				continue
			}
		}

		isOutput := mediaClass == "Stream/Output/Audio"

		streams = append(streams, streamInfo{node.ID, binary, nodeName, isOutput})
		nameCounts[binary]++
	}

	// Second pass: create sessions, adding [In] suffix for input streams
	// and [Out] suffix for output streams if the process has both directions
	for _, s := range streams {
		name := s.binary
		if nameCounts[s.binary] > 1 {
			if s.isOutput {
				name = s.binary + " [Out]"
			} else {
				name = s.binary + " [In]"
			}
		}

		newSession := newPipewireSession(sf.sessionLogger, s.nodeID, 0, name, s.nodeName)
		*sessions = append(*sessions, newSession)
	}

	return nil
}

func (sf *pipewireSessionFinder) parseDefaultNode(status string, isOutput bool) string {
	lines := strings.Split(status, "\n")
	inSection := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if isOutput && strings.Contains(trimmed, "Sinks:") {
			inSection = true
			continue
		}
		if !isOutput && strings.Contains(trimmed, "Sources:") {
			inSection = true
			continue
		}
		if inSection && (strings.Contains(trimmed, "Filters:") || strings.Contains(trimmed, "Streams:")) {
			break
		}
		if inSection && !isOutput && strings.Contains(trimmed, "Sinks:") {
			break
		}

		if inSection && strings.HasPrefix(trimmed, "*") {
			// Default node line: *   56. Internes Audio Analoges Stereo      [vol: 0.25]
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 {
				nameParts := []string{}
				for i, p := range parts[1:] {
					if i == 0 && strings.HasSuffix(p, ".") {
						continue // skip ID like "56."
					}
					if strings.HasPrefix(p, "[vol:") {
						break
					}
					nameParts = append(nameParts, p)
				}
				return strings.Join(nameParts, " ")
			}
		}
	}

	return ""
}

func (sf *pipewireSessionFinder) findNodeIDByName(nodeName string) int {
	if nodeName == "" {
		return 0
	}

	nodes, err := nodesFromPwDump()
	if err != nil {
		return 0
	}

	for _, node := range nodes {
		if node.Type != "PipeWire:Interface:Node" {
			continue
		}

		props := node.Info.Props
		if props == nil {
			continue
		}

		name, _ := props["node.name"].(string)
		mediaClass, _ := props["media.class"].(string)

		if name == nodeName && (mediaClass == "Audio/Sink" || mediaClass == "Audio/Source") {
			return node.ID
		}
	}

	return 0
}

// nodesFromPwDump runs pw-dump once and returns all nodes.
func nodesFromPwDump() ([]pipewireNode, error) {
	cmd := exec.Command("pw-dump")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pw-dump failed: %w", err)
	}

	var nodes []pipewireNode
	if err := json.Unmarshal(out, &nodes); err != nil {
		return nil, fmt.Errorf("parse pw-dump output: %w", err)
	}

	return nodes, nil
}
