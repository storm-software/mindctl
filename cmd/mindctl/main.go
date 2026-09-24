// Command mindctl runs the gateway core and its operational endpoints.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/storm-software/mindctl/internal/app"
	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/storage"
	"github.com/storm-software/mindctl/internal/storage/sqlite"
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
	var debug bool
	root := &cobra.Command{
		Use:           "mindctl",
		Short:         "Run the Mindctl gateway",
		Args:          noArgs("unexpected positional arguments"),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, _ []string) error {
			path, err := selectedConfigPath(command, settings)
			if err != nil {
				return err
			}
			settings.SetConfigFile(path)
			if err := settings.ReadInConfig(); err != nil {
				return fmt.Errorf("read config: %w", err)
			}
			return runGateway(
				ctx,
				settings.ConfigFileUsed(),
				stderr,
				command.Flags().Changed("debug"),
				debug,
			)
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().String("config", "config.example.yaml", "gateway YAML configuration")
	root.PersistentFlags().BoolVar(&debug, "debug", false, "write detailed router traces to the user cache directory")
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
	root.AddCommand(
		newModelCommand(settings),
		newProviderCommand(settings),
		newHistoryCommand(settings),
	)
	return root
}

func newHistoryCommand(settings *viper.Viper) *cobra.Command {
	var filter storage.HistoryFilter
	var since, until string
	command := &cobra.Command{
		Use:   "history",
		Short: "List processed requests, selected models, and responses",
		Args:  noArgs("unexpected arguments for history"),
		RunE: func(command *cobra.Command, _ []string) error {
			var err error
			filter.Since, err = parseHistoryTime("since", since)
			if err != nil {
				return err
			}
			filter.Until, err = parseHistoryTime("until", until)
			if err != nil {
				return err
			}
			if filter.Limit < 0 {
				return errors.New("limit must be nonnegative")
			}
			if !filter.Since.IsZero() && !filter.Until.IsZero() && filter.Since.After(filter.Until) {
				return errors.New("since must not be after until")
			}
			switch filter.Status {
			case "", "started", "succeeded", "failed":
			default:
				return errors.New("status must be started, succeeded, or failed")
			}

			cfg, err := readHistoryConfig(command, settings)
			if err != nil {
				return err
			}
			db, err := openHistoryStorage(command.Context(), cfg, os.LookupEnv)
			if err != nil {
				return err
			}
			defer db.Close()

			records, err := db.ListHistory(command.Context(), filter)
			if err != nil {
				return err
			}
			return writeHistory(command.OutOrStdout(), records)
		},
	}
	command.Flags().StringVar(&filter.Provider, "provider", "", "filter by selected provider")
	command.Flags().StringVar(&filter.ModelID, "model", "", "filter by selected model")
	command.Flags().StringVar(&filter.Status, "status", "", "filter by attempt status")
	command.Flags().StringVar(&since, "since", "", "include requests at or after an RFC3339 timestamp")
	command.Flags().StringVar(&until, "until", "", "include requests at or before an RFC3339 timestamp")
	command.Flags().IntVar(&filter.Limit, "limit", 0, "maximum requests to return (0 means all)")
	return command
}

func parseHistoryTime(name, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse --%s as RFC3339: %w", name, err)
	}
	return parsed, nil
}

func readHistoryConfig(command *cobra.Command, settings *viper.Viper) (config.Config, error) {
	path, err := selectedConfigPath(command, settings)
	if err != nil {
		return config.Config{}, err
	}
	return config.Read(path)
}

func openHistoryStorage(
	ctx context.Context,
	cfg config.Config,
	getenv func(string) (string, bool),
) (*sqlite.DB, error) {
	keys := make(map[string][]byte, len(cfg.Encryption.Keys))
	defer func() {
		for _, key := range keys {
			clear(key)
		}
	}()
	for id, environmentName := range cfg.Encryption.Keys {
		value, exists := getenv(environmentName)
		if !exists || value == "" {
			return nil, fmt.Errorf("encryption key environment variable %s is not set", environmentName)
		}
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(decoded) != 32 {
			return nil, errors.New("invalid encryption key")
		}
		keys[id] = decoded
	}
	keyring, err := contentcrypto.New(cfg.Encryption.ActiveKeyID, keys)
	if err != nil {
		return nil, err
	}
	return sqlite.Open(ctx, sqlite.Options{Path: cfg.SQLite.Path, Keyring: keyring})
}

