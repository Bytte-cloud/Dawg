package router

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	ws "github.com/gorilla/websocket"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment/qemu"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/router/tokens"
	wsocket "github.com/pterodactyl/wings/router/websocket"
)

// vncUpgrader mirrors the origin checking used by the console websocket so the
// VNC stream is only accepted from the Panel / configured origins.
var vncUpgrader = ws.Upgrader{
	// noVNC opens the socket with the "binary" subprotocol. Echo it back so the
	// 101 response acknowledges the requested subprotocol — strict clients and
	// proxies (notably Cloudflare) drop a WebSocket upgrade whose response does
	// not select a subprotocol the client offered, surfacing as a 1006 close.
	Subprotocols: []string{"binary"},
	// Negotiate permessage-deflate when the client asks for it. Chrome always
	// offers it; if the origin declines while a proxy in front (Cloudflare)
	// negotiates it with the browser, the compression state desyncs and the
	// stream corrupts — the browser reports it as an immediate 1006 close even
	// though the handshake returned 101. Agreeing keeps the chain consistent.
	EnableCompression: true,
	CheckOrigin: func(r *http.Request) bool {
		o := r.Header.Get("Origin")
		if o == config.Get().PanelLocation {
			return true
		}
		for _, origin := range config.Get().AllowedOrigins {
			if origin == "*" || origin == o {
				return true
			}
		}
		return false
	},
}

// getServerVNC proxies a websocket connection to a VM's VNC (RFB) graphical
// console. noVNC transmits RFB bytes immediately after the socket opens, so the
// JWT is supplied as a "token" query parameter and validated BEFORE the upgrade,
// unlike the text console websocket which authenticates via an in-band message.
func getServerVNC(c *gin.Context) {
	s := middleware.ExtractServer(c)

	token := c.Query("token")
	if token == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "A websocket token is required to access the graphical console."})
		return
	}

	payload, err := wsocket.NewTokenPayload([]byte(token))
	if err != nil || payload.GetServerUuid() != s.ID() {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "The provided token is not valid for this server."})
		return
	}

	// Only VM servers expose a graphical console. Return 404 (rather than a more
	// revealing status) for everything else, in addition to the type assertion
	// below — defense in depth against probing.
	if !s.IsVM() {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "This server does not have a graphical console."})
		return
	}

	env, ok := s.Environment.(*qemu.Environment)
	if !ok {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "This server does not have a graphical console."})
		return
	}

	addr, err := env.VNCAddress(c.Request.Context())
	if err != nil || addr == "" {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "The VM is not running or has no graphical console available."})
		return
	}

	tcp, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "Failed to reach the VM graphical console."})
		return
	}
	defer tcp.Close()

	conn, err := vncUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade writes its own error response on failure.
		return
	}
	defer conn.Close()

	proxyVNC(conn, tcp, payload)
}

// proxyVNC pumps bytes between the browser websocket (binary frames) and the VM
// VNC TCP connection. It tears everything down when either side closes, and also
// periodically re-checks the token denylist so that revoking a user's access
// terminates an in-progress session (the token's own expiry is intentionally not
// enforced mid-session, since a graphical session legitimately outlives the
// short token lifetime).
func proxyVNC(conn *ws.Conn, tcp net.Conn, payload *tokens.WebsocketPayload) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Close both connections as soon as the context is cancelled (by either pump
	// finishing or the denylist check firing), which unblocks the other pump.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
		_ = tcp.Close()
	}()

	// Revocation check: if the token becomes denylisted, drop the session.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if payload.Denylisted() {
					cancel()
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// VM -> browser.
	go func() {
		defer wg.Done()
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, err := tcp.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(ws.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Browser -> VM.
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt != ws.BinaryMessage && mt != ws.TextMessage {
				continue
			}
			if _, werr := tcp.Write(data); werr != nil {
				return
			}
		}
	}()

	wg.Wait()
}
