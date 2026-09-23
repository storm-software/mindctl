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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/storm-software/mindctl/internal/app"
	"github.com/storm-software/mindctl/internal/config"
	"gopkg.in/yaml.v3"
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
		RunE: func(command *cobra.Command, _ []string) error {
			path := settings.GetString("config")
			if !command.Flags().Changed("config") {
				candidate, err := userConfigPath()
				if err != nil {
					return err
				}
				if _, err := os.Stat(candidate); err == nil {
					path = candidate
				} else if !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("inspect user config: %w", err)
				}
			}
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
	root.AddCommand(newConfigCommand())
	return root
}

func newConfigCommand() *cobra.Command {
	configCommand := &cobra.Command{
		Use:   "config",
		Short: "Read and update the user configuration",
		Args:  noArgs("unexpected arguments for config"),
	}
	configCommand.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "Display the user configuration",
			Args:  noArgs("unexpected arguments for config list"),
			RunE: func(command *cobra.Command, _ []string) error {
				document, err := readUserConfig()
				if err != nil {
					return err
				}
				body, err := marshalConfig(document)
				if err != nil {
					return fmt.Errorf("format config: %w", err)
				}
				_, err = command.OutOrStdout().Write(body)
				return err
			},
		},
		&cobra.Command{
			Use:   "get <group>.<name>",
			Short: "Display one user configuration value",
			Args:  cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, args []string) error {
				document, err := readUserConfig()
				if err != nil {
					return err
				}
				value, err := configValue(document, args[0])
				if err != nil {
					return err
				}
				body, err := marshalConfig(value)
				if err != nil {
					return fmt.Errorf("format config value: %w", err)
				}
				_, err = command.OutOrStdout().Write(body)
				return err
			},
		},
		&cobra.Command{
			Use:   "set <group>.<name> <value>",
			Short: "Set one user configuration value",
			Args:  cobra.ExactArgs(2),
			RunE: func(_ *cobra.Command, args []string) error {
				path, err := userConfigPath()
				if err != nil {
					return err
				}
				document, err := readConfig(path)
				if err != nil {
					return err
				}
				value, err := parseConfigValue(args[1])
				if err != nil {
					return err
				}
				if err := setConfigValue(document, args[0], value); err != nil {
					return err
				}
				return writeConfig(path, document)
			},
		},
	)
	return configCommand
}

func userConfigPath() (string, error) {
	configHome, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(configHome, "mindctl", "config.yaml"), nil
}

func readUserConfig() (*yaml.Node, error) {
	path, err := userConfigPath()
	if err != nil {
		return nil, err
	}
	return readConfig(path)
}

func readConfig(path string) (*yaml.Node, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
		return nil, errors.New("decode config: multiple YAML documents are not allowed")
	}
	return &document, nil
}

func configValue(document *yaml.Node, dottedPath string) (*yaml.Node, error) {
	parts := strings.Split(dottedPath, ".")
	if len(parts) < 2 || strings.Contains(dottedPath, "..") || strings.HasPrefix(dottedPath, ".") || strings.HasSuffix(dottedPath, ".") {
		return nil, errors.New("config key must use group.name form")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config root must be a mapping")
	}
	current := document.Content[0]
	for _, part := range parts {
		if current.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("config key %q is not a group", strings.Join(parts[:len(parts)-1], "."))
		}
		var next *yaml.Node
		for index := 0; index < len(current.Content); index += 2 {
			if current.Content[index].Value == part {
				next = current.Content[index+1]
				break
			}
		}
		if next == nil {
			return nil, fmt.Errorf("config key %q was not found", dottedPath)
		}
		current = next
	}
	return current, nil
}

func parseConfigValue(raw string) (*yaml.Node, error) {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return nil, fmt.Errorf("parse config value: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.ScalarNode {
		return nil, errors.New("config value must be a YAML scalar")
	}
	return document.Content[0], nil
}

func setConfigValue(document *yaml.Node, dottedPath string, value *yaml.Node) error {
	parts := strings.Split(dottedPath, ".")
	if len(parts) < 2 || strings.Contains(dottedPath, "..") || strings.HasPrefix(dottedPath, ".") || strings.HasSuffix(dottedPath, ".") {
		return errors.New("config key must use group.name form")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return errors.New("config root must be a mapping")
	}
	current := document.Content[0]
	for _, part := range parts[:len(parts)-1] {
		var next *yaml.Node
		for index := 0; index < len(current.Content); index += 2 {
			if current.Content[index].Value == part {
				next = current.Content[index+1]
				break
			}
		}
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			current.Content = append(current.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, next)
		}
		if next.Kind != yaml.MappingNode {
			return fmt.Errorf("config key %q is not a group", part)
		}
		current = next
	}
	name := parts[len(parts)-1]
	for index := 0; index < len(current.Content); index += 2 {
		if current.Content[index].Value == name {
			current.Content[index+1] = value
			return nil
		}
	}
	current.Content = append(current.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, value)
	return nil
}

func writeConfig(path string, document *yaml.Node) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect config: %w", err)
	}
	body, err := marshalConfig(document)
	if err != nil {
		return fmt.Errorf("format config: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary config permissions: %w", err)
	}
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func marshalConfig(value any) ([]byte, error) {
	var body strings.Builder
	encoder := yaml.NewEncoder(&body)
	encoder.SetIndent(2)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return []byte(body.String()), nil
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
