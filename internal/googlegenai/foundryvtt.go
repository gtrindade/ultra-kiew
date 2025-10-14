package googlegenai

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/genai"
)

const (
	// FoundryVTTToolName is the name of the tool that manages FoundryVTT versions
	FoundryVTTToolName = "foundry_vtt"

	serviceFileName = "foundryvtt.service"
	latestSymlink   = "FoundryVTT-latest"
)

var (
	// FoundryVTTTool provides functionality to manage FoundryVTT versions
	FoundryVTTTool = &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        FoundryVTTToolName,
				Description: "Manages FoundryVTT versions and service configuration",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"action": {
							Type: "string",
							Description: `Actions to perform: 
								'list' to show available versions (the running version is marked as current)
								'switch' to change versions
							`,
							Enum: []string{"list", "switch"},
						},
						"version": {
							Type:        "string",
							Description: "Version to switch to (required for switch action)",
						},
					},
					Required: []string{"action"},
				},
			},
		},
	}
)

func (c *Client) FoundryVTT(args map[string]any) (string, error) {
	action, ok := args["action"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: action is required")
	}

	var response string
	var err error
	switch action {
	case "list":
		response, err = c.listFoundryVersions()
	case "switch":
		version, ok := args["version"].(string)
		if !ok || version == "" {
			return "", fmt.Errorf("version is required for switch action")
		}
		response, err = c.switchFoundryVersion(version)
	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
	if err != nil {
		fmt.Printf("Error executing FoundryVTT action %s: %v\n", action, err)
	}

	return response, err
}

func (c *Client) listFoundryVersions() (string, error) {
	entries, err := os.ReadDir(c.config.FoundryVTT.Directory)
	if err != nil {
		return "", fmt.Errorf("failed to read directory: %v", err)
	}

	var versions []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "FoundryVTT-") {
			version := strings.TrimPrefix(entry.Name(), "FoundryVTT-")
			if version == "latest" {
				continue
			}

			symlinkPath := filepath.Join(c.config.FoundryVTT.Directory, latestSymlink)
			currentVersion, err := os.Readlink(symlinkPath)
			if err == nil && currentVersion == entry.Name() {
				version += " (current)"
			}
			versions = append(versions, version)
		}
	}

	if len(versions) == 0 {
		return "No FoundryVTT versions found", nil
	}

	return fmt.Sprintf("Available versions:\n%s", strings.Join(versions, "\n")), nil
}

func (c *Client) switchFoundryVersion(version string) (string, error) {
	if !c.checkIfVersionExists(version) {
		return "", fmt.Errorf("version %s does not exist", version)
	}

	src := filepath.Join(c.config.FoundryVTT.Directory, "FoundryVTT-"+version)
	dst := filepath.Join(c.config.FoundryVTT.Directory, latestSymlink)
	err := c.overrideSymlink(src, dst)
	if err != nil {
		return "", fmt.Errorf("failed to update symlink: %v", err)
	}

	if err := c.runSystemCommand("systemctl", "daemon-reload"); err != nil {
		return "", fmt.Errorf("failed to reload systemd: %v", err)
	}

	if err := c.runSystemCommand("systemctl", "restart", "foundryvtt"); err != nil {
		return "", fmt.Errorf("failed to restart foundryvtt service: %v", err)
	}

	return fmt.Sprintf("Successfully switched to FoundryVTT version %s", version), nil
}

func (c *Client) checkIfVersionExists(version string) bool {
	versionDir := filepath.Join(c.config.FoundryVTT.Directory, "FoundryVTT-"+version)
	if _, err := os.Stat(versionDir); os.IsNotExist(err) {
		return false
	}
	return true
}

func (c *Client) overrideSymlink(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("failed to remove existing symlink: %v", err)
		}
	}

	if err := os.Symlink(src, dst); err != nil {
		return fmt.Errorf("failed to create symlink from %s to %s: %v", src, dst, err)
	}
	return nil
}

func (c *Client) runSystemCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run command %s: %v", name, err)
	}
	return nil
}
