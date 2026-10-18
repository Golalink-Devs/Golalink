// Package voice adapts the standalone DisGo transport to Lavalink voice updates.
package voice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/gorilla/websocket"
	"github.com/thomas-vilte/dave-go/session"
	"golalink/internal/protocol"
)

type Closed struct {
	Code     int
	Reason   string
	ByRemote bool
}
type Connection interface {
	Write([]byte) error
	Speaking(context.Context, bool) error
	Connected() bool
	Ping() int64
	Close()
}
type Factory interface {
	Connect(context.Context, string, string, protocol.VoiceState, func(Closed)) (Connection, error)
}
type Discord struct{ Logger *slog.Logger }

func ValidID(raw string) bool { id, e := snowflake.Parse(raw); return e == nil && id != 0 }
func Validate(v protocol.VoiceState) error {
	if v.Token == "" || v.SessionID == "" || v.ChannelID == nil || !ValidID(*v.ChannelID) {
		return errors.New("voice requires token, sessionId, endpoint and channelId")
	}
	// Discord sends the voice host with an optional port, for example
	// "c-iad21-1a2b3c.discord.media:443". DisGo builds "wss://<endpoint>?v=4"
	// from this value, so a port is valid; userinfo, path, query and fragment
	// are not. The hostname rule below is what keeps non-Discord hosts out.
	u, e := url.Parse("wss://" + v.Endpoint)
	if e != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid Discord voice endpoint %q", v.Endpoint)
	}
	host := strings.ToLower(u.Hostname())
	if !strings.HasSuffix(host, ".discord.media") && !strings.HasSuffix(host, ".discord.gg") {
		return fmt.Errorf("voice endpoint %q must be a Discord voice hostname", v.Endpoint)
	}
	return nil
}

type discordConn struct {
	conn   voice.Conn
	closed atomic.Bool
	ready  atomic.Bool
	ping   atomic.Int64
	once   sync.Once
}

func (d Discord) Connect(ctx context.Context, user, guild string, v protocol.VoiceState, onClose func(Closed)) (Connection, error) {
	if err := Validate(v); err != nil {
		return nil, err
	}
	uid, e := snowflake.Parse(user)
	if e != nil {
		return nil, e
	}
	gid, e := snowflake.Parse(guild)
	if e != nil {
		return nil, e
	}
	channel, e := snowflake.Parse(*v.ChannelID)
	if e != nil {
		return nil, e
	}
	dc := &discordConn{}
	dc.ping.Store(-1)
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	stateUpdate := func(ctx context.Context, _ snowflake.ID, target *snowflake.ID, _ bool, _ bool) error {
		if target == nil {
			return nil
		}
		return dc.conn.Gateway().Open(ctx, voice.State{GuildID: gid, UserID: uid, ChannelID: target, SessionID: v.SessionID, Token: v.Token, Endpoint: v.Endpoint})
	}
	dc.conn = voice.NewConn(gid, uid, stateUpdate, func() { dc.ready.Store(false) }, voice.WithConnLogger(logger), voice.WithConnDaveSessionLogger(logger), voice.WithConnDaveSessionCreateFunc(session.CreateFunc()), voice.WithConnGatewayConfigOpts(voice.WithGatewayAutoReconnect(false)), voice.WithConnEventHandlerFunc(func(_ voice.Gateway, _ voice.Opcode, _ int, data voice.GatewayMessageData) {
		if ack, ok := data.(voice.GatewayMessageDataHeartbeatACK); ok {
			dc.ping.Store(max(0, time.Now().UnixMilli()-ack.T))
		}
	}), voice.WithConnGatewayCreateFunc(func(dave godave.Session, handler voice.EventHandlerFunc, closeHandler voice.CloseHandlerFunc, opts ...voice.GatewayConfigOpt) voice.Gateway {
		return voice.NewGateway(dave, handler, func(g voice.Gateway, err error, reconnect bool) {
			dc.ready.Store(false)
			if !dc.closed.Load() {
				event := Closed{Code: 4000, Reason: "voice connection closed", ByRemote: true}
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					event.Code = ce.Code
					event.Reason = ce.Text
				}
				onClose(event)
			}
			closeHandler(g, err, reconnect)
		}, opts...)
	}))
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := dc.conn.Open(ctx, channel, false, true); err != nil {
		dc.Close()
		return nil, err
	}
	dc.ready.Store(true)
	return dc, nil
}
func (d *discordConn) Write(b []byte) error {
	if !d.Connected() {
		return errors.New("voice is disconnected")
	}
	_ = d.conn.UDP().SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := d.conn.UDP().Write(b)
	return err
}
func (d *discordConn) Speaking(ctx context.Context, on bool) error {
	var f voice.SpeakingFlags
	if on {
		f = voice.SpeakingFlagMicrophone
	}
	return d.conn.SetSpeaking(ctx, f)
}
func (d *discordConn) Connected() bool {
	return !d.closed.Load() && d.ready.Load() && d.conn.Gateway().Status() == voice.StatusReady
}
func (d *discordConn) Ping() int64 {
	if !d.Connected() {
		return -1
	}
	return d.ping.Load()
}
func (d *discordConn) Close() {
	d.once.Do(func() {
		d.closed.Store(true)
		d.ready.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		d.conn.Close(ctx)
	})
}
