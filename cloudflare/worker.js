/**
 * Aestrl Cloudflare Coordination Worker
 *
 * Responsibilities:
 * 1. Discovery records & session metadata.
 * 2. Public share URL resolution & browser asset delivery.
 * 3. WebRTC signaling relay (offer/answer exchange for P2P connection).
 * 4. Hosted-AI HTTP redirect handling (HTTP 307 to direct host IP/port).
 *
 * CRITICAL SECURITY INVARIANT:
 * File inspection payloads and contents NEVER pass through this Worker.
 */

// In-memory session registry and signaling mailboxes
const sessions = new Map();
const hostConnections = new Map();
const pendingSignalRequests = new Map();

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

    // 1. Hosted-AI Redirect Handling: /ai/:session/* -> 307 Redirect to host direct IP/port
    if (url.pathname.startsWith("/ai/")) {
      const parts = url.pathname.split("/").filter(Boolean);
      if (parts.length >= 2) {
        const sessionID = parts[1];
        const sess = sessions.get(sessionID);
        if (sess && sess.direct_url) {
          const subPath = parts.slice(2).join("/");
          const target = new URL(subPath + url.search, sess.direct_url);
          return Response.redirect(target.toString(), 307);
        }
        return new Response("Hosted-AI direct endpoint not registered or expired for session: " + sessionID, { status: 404 });
      }
    }

    // 2. Share Registration: POST /s/:session/register
    if (request.method === "POST" && url.pathname.endsWith("/register")) {
      const parts = url.pathname.split("/").filter(Boolean);
      const sessionID = parts[1];
      try {
        const data = await request.json();
        sessions.set(sessionID, {
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
      const sess = sessions.get(sessionID);
      if (sess) {
        return jsonResponse({
          session_id: sess.session_id,
          name: sess.name,
          expires_at: sess.expires_at,
        });
      }
      return jsonResponse({ session_id: sessionID, name: sessionID });
    }

    // 4. Host Signaling WebSocket: /s/:session/host
    if (url.pathname.endsWith("/host")) {
      const parts = url.pathname.split("/").filter(Boolean);
      const sessionID = parts[1];

      const upgradeHeader = request.headers.get("Upgrade");
      if (!upgradeHeader || upgradeHeader !== "websocket") {
        return new Response("Expected WebSocket upgrade", { status: 426 });
      }

      const webSocketPair = new WebSocketPair();
      const [client, server] = Object.values(webSocketPair);

      server.accept();
      hostConnections.set(sessionID, server);

      server.addEventListener("message", (event) => {
        try {
          const msg = JSON.parse(event.data);
          if (msg.type === "answer" && msg.request_id) {
            const resolver = pendingSignalRequests.get(msg.request_id);
            if (resolver) {
              pendingSignalRequests.delete(msg.request_id);
              resolver(msg);
            }
          }
        } catch (_) {}
      });

      server.addEventListener("close", () => {
        if (hostConnections.get(sessionID) === server) {
          hostConnections.delete(sessionID);
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

      if (request.method === "POST") {
        const hostSocket = hostConnections.get(sessionID);
        if (!hostSocket) {
          return new Response(
            JSON.stringify({ error: "Host peer is currently offline or unreachable" }),
            { status: 503, headers: corsHeaders({ "Content-Type": "application/json" }) }
          );
        }

        try {
          const body = await request.json();
          const reqID = "sig-" + Math.random().toString(36).substring(2, 10);

          const answerPromise = new Promise((resolve, reject) => {
            const timer = setTimeout(() => {
              pendingSignalRequests.delete(reqID);
              reject(new Error("Timeout waiting for host SDP answer"));
            }, 8000);

            pendingSignalRequests.set(reqID, (answerMsg) => {
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

    // 6. Static WebUI delivery: /s/:session or /
    if (url.pathname === "/" || url.pathname.startsWith("/s/")) {
      // In production Cloudflare Workers with Static Assets, env.ASSETS.fetch serves files.
      // Otherwise fallback to static index.
      if (env.ASSETS) {
        return env.ASSETS.fetch(request);
      }
      return new Response(INDEX_HTML, {
        headers: { "Content-Type": "text/html; charset=utf-8" },
      });
    }

    if (url.pathname === "/app.js" && env.ASSETS) {
      return env.ASSETS.fetch(request);
    }
    if (url.pathname === "/style.css" && env.ASSETS) {
      return env.ASSETS.fetch(request);
    }

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

const INDEX_HTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Aestrl — P2P File Inspector</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0f172a; color: #f8fafc; margin: 0; padding: 2rem; display: flex; flex-direction: column; align-items: center; justify-content: center; min-height: 80vh; }
    h1 { font-size: 2.5rem; margin-bottom: 0.5rem; }
    p { color: #94a3b8; font-size: 1.1rem; }
    .card { background: #1e293b; border-radius: 8px; padding: 2rem; border: 1px solid #334155; max-width: 600px; width: 100%; box-shadow: 0 4px 6px -1px rgba(0,0,0,0.5); }
    code { background: #0f172a; padding: 0.2rem 0.4rem; border-radius: 4px; font-family: monospace; color: #38bdf8; }
  </style>
</head>
<body>
  <div class="card">
    <h1>✦ aestrl</h1>
    <p>Portable, read-only, peer-to-peer file inspection tool.</p>
    <p>To inspect a share in browser or CLI:</p>
    <p><code>aestrl tui &lt;share_url&gt;</code> or open the share link with capability fragment.</p>
  </div>
</body>
</html>`;