func writeHistory(output io.Writer, records []storage.HistoryRecord) error {
	for index, record := range records {
		if index > 0 {
			if _, err := fmt.Fprintln(output); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(
			output,
			"%s  %s  %s\n",
			record.CreatedAt.Format(time.RFC3339),
			record.ResponseID,
			record.Status,
		); err != nil {
			return err
		}
		if !record.RequestContentRetained {
			if _, err := fmt.Fprintln(output, "  request: [not retained]"); err != nil {
				return err
			}
		} else if err := writeHistoryItems(output, "request", record.Request); err != nil {
			return err
		}
		for _, attempt := range record.Attempts {
			if _, err := fmt.Fprintf(
				output,
				"  attempt: %s/%s (%s, %s)\n",
				attempt.Provider,
				attempt.ModelID,
				attempt.Tier,
				attempt.Status,
			); err != nil {
				return err
			}
			switch {
			case attempt.Status == "started":
				if _, err := fmt.Fprintln(output, "  response: [pending]"); err != nil {
					return err
				}
			case !attempt.ContentRetained:
				if _, err := fmt.Fprintln(output, "  response: [not retained]"); err != nil {
					return err
				}
			case attempt.Result != nil:
				if err := writeHistoryItems(output, "response", attempt.Result.Output); err != nil {
					return err
				}
			default:
				if err := writeIndentedHistoryValue(output, "error", string(attempt.Error)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func writeHistoryItems(output io.Writer, label string, items []inference.Item) error {
	if len(items) == 0 {
		_, err := fmt.Fprintf(output, "  %s: [empty]\n", label)
		return err
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		value := item.Text
		if value == "" {
			encoded, err := json.Marshal(item)
			if err != nil {
				return fmt.Errorf("encode history %s: %w", label, err)
			}
			value = string(encoded)
		}
		values = append(values, value)
	}
	return writeIndentedHistoryValue(output, label, strings.Join(values, "\n"))
}

func writeIndentedHistoryValue(output io.Writer, label, value string) error {
	if _, err := fmt.Fprintf(output, "  %s:\n", label); err != nil {
		return err
	}
	for line := range strings.SplitSeq(value, "\n") {
		if _, err := fmt.Fprintf(output, "    %s\n", line); err != nil {
			return err
		}
	}
	return nil
}

func selectedConfigPath(command *cobra.Command, settings *viper.Viper) (string, error) {
	path := settings.GetString("config")
	if command.Root().PersistentFlags().Changed("config") {
		return path, nil
	}
	candidate, err := userConfigPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect user config: %w", err)
	}
	return path, nil
}

func newModelCommand(settings *viper.Viper) *cobra.Command {
	modelCommand := &cobra.Command{
		Use:   "model",
		Short: "List and update routable models",
		Args:  noArgs("unexpected arguments for model"),
	}
	var all bool
	listCommand := &cobra.Command{
		Use:   "list",
		Short: "List enabled models grouped by provider",
		Args:  noArgs("unexpected arguments for model list"),
		RunE: func(command *cobra.Command, _ []string) error {
			cfg, err := readCommandCatalog(command, settings)
			if err != nil {
				return err
			}
			return writeModelList(command.OutOrStdout(), cfg, all)
		},
	}
	listCommand.Flags().BoolVar(&all, "all", false, "display enabled and disabled models")
	modelCommand.AddCommand(
		listCommand,
		newModelToggleCommand(settings, "enable", true),
		newModelToggleCommand(settings, "disable", false),
	)
	return modelCommand
}

func newModelToggleCommand(settings *viper.Viper, action string, enabled bool) *cobra.Command {
	return &cobra.Command{
		Use:   action + " <provider[.model]>",
		Short: action + " one model or every model for a provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			cfg, err := readCatalog(command, settings)
			if err != nil {
				return err
			}
			statePath, err := config.ProvidersPath()
			if err != nil {
				return err
			}
			providerID, modelID, hasModel := strings.Cut(args[0], ".")
			if providerID == "" || (hasModel && modelID == "") {
				return errors.New("model target must use provider or provider.model form")
			}
			if hasModel {
				return config.SetModelEnabled(statePath, cfg, providerID, modelID, enabled)
			}
			return config.SetProviderEnabled(statePath, cfg, providerID, enabled)
		},
	}
}

func newProviderListCommand(settings *viper.Viper) *cobra.Command {
	var all bool
	listCommand := &cobra.Command{
		Use:   "list",
		Short: "List enabled providers",
		Args:  noArgs("unexpected arguments for provider list"),
		RunE: func(command *cobra.Command, _ []string) error {
			cfg, err := readCommandCatalog(command, settings)
			if err != nil {
				return err
			}
			return writeProviderList(command.OutOrStdout(), cfg, all)
		},
	}
	listCommand.Flags().BoolVar(&all, "all", false, "display enabled and disabled providers")
	return listCommand
}

func newProviderCommand(settings *viper.Viper) *cobra.Command {
	providerCommand := &cobra.Command{
		Use:   "provider",
		Short: "Enable or disable all models for a provider",
		Args:  noArgs("unexpected arguments for provider"),
	}
	providerCommand.AddCommand(newProviderListCommand(settings))
	for _, option := range []struct {
		action  string
		enabled bool
	}{{"enable", true}, {"disable", false}} {
		action, enabled := option.action, option.enabled
		providerCommand.AddCommand(&cobra.Command{
			Use:   action + " <provider>",
			Short: action + " every model for a provider",
			Args:  cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, args []string) error {
				cfg, err := readCatalog(command, settings)
				if err != nil {
					return err
				}
				statePath, err := config.ProvidersPath()
				if err != nil {
					return err
				}
				return config.SetProviderEnabled(statePath, cfg, args[0], enabled)
			},
		})
	}
	return providerCommand
}

func readCatalog(command *cobra.Command, settings *viper.Viper) (config.Config, error) {
	path, err := selectedConfigPath(command, settings)
	if err != nil {
		return config.Config{}, err
	}
	cfg, err := config.Read(path)
	if err != nil {
		return config.Config{}, err
	}
	if err := cfg.ValidateCatalog(); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func readCommandCatalog(command *cobra.Command, settings *viper.Viper) (config.Config, error) {
	cfg, err := readCatalog(command, settings)
	if err != nil {
		return config.Config{}, err
	}
	statePath, err := config.ProvidersPath()
	if err != nil {
		return config.Config{}, err
	}
	if err := config.ApplyProvidersFile(statePath, &cfg); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func writeModelList(output io.Writer, cfg config.Config, all bool) error {
	for _, provider := range cfg.Providers {
		models := make([]config.ModelConfig, 0)
		for _, model := range cfg.Models {
			if model.Provider == provider.ID && (all || model.Available) {
				models = append(models, model)
			}
		}
		if len(models) == 0 {
			continue
		}
		if _, err := fmt.Fprintln(output, provider.ID); err != nil {
			return err
		}
		for _, model := range models {
			status := ""
			if all {
				status = " (disabled)"
				if model.Available {
					status = " (enabled)"
				}
			}
			if _, err := fmt.Fprintf(output, "  %s%s\n", model.ID, status); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeProviderList(output io.Writer, cfg config.Config, all bool) error {
	for _, provider := range cfg.Providers {
		enabled := false
		for _, model := range cfg.Models {
			if model.Provider == provider.ID && model.Available {
				enabled = true
				break
			}
		}
		if !all && !enabled {
			continue
		}
		status := ""
		if all {
			status = " (disabled)"
			if enabled {
				status = " (enabled)"
			}
		}
		if _, err := fmt.Fprintf(output, "%s%s\n", provider.ID, status); err != nil {
			return err
		}
	}
	return nil
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

func runGateway(ctx context.Context, path string, stderr io.Writer, debugChanged, debugFlag bool) (err error) {
	cfg, err := loadGatewayConfig(path)
	if err != nil {
		return err
	}
	cfg.Debug, err = resolveDebug(cfg.Debug, debugChanged, debugFlag, os.LookupEnv)
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

func resolveDebug(
	configured bool,
	flagChanged bool,
	flagValue bool,
	getenv func(string) (string, bool),
) (bool, error) {
	if flagChanged {
		return flagValue, nil
	}
	value, exists := getenv("MINDCTL_DEBUG")
	if !exists {
		return configured, nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("parse MINDCTL_DEBUG: %w", err)
	}
	return enabled, nil
}

func loadGatewayConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path, os.LookupEnv)
	if err != nil {
		return config.Config{}, err
	}
	statePath, err := config.ProvidersPath()
	if err != nil {
		return config.Config{}, err
	}
	if err := config.ApplyProvidersFile(statePath, &cfg); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
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
