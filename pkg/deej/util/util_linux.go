package util

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var errNoCompositor = errors.New("no supported compositor found for current window detection")

// getCurrentWindowProcessNames tries to find the currently focused window's
// process name using compositor-specific tools.
// Supports: Hyprland, Sway, and X11 (xprop fallback).
func getCurrentWindowProcessNames() ([]string, error) {
	// Try Hyprland first
	if names, err := tryHyprland(); err == nil {
		return names, nil
	}

	// Try Sway
	if names, err := trySway(); err == nil {
		return names, nil
	}

	// Fallback to X11
	if names, err := tryX11(); err == nil {
		return names, nil
	}

	return nil, errNoCompositor
}

// hyprlandActiveWindow represents the JSON output of `hyprctl activewindow -j`
type hyprlandActiveWindow struct {
	Class string `json:"class"`
}

func tryHyprland() ([]string, error) {
	cmd := exec.Command("hyprctl", "activewindow", "-j")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("hyprctl failed: %w", err)
	}

	var win hyprlandActiveWindow
	if err := json.Unmarshal(out, &win); err != nil {
		return nil, fmt.Errorf("hyprctl json parse failed: %w", err)
	}

	if win.Class == "" {
		return nil, errors.New("hyprctl returned empty class")
	}

	return []string{strings.ToLower(win.Class)}, nil
}

// swayNode represents a node in Sway's window tree
type swayNode struct {
	Focused bool     `json:"focused"`
	AppID   string   `json:"app_id"`
	WindowProperties struct {
		Class string `json:"class"`
	} `json:"window_properties"`
	Nodes []swayNode `json:"nodes"`
}

func trySway() ([]string, error) {
	cmd := exec.Command("swaymsg", "-t", "get_tree")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("swaymsg failed: %w", err)
	}

	var root swayNode
	if err := json.Unmarshal(out, &root); err != nil {
		return nil, fmt.Errorf("swaymsg json parse failed: %w", err)
	}

	name := findSwayFocused(&root)
	if name == "" {
		return nil, errors.New("sway: no focused window found")
	}

	return []string{strings.ToLower(name)}, nil
}

func findSwayFocused(node *swayNode) string {
	if node.Focused {
		if node.AppID != "" {
			return node.AppID
		}
		return node.WindowProperties.Class
	}
	for i := range node.Nodes {
		if result := findSwayFocused(&node.Nodes[i]); result != "" {
			return result
		}
	}
	return ""
}

func tryX11() ([]string, error) {
	// Get the active window ID
	cmd := exec.Command("xprop", "-root", "_NET_ACTIVE_WINDOW")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("xprop root failed: %w", err)
	}

	parts := strings.Fields(string(out))
	if len(parts) < 5 {
		return nil, errors.New("xprop: unexpected output format")
	}

	winID := parts[4]

	// Get WM_CLASS for that window
	cmd = exec.Command("xprop", "-id", winID, "WM_CLASS")
	out, err = cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("xprop id failed: %w", err)
	}

	// WM_CLASS output format: WM_CLASS(STRING) = "class", "class"
	fields := strings.Split(string(out), " = ")
	if len(fields) < 2 {
		return nil, errors.New("xprop: unexpected WM_CLASS format")
	}

	classes := strings.Split(fields[1], ", ")
	if len(classes) == 0 {
		return nil, errors.New("xprop: no classes found")
	}

	// Take the second quoted string if available (instance, class)
	name := strings.Trim(strings.TrimSpace(classes[len(classes)-1]), `"`)
	if name == "" {
		return nil, errors.New("xprop: empty class")
	}

	return []string{strings.ToLower(name)}, nil
}
