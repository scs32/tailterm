// Command tailterm-discord is the Discord bridge for the Tailterm hub
// (broker phase 2b, docs/broker-phase-2b.md). It runs beside the hub on
// TrueNAS, reaches the hub with a route-limited bridge token and opens only
// outbound connections to Discord.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/bridge"
	"github.com/scs32/tailterm/hub/internal/discord"
)

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func secret(key string) string {
	path := os.Getenv(key)
	if path == "" {
		log.Fatalf("%s is required", key)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", key, err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		log.Fatalf("%s is empty", key)
	}
	return value
}

func main() {
	hub, err := api.NewClient(env("TAILTERM_HUB_URL", "http://tailterm-hub:18765"), 30*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	hub.Token = secret("TAILTERM_BRIDGE_TOKEN_FILE")
	token := secret("DISCORD_TOKEN_FILE")
	guild := env("DISCORD_GUILD_ID", "")
	var owners []string
	for _, id := range strings.Split(env("DISCORD_OWNER_IDS", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			owners = append(owners, id)
		}
	}
	stateDir := env("TAILTERM_BRIDGE_STATE", "/state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		log.Fatalf("state dir: %v", err)
	}
	state, err := bridge.OpenState(filepath.Join(stateDir, "bridge.sqlite"))
	if err != nil {
		log.Fatalf("open state: %v", err)
	}
	defer state.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	who, err := hub.Whoami(ctx)
	if err != nil {
		log.Fatalf("hub: %v", err)
	}
	if who.Node != api.BridgeNode {
		log.Fatalf("the hub token is not a bridge token (node %q); refusing to run with wider access", who.Node)
	}
	client := &discord.Client{Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
	app := env("DISCORD_APPLICATION_ID", "")
	if app == "" {
		me, err := client.CurrentUser(ctx)
		if err != nil {
			log.Fatalf("discord: %v", err)
		}
		app = me.ID // a bot's user ID is its application ID
	}
	b, err := bridge.New(bridge.Config{
		Hub: hub, Discord: client,
		Gateway: &discord.Gateway{
			Token:   token,
			Intents: discord.IntentGuilds | discord.IntentGuildMessages | discord.IntentMessageContent,
			URL:     client.GatewayURL,
			Log:     log.Printf,
		},
		AppID: app, GuildID: guild, Owners: owners,
		ActiveCategory:  env("DISCORD_ACTIVE_CATEGORY", "Tailterm projects"),
		ArchiveCategory: env("DISCORD_ARCHIVE_CATEGORY", "Tailterm archive"),
		TailOSURL:       env("TAILOS_URL", ""),
		State:           state,
		Log:             log.Printf,
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("discord bridge starting for guild %s with %d owner(s)", guild, len(owners))
	if err := b.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
