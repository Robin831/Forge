package main

import (
	"fmt"
	"os"

	"github.com/Robin831/Forge/internal/autostart"
	"github.com/spf13/cobra"
)

var autostartCmd = &cobra.Command{
	Use:   "autostart",
	Short: "Manage auto-start at user logon",
	Long:  `Install, remove, or check the Windows Task Scheduler task that starts forge up at logon.`,
}

var autostartInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Register forge as a logon task",
	RunE: func(cmd *cobra.Command, args []string) error {
		return autostart.Install()
	},
}

var autostartRemoveCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove the autostart task",
	RunE: func(cmd *cobra.Command, args []string) error {
		return autostart.Remove()
	},
}

var autostartStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check autostart registration",
	RunE: func(cmd *cobra.Command, args []string) error {
		registered, nextRun, err := autostart.Status()
		if err != nil {
			return err
		}
		if !registered {
			fmt.Println("Not registered. Run 'forge autostart install' to set up.")
			os.Exit(1)
		}
		fmt.Printf("Registered: %s\n", autostart.TaskName)
		if nextRun != "" {
			fmt.Printf("Next run: %s\n", nextRun)
		}
		return nil
	},
}

var autostartGenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate the task XML without registering",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := autostart.GenerateXML()
		if err != nil {
			return err
		}
		fmt.Printf("XML written to: %s\n", path)
		fmt.Println("Register manually: schtasks /create /tn ForgeAutoStart /xml <path> /f")
		return nil
	},
}

func init() {
	autostartCmd.AddCommand(autostartInstallCmd)
	autostartCmd.AddCommand(autostartRemoveCmd)
	autostartCmd.AddCommand(autostartStatusCmd)
	autostartCmd.AddCommand(autostartGenerateCmd)
	rootCmd.AddCommand(autostartCmd)
}
