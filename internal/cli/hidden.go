package cli

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"sync"

	"github.com/spf13/cobra"

	"github.com/dokwalt/dokwalt/internal/api"
	"github.com/dokwalt/dokwalt/internal/daemon"
)

func hiddenCommands() []*cobra.Command {
	return []*cobra.Command{daemonCmd(), dialStdioCmd(), versionCmd()}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print the version",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if gf.json {
				return printJSON(api.VersionInfo{APIVersion: api.Version, Build: Build, OS: runtime.GOOS, Arch: runtime.GOARCH})
			}
			fmt.Printf("dokwalt %s (API %s, %s/%s)\n", Build, api.Version, runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

func daemonCmd() *cobra.Command {
	var o daemon.Options
	var debug bool
	cmd := &cobra.Command{
		Use: "daemon", Short: "Run the server daemon (systemd runs this)", Hidden: true,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			disableTHP()
			level := slog.LevelInfo
			if debug {
				level = slog.LevelDebug
			}
			// systemd's journal adds timestamps.
			o.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey && os.Getenv("INVOCATION_ID") != "" {
					return slog.Attr{}
				}
				return a
			}}))
			o.Build = Build
			return daemon.Run(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.DataDir, "data-dir", daemon.DefaultDataDir, "state directory")
	cmd.Flags().StringVar(&o.Socket, "socket", daemon.DefaultSocket, "API socket")
	cmd.Flags().StringVar(&o.DockerSocket, "docker-socket", "/var/run/docker.sock", "Docker socket")
	cmd.Flags().BoolVar(&debug, "debug", false, "verbose logs")
	return cmd
}

// dialStdioCmd bridges stdin/stdout to the daemon socket (or a TCP address,
// for db:connect). The CLI runs it over SSH: no port forwarding needed.
func dialStdioCmd() *cobra.Command {
	var socket, tcp string
	cmd := &cobra.Command{
		Use: "dial-stdio", Short: "Bridge stdio to the daemon socket", Hidden: true,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			network, addr := "unix", socket
			if tcp != "" {
				network, addr = "tcp", tcp
			}
			conn, err := net.Dial(network, addr)
			if err != nil {
				return err
			}
			defer conn.Close()
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = io.Copy(conn, os.Stdin)
				if cw, ok := conn.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
			}()
			_, _ = io.Copy(os.Stdout, conn)
			return nil
		},
	}
	cmd.Flags().StringVar(&socket, "socket", daemon.DefaultSocket, "daemon socket")
	cmd.Flags().StringVar(&tcp, "tcp", "", "connect to this TCP address instead")
	return cmd
}
