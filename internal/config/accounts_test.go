package config

import (
	"strings"
	"testing"
)

func TestUnknownAccountStrategyIsRejected(t *testing.T) {
	cfg := Config{Routing: RoutingConfig{Accounts: AccountRoutingConfig{Strategy: "shuffle"}}}
	err := cfg.Validate(func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "unknown account routing strategy") {
		t.Fatal(err)
	}
}

func TestEmptyAccountStrategyIsValidAsideFromOtherFields(t *testing.T) {
	cfg := Config{Routing: RoutingConfig{Accounts: AccountRoutingConfig{}}}
	err := cfg.Validate(func(string) (string, bool) { return "", false })
	if err != nil && strings.Contains(err.Error(), "unknown account routing strategy") {
		t.Fatal(err)
	}
}
