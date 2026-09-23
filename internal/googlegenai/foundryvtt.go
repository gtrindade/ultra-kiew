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

	// symlink is the link inside the install directory that points at the
	// version currently in use, and versionPrefix is what an installed
	// version directory is called: Server-13.351, Server-14.368 and so on.
	//
	// These were "FoundryVTT" and "FoundryVTT-", which matched nothing on the
	// actual server and is why the listing came back empty while Foundry ran
	// perfectly well. The real layout is:
	//
	//	<directory>/Data/                     -- Foundry's data, not a version
	//	<directory>/Server -> Server-13.351   -- the active version
	//	<directory>/Server-13.351/
	//	<directory>/Server-14.368/
	//
	// Data is excluded for free: it does not carry the prefix. The symlink is
	// deliberately NOT prefixed either, which is what keeps it out of the
	// version list.
	symlink       = "Server"
	versionPrefix = symlink + "-"

	// maxListedEntries bounds what a failed listing reports back, so a
	// directory pointed at something enormous does not try to send all of it
	// to Telegram.
	maxListedEntries = 15
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

// foundryDir returns the configured install directory, or an error naming what
// is missing.
//
// Without this, a config with no foundry_vtt block dereferenced a nil pointer
// and took the whole bot down -- a panic inside a tool call is not caught
// anywhere above it.
func (c *Client) foundryDir() (string, error) {
	if c.config == nil || c.config.FoundryVTT == nil {
		return "", fmt.Errorf("FoundryVTT is not configured on this server: config.yaml has no foundry_vtt block. Tell the user that plainly")
	}
	if strings.TrimSpace(c.config.FoundryVTT.Directory) == "" {
		return "", fmt.Errorf("FoundryVTT is configured but foundry_vtt.directory is empty in config.yaml. Tell the user that plainly")
	}
	return c.config.FoundryVTT.Directory, nil
}

func (c *Client) listFoundryVersions() (string, error) {
	dir, err := c.foundryDir()
	if err != nil {
		return "", err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("failed to read the FoundryVTT directory %s: %w", dir, err)
	}

	// Stat rather than Readlink, and compared with SameFile, so that the link
	// is recognised whether it is relative (Server -> Server-13.351, what is
	// on the server and what switchFoundryVersion now writes) or absolute
	// (what it used to write). Comparing raw link text against a bare
	// directory name only ever matched the first kind, so the "(current)"
	// marker silently stopped working for any version the bot switched to
	// itself.
	var currentInfo os.FileInfo
	if info, err := os.Stat(filepath.Join(dir, symlink)); err == nil {
		currentInfo = info
	}

	var versions []string
	var ignored []string
	for _, entry := range entries {
		name := entry.Name()
		if name == symlink {
			continue
		}
		if !strings.HasPrefix(name, versionPrefix) {
			ignored = append(ignored, name)
			continue
		}

		// os.Stat, not entry.IsDir(): ReadDir reports the type of the
		// directory entry itself, so a version that lives elsewhere and is
		// symlinked in here reads as "not a directory" and was skipped in
		// silence -- one of the ways this listing can come back empty on an
		// install that is working perfectly well.
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.IsDir() {
			ignored = append(ignored, name+" (not a readable directory)")
			continue
		}

		version := strings.TrimPrefix(name, versionPrefix)
		if currentInfo != nil && os.SameFile(currentInfo, info) {
			version += " (current)"
		}
		versions = append(versions, version)
	}

	if len(versions) == 0 {
		return emptyFoundryListing(dir, entries, ignored), nil
	}

	return fmt.Sprintf("Available FoundryVTT versions in %s:\n%s", dir, strings.Join(versions, "\n")), nil
}

// emptyFoundryListing explains WHY nothing was found.
//
// The message this replaces was "No FoundryVTT versions found": true, and
// useless. It cannot tell a wrong path from an empty directory from a renamed
// folder, so the only way to distinguish them was to go and look at the server
// by hand -- which is the exact chore a bot with filesystem access should be
// saving someone. A negative result should say what it actually saw.
func emptyFoundryListing(dir string, entries []os.DirEntry, ignored []string) string {
	if len(entries) == 0 {
		return fmt.Sprintf("No FoundryVTT versions found: the directory %s exists but is completely empty. Either the path in config.yaml is wrong or the installs live somewhere else. Report this to the user including the path.", dir)
	}

	listed := ignored
	if len(listed) > maxListedEntries {
		listed = append(listed[:maxListedEntries:maxListedEntries], fmt.Sprintf("...and %d more", len(ignored)-maxListedEntries))
	}

	return fmt.Sprintf("No FoundryVTT versions found in %s. A version has to be a directory named %s<version>, for example %s13.351. That directory holds %d entries, none of which match: %s. Report this to the user exactly, including the path and what was found, so they can see whether the path is wrong or the folders are named differently.",
		dir, versionPrefix, versionPrefix, len(entries), strings.Join(listed, ", "))
}

func (c *Client) switchFoundryVersion(version string) (string, error) {
	dir, err := c.foundryDir()
	if err != nil {
		return "", err
	}
	if !c.checkIfVersionExists(version) {
		return "", fmt.Errorf("version %s does not exist", version)
	}

	// A RELATIVE target, matching the link already on the server
	// (Server -> Server-13.351) rather than an absolute one. It keeps the
	// whole install directory movable, and it means a link the bot writes
	// looks exactly like one written by hand.
	dst := filepath.Join(dir, symlink)
	if err := c.overrideSymlink(versionPrefix+version, dst); err != nil {
		return "", fmt.Errorf("failed to update symlink: %w", err)
	}

	// There was a "systemctl daemon-reload" here. It re-reads unit files, and
	// repointing a symlink changes no unit file -- systemd resolves ExecStart
	// at exec time. So it could never help, and as a sudo call that aborts the
	// switch when it fails, it could only hurt.

	if err := c.runSystemCommand("sudo", "systemctl", "restart", "foundryvtt"); err != nil {
		return "", fmt.Errorf("failed to restart foundryvtt service: %w", err)
	}

	return fmt.Sprintf("Successfully switched to FoundryVTT version %s", version), nil
}

func (c *Client) checkIfVersionExists(version string) bool {
	dir, err := c.foundryDir()
	if err != nil {
		return false
	}
	// Stat follows symlinks, and IsDir is checked rather than assumed: a
	// dangling link or a stray file of the right name is not a version.
	info, err := os.Stat(filepath.Join(dir, versionPrefix+version))
	return err == nil && info.IsDir()
}

func (c *Client) overrideSymlink(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("failed to remove existing symlink: %w", err)
		}
	}

	if err := os.Symlink(src, dst); err != nil {
		return fmt.Errorf("failed to create symlink from %s to %s: %w", src, dst, err)
	}
	return nil
}

func (c *Client) runSystemCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run command %s: %w", name, err)
	}
	return nil
}
