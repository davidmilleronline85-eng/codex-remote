package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/davidmilleronline85-eng/codex-remote/internal/codexremote"

	"github.com/spf13/cobra"
)

const version = "0.1.0"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer cancel()

	var configPath string

	rootCmd := &cobra.Command{
		Use:               "codex-remote",
		Short:             "Make a machine Codex-remote-ready",
		Long:              "codex-remote installs and supervises a local Codex app-server, then helps you expose it safely for remote agents.",
		Version:           version,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		SilenceUsage:      true,
		SilenceErrors:     true,
	}
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "path to the codex-remote config file")

	rootCmd.AddCommand(
		cmdStart(&configPath),
		cmdStatus(&configPath),
		cmdToken(&configPath),
		cmdExpose(&configPath),
		cmdDaemon(&configPath),
		cmdRun(&configPath),
		cmdDoctor(&configPath),
		cmdPrintLaunchd(&configPath),
	)

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func cmdStart(configPath *string) *cobra.Command {
	var opts codexremote.StartOptions

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start Codex Remote in the foreground and print an agent-ready handoff block",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.ConfigPath = *configPath
			return codexremote.StartForeground(cmd.Context(), opts, os.Stdout, os.Stderr)
		},
	}

	cmd.Flags().StringVar(&opts.StateDir, "state-dir", "", "state directory for config, logs, and token")
	cmd.Flags().StringVar(&opts.CodexPath, "codex-path", "", "path to the codex executable")
	cmd.Flags().StringVar(&opts.ListenURL, "listen-url", "", "listen URL for codex app-server, e.g. ws://127.0.0.1:8765")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "overwrite an existing config")
	cmd.Flags().DurationVar(&opts.WaitTimeout, "wait-timeout", 20*time.Second, "how long to wait for readyz after install")
	cmd.Flags().BoolVar(&opts.Public, "public", true, "create a public Cloudflare Quick Tunnel and print a shareable URL")
	cmd.Flags().BoolVar(&opts.Public, "tunnel", true, "alias for --public")
	return cmd
}

func cmdRun(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the codex-remote supervisor in the foreground",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := codexremote.Load(*configPath)
			if err != nil {
				return err
			}
			runner := codexremote.Runner{Config: cfg}
			return runner.Run(cmd.Context())
		},
	}
	cmd.Hidden = true
	return cmd
}

func cmdDoctor(configPath *string) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check whether the machine is codex-remote-ready",
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := codexremote.RunDoctor(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			if jsonOutput {
				data, err := report.JSON()
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			fmt.Printf("Config: %s\n", report.Config)
			for _, check := range report.Checks {
				status := "ok"
				if !check.OK {
					status = "fail"
				}
				fmt.Printf("[%s] %s: %s\n", status, check.Name, check.Details)
			}
			if report.OK {
				fmt.Println("Doctor result: ready")
			} else {
				fmt.Println("Doctor result: not ready")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the doctor report as JSON")
	cmd.Hidden = true
	return cmd
}

func cmdStatus(configPath *string) *cobra.Command {
	var label string
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show service status and health checks",
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := codexremote.RunDoctor(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			service, err := codexremote.GetServiceStatus(label)
			if err != nil {
				return err
			}
			if jsonOutput {
				data, err := codexremote.StatusJSON(service, report)
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			fmt.Printf("Service label: %s\n", service.Label)
			fmt.Printf("Installed: %t\n", service.Installed)
			fmt.Printf("Loaded: %t\n", service.Loaded)
			if service.PID > 0 {
				fmt.Printf("PID: %d\n", service.PID)
			}
			fmt.Printf("Config: %s\n", report.Config)
			for _, check := range report.Checks {
				status := "ok"
				if !check.OK {
					status = "fail"
				}
				fmt.Printf("[%s] %s: %s\n", status, check.Name, check.Details)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", codexremote.DefaultLabel, "launchd service label")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the full status as JSON")
	return cmd
}

func cmdToken(configPath *string) *cobra.Command {
	var shell bool
	var pathOnly bool

	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print the Codex bearer token or token file path",
		RunE: func(cmd *cobra.Command, args []string) error {
			if pathOnly {
				cfg, _, err := codexremote.Load(*configPath)
				if err != nil {
					return err
				}
				fmt.Println(cfg.Codex.TokenFile)
				return nil
			}
			token, err := codexremote.ReadToken(*configPath)
			if err != nil {
				return err
			}
			if shell {
				fmt.Printf("export CODEX_REMOTE_TOKEN=%q\n", token)
				return nil
			}
			fmt.Println(token)
			return nil
		},
	}
	cmd.Flags().BoolVar(&shell, "shell", false, "print an export command instead of the raw token")
	cmd.Flags().BoolVar(&pathOnly, "path", false, "print the token file path instead of the token value")
	return cmd
}

func cmdExpose(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "expose",
		Short: "Expose the local Codex app-server",
	}
	cmd.AddCommand(cmdExposeQuick(configPath))
	return cmd
}

func cmdExposeQuick(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "quick",
		Short: "Expose the local app-server through a temporary Cloudflare Quick Tunnel",
		RunE: func(cmd *cobra.Command, args []string) error {
			return codexremote.RunQuickTunnel(cmd.Context(), *configPath, os.Stdout, os.Stderr, nil)
		},
	}
}

func cmdDaemon(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Optional background-service commands for macOS launchd",
	}
	cmd.AddCommand(
		cmdDaemonInstall(configPath),
		cmdDaemonStart(configPath),
		cmdDaemonStop(),
		cmdDaemonRestart(configPath),
		cmdDaemonUninstall(configPath),
	)
	return cmd
}

