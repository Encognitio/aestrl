/**
 * Aestrl Cloudflare Coordination Worker & Signaling Durable Object
 *
 * Responsibilities:
 * 1. Durable Object-based WebRTC signaling relay across all Worker isolates.
 * 2. Ephemeral discovery records & session metadata.
 * 3. Static WebUI delivery for browser peer connections.
 * 4. Hosted-AI HTTP redirect handling (HTTP 307 to direct host IP/port).
 *
 * CRITICAL SECURITY INVARIANT:
 * File inspection payloads and contents NEVER pass through this Worker or Durable Object.
 * Cloudflare only handles SDP offer/answer coordination and static UI assets.
 * Established P2P sessions continue functioning completely independently of signaling availability.
 */

// Local fallback maps when SIGNALING_DO is not bound (e.g., local tests or standard workers)
const fallbackSessions = new Map();
const fallbackHostSockets = new Map();
const fallbackPendingSignals = new Map();

/**
 * SignalingDO provides globally consistent, isolate-independent WebRTC signaling.
 * Each Aestrl session gets an isolated Durable Object instance.
 */
export class SignalingDO {
  constructor(ctx, env) {
    this.ctx = ctx;
    this.env = env;
    this.sessionMeta = null;
    this.hostSocket = null;
    this.pendingSignals = new Map();
  }

