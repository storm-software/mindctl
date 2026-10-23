package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/storm-software/mindctl/internal/accounts"
	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/discover"
	"github.com/storm-software/mindctl/internal/oauth"
)

func newLoginCommand() *cobra.Command {
	var noBrowser bool
	var port int
	var authDir string
	command := &cobra.Command{
		Use:   "login <provider>",
		Short: "Store a local OAuth login in the auth directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			dir, err := resolveAuthDir(authDir)
			if err != nil {
				return err
			}
			path, err := oauth.Login(command.Context(), args[0], oauth.Options{
				NoBrowser: noBrowser, Port: port, AuthDir: dir, Stdout: command.OutOrStdout(),
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "saved %s\n", path)
			return nil
		},
	}
	command.Flags().BoolVar(&noBrowser, "no-browser", false, "print the login URL without opening a browser")
	command.Flags().IntVar(&port, "oauth-callback-port", 0, "localhost port for the OAuth callback")
	command.Flags().StringVar(&authDir, "auth-dir", "", "directory for local account files")
	return command
}

func newCredentialsCommand() *cobra.Command {
	var authDir string
	command := &cobra.Command{
		Use:   "credentials",
		Short: "List and edit local account files",
	}
	command.PersistentFlags().StringVar(&authDir, "auth-dir", "", "directory for local account files")
	command.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List local accounts without secrets",
			Args:  noArgs("unexpected arguments for credentials list"),
			RunE: func(command *cobra.Command, _ []string) error {
				pool, err := openAuthPool(authDir)
				if err != nil {
					return err
				}
				writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(writer, "ID\tPROVIDER\tENABLED")
				for _, account := range pool.Accounts() {
					if account.Configured {
						continue
					}
					fmt.Fprintf(writer, "%s\t%s\t%t\n", account.ID, account.Provider, account.Enabled)
				}
				return writer.Flush()
			},
		},
		credentialToggle("enable", true),
		credentialToggle("disable", false),
		&cobra.Command{
			Use:   "refresh <id>",
			Short: "Refresh one local account token",
			Args:  cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, args []string) error {
				dir, err := resolveAuthDir(authDir)
				if err != nil {
					return err
				}
				pool, err := accounts.NewPool(accounts.Settings{}, dir, nil)
				if err != nil {
					return err
				}
				account, secret, ok := pool.Lookup(args[0])
				if !ok {
					return fmt.Errorf("unknown account %s", args[0])
				}
				if account.Kind == accounts.KindServiceAccount {
					token, tokenErr := oauth.GoogleAccessToken(command.Context(), oauth.ServiceAccount{
						ClientEmail: secret.ClientEmail, PrivateKey: secret.PrivateKey, TokenURI: secret.TokenURI,
					})
					if tokenErr != nil {
						return tokenErr
					}
					secret.AccessToken = token
				} else {
					access, refresh, refreshErr := oauth.Refresh(command.Context(), nil, refreshLogin(account.Provider), secret.RefreshToken)
					if refreshErr != nil {
						return refreshErr
					}
					secret.AccessToken = access
					secret.RefreshToken = refresh
				}
				_, err = accounts.WriteAuthFile(dir, account, secret)
				return err
			},
		},
	)
	return command
}

func credentialToggle(use string, enabled bool) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <id>",
		Short: use + " one local account",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			dir, _ := command.Flags().GetString("auth-dir")
			pool, err := openAuthPool(dir)
			if err != nil {
				return err
			}
			return pool.SetEnabled(args[0], enabled)
		},
	}
}

func newVertexCommand() *cobra.Command {
	command := &cobra.Command{Use: "vertex", Short: "Import Vertex credentials"}
	var authDir, prefix string
	importCommand := &cobra.Command{
		Use:   "import <service-account.json>",
		Short: "Import a Vertex service-account JSON key",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			dir, err := resolveAuthDir(authDir)
			if err != nil {
				return err
			}
			path, err := oauth.ImportVertex(dir, args[0], prefix)
			if err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "saved %s\n", path)
			return nil
		},
	}
	importCommand.Flags().StringVar(&authDir, "auth-dir", "", "directory for local account files")
	importCommand.Flags().StringVar(&prefix, "prefix", "", "model prefix this service account can serve")
	command.AddCommand(importCommand)
	return command
}

func newDiscoverCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "discover",
		Short: "Find other gateways on the local network",
		Args:  noArgs("unexpected arguments for discover"),
		RunE: func(command *cobra.Command, _ []string) error {
			found, err := discover.Query(command.Context())
			if err != nil {
				return err
			}
			if len(found) == 0 {
				fmt.Fprintln(command.OutOrStdout(), "no gateways")
				return nil
			}
			for _, gateway := range found {
				fmt.Fprintln(command.OutOrStdout(), discover.Format(gateway))
			}
			return nil
		},
	}
}

func resolveAuthDir(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	return config.DefaultAuthDir()
}

func openAuthPool(authDir string) (*accounts.Pool, error) {
	dir, err := resolveAuthDir(authDir)
	if err != nil {
		return nil, err
	}
	return accounts.NewPool(accounts.Settings{}, dir, nil)
}

func refreshLogin(providerID string) string {
	switch providerID {
	case "openai":
		return "codex"
	case "anthropic":
		return "claude"
	default:
		return providerID
	}
}
