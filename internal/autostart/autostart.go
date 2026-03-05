// Package autostart generates and manages Windows Task Scheduler definitions
// for starting the Forge daemon automatically at user logon.
//
// Usage:
//
//	forge autostart install  -- creates and registers the scheduled task
//	forge autostart remove   -- removes the scheduled task
//	forge autostart status   -- checks current registration
package autostart

import (
	"fmt"
	"os"
	"os/exec"

	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/Robin831/Forge/internal/executil"
)

const (
	// TaskName is the Windows Task Scheduler task name.
	TaskName = "ForgeAutoStart"

	// TaskXMLFile is the filename for the exported XML definition.
	TaskXMLFile = "forge-autostart.xml"
)

// taskXMLTemplate is the Task Scheduler XML definition for forge up at logon.
var taskXMLTemplate = template.Must(template.New("task").Parse(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Starts the Forge orchestration daemon at user logon.</Description>
    <Author>{{.Username}}</Author>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>{{.Username}}</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>{{.Username}}</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT5M</Interval>
      <Count>3</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>{{.ForgePath}}</Command>
      <Arguments>up --foreground</Arguments>
      <WorkingDirectory>{{.WorkingDir}}</WorkingDirectory>
    </Exec>
  </Actions>
</Task>`))

// TaskConfig holds the template parameters for the scheduled task.
type TaskConfig struct {
	Username   string
	ForgePath  string
	WorkingDir string
}

// Install creates and registers the scheduled task.
// On non-Windows platforms, it returns an error with instructions.
func Install() error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("autostart install is Windows-only; on Linux/macOS use systemd/launchd instead")
	}

	cfg, err := buildConfig()
	if err != nil {
		return fmt.Errorf("building task config: %w", err)
	}

	// Write XML file
	xmlPath, err := writeXML(cfg)
	if err != nil {
		return fmt.Errorf("writing task XML: %w", err)
	}

	// Register via schtasks
	cmd := executil.HideWindow(exec.Command("schtasks", "/create", "/tn", TaskName, "/xml", xmlPath, "/f"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("registering task: %s (output: %s)", err, strings.TrimSpace(string(output)))
	}

	fmt.Printf("Task %q registered successfully.\n", TaskName)
	fmt.Printf("XML saved to: %s\n", xmlPath)
	fmt.Println("Forge daemon will start automatically at logon.")
	return nil
}

// Remove deregisters the scheduled task.
func Remove() error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("autostart is Windows-only")
	}

	cmd := executil.HideWindow(exec.Command("schtasks", "/delete", "/tn", TaskName, "/f"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("removing task: %s (output: %s)", err, strings.TrimSpace(string(output)))
	}

	fmt.Printf("Task %q removed.\n", TaskName)
	return nil
}

// Status checks whether the scheduled task is registered.
func Status() (registered bool, nextRun string, err error) {
	if runtime.GOOS != "windows" {
		return false, "", fmt.Errorf("autostart is Windows-only")
	}

	cmd := executil.HideWindow(exec.Command("schtasks", "/query", "/tn", TaskName, "/fo", "CSV", "/nh"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, "", nil // Not registered
	}

	line := strings.TrimSpace(string(output))
	if line == "" {
		return false, "", nil
	}

	// CSV format: "TaskName","Next Run Time","Status"
	parts := strings.Split(line, ",")
	if len(parts) >= 2 {
		nextRun = strings.Trim(parts[1], "\"")
	}

	return true, nextRun, nil
}

// GenerateXML writes the task XML to ~/.forge/ without registering it.
func GenerateXML() (string, error) {
	cfg, err := buildConfig()
	if err != nil {
		return "", fmt.Errorf("building config: %w", err)
	}
	return writeXML(cfg)
}

// buildConfig gathers the system info needed for the task definition.
func buildConfig() (*TaskConfig, error) {
	username := os.Getenv("USERNAME")
	if domain := os.Getenv("USERDOMAIN"); domain != "" {
		username = domain + "\\" + username
	}
	if username == "" {
		return nil, fmt.Errorf("cannot determine username")
	}

	// Find forge executable
	forgePath, err := os.Executable()
	if err != nil {
		// Fallback to looking in PATH
		forgePath, err = exec.LookPath("forge")
		if err != nil {
			return nil, fmt.Errorf("cannot find forge executable: %w", err)
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("finding home dir: %w", err)
	}

	return &TaskConfig{
		Username:   username,
		ForgePath:  forgePath,
		WorkingDir: home,
	}, nil
}

// writeXML renders the template and writes it to ~/.forge/.
func writeXML(cfg *TaskConfig) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	xmlPath := filepath.Join(home, ".forge", TaskXMLFile)
	f, err := os.Create(xmlPath)
	if err != nil {
		return "", fmt.Errorf("creating XML file: %w", err)
	}
	defer f.Close()

	if err := taskXMLTemplate.Execute(f, cfg); err != nil {
		return "", fmt.Errorf("rendering template: %w", err)
	}

	return xmlPath, nil
}
