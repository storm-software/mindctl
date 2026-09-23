// Command mindctl runs the gateway core and its operational endpoints.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/storm-software/mindctl/internal/app"
	"github.com/storm-software/mindctl/internal/config"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mindctl:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	command := newRootCommand(ctx, stdout, stderr)
	command.SetArgs(normalizeLegacyConfigFlag(args))
	return command.ExecuteContext(ctx)
}

func normalizeLegacyConfigFlag(args []string) []string {
	normalized := make([]string, len(args))
	for index, arg := range args {
		switch {
		case arg == "-config":
			normalized[index] = "--config"
		case strings.HasPrefix(arg, "-config="):
			normalized[index] = "--" + arg[1:]
		default:
			normalized[index] = arg
		}
	}
	return normalized
}

func newRootCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	settings := viper.New()
	root := &cobra.Command{
		Use:           "mindctl",
		Short:         "Run the Mindctl gateway",
		Args:          noArgs("unexpected positional arguments"),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(*cobra.Command, []string) error {
			path := settings.GetString("config")
			settings.SetConfigFile(path)
			if err := settings.ReadInConfig(); err != nil {
				return fmt.Errorf("read config: %w", err)
			}
			return runGateway(ctx, settings.ConfigFileUsed(), stderr)
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().String("config", "config.example.yaml", "gateway YAML configuration")
	if err := settings.BindPFlag("config", root.PersistentFlags().Lookup("config")); err != nil {
		panic(fmt.Sprintf("bind config flag: %v", err))
	}
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  noArgs("unexpected arguments for version"),
		RunE: func(command *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(command.OutOrStdout(), "version=%s\ncommit=%s\ndate=%s\n", version, commit, date)
			return err
		},
	})
	return root
}

func noArgs(message string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		return errors.New(message)
	}
}

func runGateway(ctx context.Context, path string, stderr io.Writer) (err error) {
	cfg, err := config.Load(path, os.LookupEnv)
	if err != nil {
		return err
	}
	application, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := application.Close(); closeErr != nil {
			err = errors.Join(err, errors.New("close application resources failed"))
		}
	}()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("listen on configured address failed")
	}
	defer listener.Close()
	logger := log.New(stderr, "mindctl: ", 0)
	if cfg.SQLite.Retention == 0 {
		logger.Print("raw-content retention=unlimited; disk usage can grow without bound; review storage and governance requirements")
	} else {
		logger.Printf("raw-content retention=%s", cfg.SQLite.Retention)
	}
	logger.Printf("listening on %s", listener.Addr())
	server := &http.Server{
		Handler: application.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute,
		// HTTP internals can include peer-controlled data in diagnostics.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	return serve(ctx, server, listener, 5*time.Second)
}

// serve waits for both shutdown and Serve to finish before the app is closed.
func serve(ctx context.Context, server *http.Server, listener net.Listener, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	var serveErr error
	var terminated bool
	select {
	case serveErr = <-done:
		terminated = true
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		// Shutdown does not interrupt active connections when its deadline expires.
		_ = server.Close()
	}
	if !terminated {
		serveErr = <-done
	}
	var errs []error
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		errs = append(errs, errors.New("HTTP serve failed"))
	}
	if shutdownErr != nil {
		errs = append(errs, errors.New("HTTP graceful shutdown failed"))
	}
	return errors.Join(errs...)
}