  async fetch(request) {
    const url = new URL(request.url);

    // 1. Session registration
    if (request.method === "POST" && url.pathname.endsWith("/register")) {
      try {
        const data = await request.json();
        this.sessionMeta = {
          session_id: data.session_id,
          name: data.name || data.session_id,
          secret_hash: data.secret_hash,
          expires_at: data.expires_at,
          direct_url: data.direct_url,
          created_at: new Date().toISOString(),
        };
        if (this.ctx.storage) {
          await this.ctx.storage.put("meta", this.sessionMeta);
        }
        return jsonResponse({ status: "registered", session_id: data.session_id });
      } catch (e) {
        return new Response("Invalid JSON: " + e.message, { status: 400 });
      }
    }

    // 2. Metadata lookup
    if (request.method === "GET" && (url.pathname.endsWith("/meta") || url.pathname.includes("/api/share/"))) {
      if (!this.sessionMeta && this.ctx.storage) {
        this.sessionMeta = await this.ctx.storage.get("meta");
      }
      if (this.sessionMeta) {
        return jsonResponse({
          session_id: this.sessionMeta.session_id,
          name: this.sessionMeta.name,
          expires_at: this.sessionMeta.expires_at,
          has_host: !!(this.hostSocket && this.hostSocket.readyState === WebSocket.OPEN),
        });
      }
      return jsonResponse({ error: "Session not found" }, 404);
    }

    // 3. Direct URL lookup (for Hosted-AI redirect)
    if (url.pathname.endsWith("/direct_url")) {
      if (!this.sessionMeta && this.ctx.storage) {
        this.sessionMeta = await this.ctx.storage.get("meta");
      }
      return jsonResponse({ direct_url: this.sessionMeta ? this.sessionMeta.direct_url : null });
    }

    // 4. Host WebSocket connection (/host)
    if (url.pathname.endsWith("/host")) {
      const upgradeHeader = request.headers.get("Upgrade");
      if (!upgradeHeader || upgradeHeader.toLowerCase() !== "websocket") {
        return new Response("Expected WebSocket upgrade", { status: 426 });
      }

      const webSocketPair = new WebSocketPair();
      const [client, server] = Object.values(webSocketPair);

      server.accept();
      this.hostSocket = server;

      server.addEventListener("message", (event) => {
        try {
          const msg = JSON.parse(event.data);
          if (msg.type === "answer" && msg.request_id) {
            const resolver = this.pendingSignals.get(msg.request_id);
            if (resolver) {
              this.pendingSignals.delete(msg.request_id);
              resolver(msg);
            }
          } else if (msg.type === "ping") {
            server.send(JSON.stringify({ type: "pong" }));
          }
        } catch (_) {}
      });

      server.addEventListener("close", () => {
        if (this.hostSocket === server) {
          this.hostSocket = null;
        }
      });

      return new Response(null, {
        status: 101,
        webSocket: client,
      });
    }

    // 5. Client WebRTC SDP Signaling (/signal)
    if (url.pathname.endsWith("/signal")) {
      if (request.method !== "POST") {
        return new Response("Method Not Allowed", { status: 405 });
      }

      // Check if host daemon is connected
      if (!this.hostSocket || this.hostSocket.readyState !== WebSocket.OPEN) {
        return new Response(
          JSON.stringify({
            error: "Host peer is currently offline or unreachable. Please verify 'aestrl share' is running on the host.",
            code: "HOST_OFFLINE"
          }),
          { status: 503, headers: corsHeaders({ "Content-Type": "application/json" }) }
        );
      }

      try {
        const body = await request.json();
        const reqID = "sig-" + Math.random().toString(36).substring(2, 11);

        const answerPromise = new Promise((resolve, reject) => {
          const timer = setTimeout(() => {
            this.pendingSignals.delete(reqID);
            reject(new Error("Timeout waiting for host SDP answer"));
          }, 8000);

          this.pendingSignals.set(reqID, (answerMsg) => {
            clearTimeout(timer);
            resolve(answerMsg);
          });
        });

        // Forward SDP offer to host
        this.hostSocket.send(JSON.stringify({
          type: "offer",
          request_id: reqID,
          offer_sdp: body.offer_sdp,
        }));

        const answer = await answerPromise;
        return jsonResponse({
          answer_sdp: answer.answer_sdp,
          error: answer.error,
        });
      } catch (e) {
        return new Response(
          JSON.stringify({ error: e.message }),
          { status: 500, headers: corsHeaders({ "Content-Type": "application/json" }) }
        );
      }
    }

    return new Response("Not found in DO", { status: 404 });
  }
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);

    // Handle CORS preflight
    if (request.method === "OPTIONS") {
      return new Response(null, {
        headers: {
          "Access-Control-Allow-Origin": "*",
          "Access-Control-Allow-Methods": "GET, POST, OPTIONS",
          "Access-Control-Allow-Headers": "Content-Type, Authorization",
        },
      });
    }

    // Health check
    if (url.pathname === "/health") {
      return new Response("ok", { status: 200 });
    }

    // Helper: get DO stub for session if binding is present
    const getDOStub = (sessionID) => {
      if (env.SIGNALING_DO && sessionID) {
        const id = env.SIGNALING_DO.idFromName(sessionID);
        return env.SIGNALING_DO.get(id);
      }
      return null;
    };

    // 1. Hosted-AI Redirect Handling: /ai/:session/* -> 307 Redirect to host direct IP/port
    if (url.pathname.startsWith("/ai/")) {
      const parts = url.pathname.split("/").filter(Boolean);
      if (parts.length >= 2) {
        const sessionID = parts[1];
        let directURL = null;

        const stub = getDOStub(sessionID);
        if (stub) {
          try {
            const resp = await stub.fetch(new Request("https://do/direct_url"));
            if (resp.ok) {
              const data = await resp.json();
              directURL = data.direct_url;
            }
          } catch (_) {}
        } else {
          const sess = fallbackSessions.get(sessionID);
          if (sess) directURL = sess.direct_url;
        }

        if (directURL) {
          const subPath = parts.slice(2).join("/");
          const target = new URL(subPath + url.search, directURL);
          return Response.redirect(target.toString(), 307);
        }
        return new Response("Hosted-AI direct endpoint not registered or expired for session: " + sessionID, { status: 404 });
      }
    }

    // 2. Share Registration: POST /s/:session/register
    if (request.method === "POST" && url.pathname.endsWith("/register")) {
      const parts = url.pathname.split("/").filter(Boolean);
      const sessionID = parts[1];

      const stub = getDOStub(sessionID);
      if (stub) {
        return stub.fetch(request);
      }

      // Fallback in-memory registration
      try {
        const data = await request.json();
        fallbackSessions.set(sessionID, {
          session_id: sessionID,
          name: data.name || sessionID,
          secret_hash: data.secret_hash,
          expires_at: data.expires_at,
          direct_url: data.direct_url,
          created_at: new Date().toISOString(),
        });
        return jsonResponse({ status: "registered", session_id: sessionID });
      } catch (e) {
        return new Response("Invalid JSON: " + e.message, { status: 400 });
      }
    }

    // 3. Share Metadata: GET /api/share/:session
    if (request.method === "GET" && url.pathname.startsWith("/api/share/")) {
      const parts = url.pathname.split("/").filter(Boolean);
      const sessionID = parts[2];

      const stub = getDOStub(sessionID);
      if (stub) {
        return stub.fetch(request);
      }

      const sess = fallbackSessions.get(sessionID);
      if (sess) {
        return jsonResponse({
          session_id: sess.session_id,
          name: sess.name,
          expires_at: sess.expires_at,
          has_host: fallbackHostSockets.has(sessionID),
        });
      }
      return jsonResponse({ session_id: sessionID, name: sessionID });
    }

    // 4. Host Signaling WebSocket: /s/:session/host
    if (url.pathname.endsWith("/host")) {
      const parts = url.pathname.split("/").filter(Boolean);
      const sessionID = parts[1];

      const stub = getDOStub(sessionID);
      if (stub) {
        return stub.fetch(request);
      }

      // Fallback in-memory host socket
      const upgradeHeader = request.headers.get("Upgrade");
      if (!upgradeHeader || upgradeHeader.toLowerCase() !== "websocket") {
        return new Response("Expected WebSocket upgrade", { status: 426 });
      }

      const webSocketPair = new WebSocketPair();
      const [client, server] = Object.values(webSocketPair);

      server.accept();
      fallbackHostSockets.set(sessionID, server);

      server.addEventListener("message", (event) => {
        try {
          const msg = JSON.parse(event.data);
          if (msg.type === "answer" && msg.request_id) {
            const resolver = fallbackPendingSignals.get(msg.request_id);
            if (resolver) {
              fallbackPendingSignals.delete(msg.request_id);
              resolver(msg);
            }
          }
        } catch (_) {}
      });

      server.addEventListener("close", () => {
        if (fallbackHostSockets.get(sessionID) === server) {
          fallbackHostSockets.delete(sessionID);
        }
      });

      return new Response(null, {
        status: 101,
        webSocket: client,
      });
    }

    // 5. Client WebRTC Signaling: POST /s/:session/signal
    if (url.pathname.endsWith("/signal")) {
      const parts = url.pathname.split("/").filter(Boolean);
      const sessionID = parts[1];

      const stub = getDOStub(sessionID);
      if (stub) {
        return stub.fetch(request);
      }

      // Fallback in-memory signaling
      if (request.method === "POST") {
        const hostSocket = fallbackHostSockets.get(sessionID);
        if (!hostSocket) {
          return new Response(
            JSON.stringify({
              error: "Host peer is currently offline or unreachable",
              code: "HOST_OFFLINE"
            }),
            { status: 503, headers: corsHeaders({ "Content-Type": "application/json" }) }
          );
        }

        try {
          const body = await request.json();
          const reqID = "sig-" + Math.random().toString(36).substring(2, 10);

          const answerPromise = new Promise((resolve, reject) => {
            const timer = setTimeout(() => {
              fallbackPendingSignals.delete(reqID);
              reject(new Error("Timeout waiting for host SDP answer"));
            }, 8000);

            fallbackPendingSignals.set(reqID, (answerMsg) => {
              clearTimeout(timer);
              resolve(answerMsg);
            });
          });

          hostSocket.send(JSON.stringify({
            type: "offer",
            request_id: reqID,
            offer_sdp: body.offer_sdp,
          }));

          const answer = await answerPromise;
          return jsonResponse({
            answer_sdp: answer.answer_sdp,
            error: answer.error,
          });
        } catch (e) {
          return new Response(
            JSON.stringify({ error: e.message }),
            { status: 500, headers: corsHeaders({ "Content-Type": "application/json" }) }
          );
        }
      }
    }

    // 6. Static WebUI Asset Delivery:
    // If env.ASSETS is configured (Cloudflare Static Assets), fetch directly through it.
    if (env.ASSETS) {
      if (url.pathname.startsWith("/s/")) {
        // Rewrite /s/:session to /index.html for SPA routing
        const spaRequest = new Request(new URL("/index.html", request.url), request);
        return env.ASSETS.fetch(spaRequest);
      }
      return env.ASSETS.fetch(request);
    }

    // Fallback: Return basic SPA redirect or not found
    return new Response("Not found", { status: 404 });
  },
};

function jsonResponse(data, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: corsHeaders({ "Content-Type": "application/json" }),
  });
}

function corsHeaders(extra = {}) {
  return {
    "Access-Control-Allow-Origin": "*",
    "Access-Control-Allow-Methods": "GET, POST, OPTIONS",
    "Access-Control-Allow-Headers": "Content-Type, Authorization",
    ...extra,
  };
}
