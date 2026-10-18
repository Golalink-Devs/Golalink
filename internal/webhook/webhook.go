// Package webhook delivers node events to an external endpoint.
//
// A Discord webhook URL receives an embed, which is what a Discord channel
// expects; any other endpoint receives the event itself as JSON. Delivery runs
// on one background worker so a slow endpoint never blocks the player that
// produced the event, and a delivery that cannot be completed stops the node:
// a configured endpoint is part of the deployment, not an optional extra.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golalink/internal/config"
	"golalink/internal/logging"
	"golalink/internal/protocol"
)

const (
	// queueSize bounds how many events may wait for delivery. An endpoint that
	// cannot keep up is a failure like any other, so a full queue is fatal
	// rather than a silent drop.
	queueSize = 128
	// sendTimeout bounds one delivery attempt.
	sendTimeout = 10 * time.Second
	// PlayerStateType marks the periodic node-wide report of every player on the
	// node. It is not a Lavalink event, so it is rendered as its own embed
	// rather than as a track event.
	PlayerStateType = "PlayerStateUpdate"
	// maxEmbedFields is the number of fields Discord renders in one embed.
	maxEmbedFields = 25
)

// Client posts events to a single URL.
type Client struct {
	url     string
	discord bool
	http    *http.Client
	logger  *slog.Logger
	events  chan []byte
}

// New returns a client for the webhook configuration, or nil when the webhook is
// off.
func New(cfg config.Webhook, logger *slog.Logger) *Client {
	target := strings.TrimSpace(cfg.URL)
	if !cfg.Enable || target == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	c := &Client{
		url:     target,
		discord: isDiscord(target),
		http:    &http.Client{Timeout: sendTimeout},
		logger:  logger,
		events:  make(chan []byte, queueSize),
	}
	go c.run()
	return c
}

// Send queues an event for delivery. It returns immediately; the payload is
// encoded here so the caller's event cannot change underneath the worker. A
// node with no webhook has a nil client, and every call on it is ignored, so a
// caller never has to check whether one is configured.
func (c *Client) Send(v any) error {
	if c == nil {
		return nil
	}
	body, err := json.Marshal(v)
	if err != nil {
		c.fatal("webhook event could not be encoded", err)
		return err
	}
	select {
	case c.events <- body:
	default:
		c.fatal("webhook is not keeping up; event queue is full", errors.New("webhook queue full"))
	}
	return nil
}

func (c *Client) run() {
	for body := range c.events {
		if err := c.post(body); err != nil {
			c.fatal("webhook delivery failed", err)
			return
		}
	}
}

// post delivers one event. A Discord endpoint gets an embed built from the
// event; every other endpoint gets the event as it was published.
func (c *Client) post(body []byte) error {
	if c.discord {
		var event protocol.Object
		if err := json.Unmarshal(body, &event); err != nil {
			return fmt.Errorf("decode event: %w", err)
		}
		embedded, err := json.Marshal(discordPayload(event))
		if err != nil {
			return fmt.Errorf("encode embed: %w", err)
		}
		body = embedded
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Golalink")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook answered %s", resp.Status)
	}
	return nil
}

// fatal reports a delivery the node cannot recover from and stops the process.
// The endpoint is configuration, so a node that cannot reach it was told to
// talk to something that is not listening.
func (c *Client) fatal(message string, err error) {
	c.logger.Log(context.Background(), logging.LevelFatal, message, "url", c.url, "error", err)
	os.Exit(1)
}

// isDiscord reports whether a URL points at a Discord webhook, which is served
// an embed instead of the raw event.
func isDiscord(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "discord.com" || host == "discordapp.com" || strings.HasSuffix(host, ".discord.com") {
		return true
	}
	return strings.Contains(u.Path, "/api/webhooks/")
}

// Embed colours, chosen so a track that started reads calm and a failure that
// stopped playback reads as a fault.
const (
	colorStart   = 0x57f287 // green
	colorEnd     = 0x95a5a6 // gray
	colorFault   = 0xed4245 // red
	colorDefault = 0x5865f2 // blurple
	colorState   = 0x5865f2 // blurple
)

// discordPayload renders an event as the {"embeds": [...]} body a Discord
// webhook accepts. The track is the headline, and the remaining fields carry
// what an operator needs to tell one occurrence from another.
func discordPayload(event protocol.Object) protocol.Object {
	kind, _ := event["type"].(string)
	if kind == PlayerStateType {
		return playerStatePayload(event)
	}
	embed := protocol.Object{
		"title":     eventTitle(kind),
		"color":     eventColor(kind),
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"footer":    protocol.Object{"text": "Golalink"},
	}
	var fields []protocol.Object
	add := func(name string, value any) {
		text := embedValue(value)
		if text == "" {
			return
		}
		fields = append(fields, protocol.Object{"name": name, "value": truncate(text, 1024), "inline": true})
	}
	if track, ok := event["track"].(map[string]any); ok {
		info, _ := track["info"].(map[string]any)
		if title := embedValue(info["title"]); title != "" {
			description := "**" + title + "**"
			if author := embedValue(info["author"]); author != "" {
				description += " — " + author
			}
			embed["description"] = truncate(description, 4096)
		}
		add("Source", info["sourceName"])
		add("Guild", event["guildId"])
		add("URI", info["uri"])
		if length, ok := number(info["length"]); ok && length > 0 {
			add("Duration", formatDuration(time.Duration(length)*time.Millisecond))
		}
	} else {
		// An event with no track is about the player itself: which guild it
		// serves and, for the voice events, which channel the bot was on.
		add("Guild", event["guildId"])
		add("Channel", event["channelId"])
	}
	add("Reason", event["reason"])
	add("Code", event["code"])
	add("By remote", event["byRemote"])
	if threshold, ok := number(event["thresholdMs"]); ok && threshold > 0 {
		add("Threshold", fmt.Sprintf("%.0f ms", threshold))
	}
	if exception, ok := event["exception"].(map[string]any); ok {
		add("Exception", exception["message"])
		add("Cause", exception["cause"])
	}
	if len(fields) > 0 {
		embed["fields"] = fields
	}
	return protocol.Object{"embeds": []protocol.Object{embed}}
}

