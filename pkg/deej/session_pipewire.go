package deej

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// pipewireSession represents a single audio stream node in PipeWire
type pipewireSession struct {
	baseSession

	nodeID      int
	serial      int
	processName string
	nodeName    string
}

// pipewireMasterSession represents the default audio sink/source
type pipewireMasterSession struct {
	baseSession

	nodeName string
	nodeID   int
	isOutput bool
}

func newPipewireSession(
	logger *zap.SugaredLogger,
	nodeID int,
	serial int,
	processName string,
	nodeName string,
) *pipewireSession {

	s := &pipewireSession{
		nodeID:      nodeID,
		serial:      serial,
		processName: processName,
		nodeName:    nodeName,
	}

	s.name = processName
	s.humanReadableDesc = fmt.Sprintf("%s (node %d)", processName, nodeID)

	// use a self-identifying session name e.g. deej.sessions.chrome
	s.logger = logger.Named(s.Key())
	s.logger.Debugw(sessionCreationLogMessage, "session", s)

	return s
}

func newPipewireMasterSession(
	logger *zap.SugaredLogger,
	nodeName string,
	nodeID int,
	isOutput bool,
	key string,
) *pipewireMasterSession {

	s := &pipewireMasterSession{
		nodeName: nodeName,
		nodeID:   nodeID,
		isOutput: isOutput,
	}

	s.master = true
	s.device = (key != masterSessionName && key != inputSessionName)
	s.name = key
	s.humanReadableDesc = key

	s.logger = logger.Named(key)
	s.logger.Debugw(sessionCreationLogMessage, "session", s)

	return s
}

func (s *pipewireSession) GetVolume() float32 {
	// App streams use pw-cli enum-params to read channelVolumes
	cmd := exec.Command("pw-cli", "enum-params", strconv.Itoa(s.nodeID))
	out, err := cmd.Output()
	if err != nil {
		s.logger.Warnw("Failed to get session volume", "error", err)
		return 0
	}

	return parsePwCliVolume(string(out))
}

func (s *pipewireSession) SetVolume(v float32) error {
	cmd := exec.Command("pw-cli", "s", strconv.Itoa(s.nodeID), "Props",
		fmt.Sprintf("{ channelVolumes: [ %.2f, %.2f ] }", v, v))
	if err := cmd.Run(); err != nil {
		s.logger.Warnw("Failed to set session volume", "error", err, "volume", v)
		return fmt.Errorf("adjust session volume: %w", err)
	}

	s.logger.Debugw("Adjusting session volume", "to", fmt.Sprintf("%.2f", v))
	return nil
}

func (s *pipewireSession) Release() {
	s.logger.Debug("Releasing audio session")
}

func (s *pipewireSession) String() string {
	return fmt.Sprintf(sessionStringFormat, s.humanReadableDesc, s.GetVolume())
}

func (s *pipewireMasterSession) GetVolume() float32 {
	var target string
	if s.nodeID > 0 {
		target = strconv.Itoa(s.nodeID)
	} else if s.isOutput {
		target = "@DEFAULT_AUDIO_SINK@"
	} else {
		target = "@DEFAULT_AUDIO_SOURCE@"
	}

	cmd := exec.Command("wpctl", "get-volume", target)
	out, err := cmd.Output()
	if err != nil {
		s.logger.Warnw("Failed to get master volume", "error", err)
		return 0
	}

	return parseWpctlVolume(string(out))
}

func (s *pipewireMasterSession) SetVolume(v float32) error {
	var target string
	if s.nodeID > 0 {
		target = strconv.Itoa(s.nodeID)
	} else if s.isOutput {
		target = "@DEFAULT_AUDIO_SINK@"
	} else {
		target = "@DEFAULT_AUDIO_SOURCE@"
	}

	cmd := exec.Command("wpctl", "set-volume", target, fmt.Sprintf("%.2f", v))
	if err := cmd.Run(); err != nil {
		s.logger.Warnw("Failed to set master volume", "error", err, "volume", v)
		return fmt.Errorf("adjust master volume: %w", err)
	}

	s.logger.Debugw("Adjusting master volume", "to", fmt.Sprintf("%.2f", v))
	return nil
}

func (s *pipewireMasterSession) Release() {
	s.logger.Debug("Releasing audio session")
}

func (s *pipewireMasterSession) String() string {
	return fmt.Sprintf(sessionStringFormat, s.humanReadableDesc, s.GetVolume())
}

// parseWpctlVolume parses "Volume: 0.25" or "Volume: 0.25 [MUTED]" into a float32
func parseWpctlVolume(output string) float32 {
	output = strings.TrimSpace(output)
	if !strings.HasPrefix(output, "Volume: ") {
		return 0
	}

	fields := strings.Fields(output)
	if len(fields) < 2 {
		return 0
	}

	vol, err := strconv.ParseFloat(fields[1], 32)
	if err != nil {
		return 0
	}

	return float32(vol)
}

// parsePwCliVolume parses pw-cli enum-params output looking for channelVolumes
// It expects output like: "Float 0,500000" or "Float 1,000000"
func parsePwCliVolume(output string) float32 {
	lines := strings.Split(output, "\n")
	var volumes []float64

	for i, line := range lines {
		if strings.Contains(line, "channelVolumes") && i+3 < len(lines) {
			// Look at the next few lines for Float values
			for j := i + 1; j <= i+3 && j < len(lines); j++ {
				if strings.Contains(lines[j], "Float") {
					parts := strings.Fields(lines[j])
					if len(parts) >= 2 {
						// pw-cli uses comma as decimal separator in some locales
						valStr := strings.Replace(parts[1], ",", ".", 1)
						val, err := strconv.ParseFloat(valStr, 64)
						if err == nil {
							volumes = append(volumes, val)
						}
					}
				}
			}
		}
	}

	if len(volumes) == 0 {
		return 0
	}

	// Return average of all channel volumes
	var sum float64
	for _, v := range volumes {
		sum += v
	}
	return float32(sum / float64(len(volumes)))
}
