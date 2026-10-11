/**
 * Aestrl Cloudflare Signaling Worker
 * Minimal WebRTC signaling relay only.
 * CRITICAL SECURITY INVARIANT: File inspection payloads and contents NEVER pass through this Worker.
 */

export default {
  async fetch(request, env) {
    const url = new URL(request.url);

    // Handle CORS preflight
    if (request.method === "OPTIONS") {
      return new Response(null, {
        headers: {
          "Access-Control-Allow-Origin": "*",
          "Access-Control-Allow-Methods": "GET, POST, OPTIONS",
          "Access-Control-Allow-Headers": "Content-Type",
        },
      });
    }

    // Health check
    if (url.pathname === "/health") {
      return new Response("ok", { status: 200 });
    }

    // Only handle signaling routes: /s/:session/signal or /api/share/:session
    const isSignal = url.pathname.includes("/signal");
    const isShareMeta = url.pathname.startsWith("/api/share/");

    if (!isSignal && !isShareMeta) {
      return new Response("Not Found: Only signaling is handled by this worker", { status: 404 });
    }

    // Forward signaling request to the public Aestrl Go hub
    const hubOrigin = env.HUB_ORIGIN || "https://aestrl.dvrsfy.org";
    const targetURL = new URL(url.pathname + url.search, hubOrigin);

    const forwardRequest = new Request(targetURL.toString(), {
      method: request.method,
      headers: request.headers,
      body: request.body,
      redirect: "follow",
    });

    const response = await fetch(forwardRequest);
    const newHeaders = new Headers(response.headers);
    newHeaders.set("Access-Control-Allow-Origin", "*");

    return new Response(response.body, {
      status: response.status,
      headers: newHeaders,
    });
  },
};