// playerStatePayload renders the node-wide player report as an embed. The
// headline says how busy the node is and each player becomes a field, so a
// channel shows what the node is playing without opening a client.
func playerStatePayload(report protocol.Object) protocol.Object {
	embed := protocol.Object{
		"title":     "Player State",
		"color":     colorState,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"footer":    protocol.Object{"text": "Golalink"},
	}
	embed["description"] = fmt.Sprintf("%s · %s playing · %s",
		plural(embedValue(report["players"]), "player"),
		embedValue(report["playingPlayers"]),
		plural(embedValue(report["sessions"]), "session"))
	if states, ok := report["states"].([]any); ok {
		if fields := playerStateFields(states); len(fields) > 0 {
			embed["fields"] = fields
		}
	}
	return protocol.Object{"embeds": []protocol.Object{embed}}
}

// playerStateFields renders one field per player. Discord caps an embed at
// maxEmbedFields, so a busier node gets a final field counting the rest instead
// of a body Discord would reject.
func playerStateFields(states []any) []protocol.Object {
	fields := make([]protocol.Object, 0, len(states))
	for i, raw := range states {
		if len(fields) >= maxEmbedFields-1 {
			fields = append(fields, protocol.Object{
				"name":   "…",
				"value":  fmt.Sprintf("%d more players", len(states)-i),
				"inline": false,
			})
			break
		}
		state, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fields = append(fields, protocol.Object{
			"name":   truncate(playerStateName(state), 256),
			"value":  truncate(playerStateValue(state), 1024),
			"inline": false,
		})
	}
	return fields
}

// playerStateName heads a player field with the guild it serves and what it is
// playing, which is what an operator scans a channel for.
func playerStateName(state map[string]any) string {
	name := embedValue(state["guildId"])
	if title := embedValue(state["title"]); title != "" {
		name += " · " + title
	}
	if strings.TrimSpace(name) == "" {
		return "Player"
	}
	return name
}

// playerStateValue describes one player: where its track is, whether it is
// playing, how loud it is and how it reaches the voice channel. Who owns the
// player is deliberately absent, because the report is readable by anyone the
// endpoint is shared with.
func playerStateValue(state map[string]any) string {
	var parts []string
	if embedValue(state["title"]) == "" {
		parts = append(parts, "idle")
	} else {
		elapsed := ""
		if position, ok := number(state["position"]); ok {
			elapsed = formatDuration(time.Duration(position) * time.Millisecond)
		}
		if length, ok := number(state["length"]); ok && length > 0 {
			elapsed += "/" + formatDuration(time.Duration(length)*time.Millisecond)
		}
		if elapsed != "" {
			parts = append(parts, elapsed)
		}
		if paused, ok := state["paused"].(bool); ok {
			if paused {
				parts = append(parts, "paused")
			} else {
				parts = append(parts, "playing")
			}
		}
	}
	if volume := embedValue(state["volume"]); volume != "" {
		parts = append(parts, "vol "+volume)
	}
	if connected, ok := state["connected"].(bool); ok && !connected {
		parts = append(parts, "disconnected")
	}
	if ping, ok := number(state["ping"]); ok && ping >= 0 {
		parts = append(parts, fmt.Sprintf("ping %d", int64(ping)))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " · ")
}

// plural renders a count with its noun, keeping the singular when the count is
// one so an embed does not read "1 players".
func plural(count, noun string) string {
	if count == "1" {
		return count + " " + noun
	}
	return count + " " + noun + "s"
}

// number reads a value that JSON decoded as a number.
func number(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// eventTitle turns a Lavalink event name into the heading a Discord embed shows.
func eventTitle(kind string) string {
	switch kind {
	case "TrackStartEvent":
		return "Track Started"
	case "TrackEndEvent":
		return "Track Ended"
	case "TrackExceptionEvent":
		return "Track Exception"
	case "TrackStuckEvent":
		return "Track Stuck"
	case "WebSocketClosedEvent":
		return "Voice WebSocket Closed"
	case "BotConnectEvent":
		return "Bot Joined Voice"
	case "BotDisconnectEvent":
		return "Bot Left Voice"
	case "":
		return "Golalink Event"
	default:
		return kind
	}
}

// eventColor picks the embed accent for an event.
func eventColor(kind string) int {
	switch kind {
	case "TrackStartEvent", "BotConnectEvent":
		return colorStart
	case "TrackEndEvent", "BotDisconnectEvent":
		return colorEnd
	case "TrackExceptionEvent", "TrackStuckEvent":
		return colorFault
	default:
		return colorDefault
	}
}

// embedValue renders a value decoded from JSON as the string an embed field
// needs, and returns the empty string for anything an embed cannot show.
func embedValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

// truncate keeps a value inside the length Discord enforces, counting runes so
// a CJK title is not cut in the middle of a character.
func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	if limit <= 1 {
		return string(r[:limit])
	}
	return string(r[:limit-1]) + "…"
}

// formatDuration renders a track length as m:ss or h:mm:ss.
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