func cmdDaemonInstall(configPath *string) *cobra.Command {
	var opts codexremote.InstallOptions

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start codex-remote as a background launchd service",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.ConfigPath = *configPath
			result, err := codexremote.Install(cmd.Context(), opts)
			if err != nil {
				return err
			}
			fmt.Printf("Installed daemon %s\n", result.Label)
			fmt.Printf("Config: %s\n", result.ConfigPath)
			fmt.Printf("Ready URL: %s\n", result.ReadyURL)
			return nil
		},
	}

	cmd.Flags().StringVar(&opts.StateDir, "state-dir", "", "state directory for config, logs, and token")
	cmd.Flags().StringVar(&opts.CodexPath, "codex-path", "", "path to the codex executable")
	cmd.Flags().StringVar(&opts.ListenURL, "listen-url", "", "listen URL for codex app-server, e.g. ws://127.0.0.1:8765")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "overwrite an existing config")
	cmd.Flags().StringVar(&opts.Label, "label", codexremote.DefaultLabel, "launchd service label")
	cmd.Flags().DurationVar(&opts.WaitTimeout, "wait-timeout", 20*time.Second, "how long to wait for readyz after install")
	return cmd
}

func cmdDaemonStart(configPath *string) *cobra.Command {
	var label string
	var waitTimeout time.Duration

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the installed background service",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := codexremote.Load(*configPath)
			if err != nil {
				return err
			}
			if err := codexremote.StartService(label); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), waitTimeout)
			defer cancel()
			if err := codexremote.WaitReady(ctx, cfg.Codex.ReadyURL, 2*time.Second); err != nil {
				return err
			}
			fmt.Printf("Started %s\n", label)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", codexremote.DefaultLabel, "launchd service label")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 20*time.Second, "how long to wait for readyz after start")
	return cmd
}

func cmdDaemonStop() *cobra.Command {
	var label string

	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the installed background service",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := codexremote.StopService(label); err != nil {
				return err
			}
			fmt.Printf("Stopped %s\n", label)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", codexremote.DefaultLabel, "launchd service label")
	return cmd
}

func cmdDaemonRestart(configPath *string) *cobra.Command {
	var label string
	var waitTimeout time.Duration

	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the installed background service",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := codexremote.Load(*configPath)
			if err != nil {
				return err
			}
			if err := codexremote.RestartService(label); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), waitTimeout)
			defer cancel()
			if err := codexremote.WaitReady(ctx, cfg.Codex.ReadyURL, 2*time.Second); err != nil {
				return err
			}
			fmt.Printf("Restarted %s\n", label)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", codexremote.DefaultLabel, "launchd service label")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 20*time.Second, "how long to wait for readyz after restart")
	return cmd
}

func cmdDaemonUninstall(configPath *string) *cobra.Command {
	var label string
	var purge bool

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the background service and optionally remove state",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := codexremote.Uninstall(*configPath, label, purge); err != nil {
				return err
			}
			fmt.Printf("Uninstalled %s\n", label)
			if purge {
				fmt.Println("Removed state directory")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", codexremote.DefaultLabel, "launchd service label")
	cmd.Flags().BoolVar(&purge, "purge", false, "delete the state directory, token, and generated config")
	return cmd
}

func cmdPrintLaunchd(configPath *string) *cobra.Command {
	var label string
	var binaryPath string

	cmd := &cobra.Command{
		Use:   "print-launchd",
		Short: "Print the generated launchd plist",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, resolvedConfig, err := codexremote.Load(*configPath)
			if err != nil {
				return err
			}
			if binaryPath == "" {
				binaryPath, err = os.Executable()
				if err != nil {
					return err
				}
			}
			plist, err := codexremote.RenderLaunchdPlist(label, binaryPath, resolvedConfig, cfg.StateDir)
			if err != nil {
				return err
			}
			fmt.Println(plist)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", codexremote.DefaultLabel, "launchd service label")
	cmd.Flags().StringVar(&binaryPath, "binary-path", "", "path to the codex-remote binary")
	cmd.Hidden = true
	return cmd
}
