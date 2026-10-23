// Proxies /api/* to the Go API so the browser only talks to this origin.
// API_URL is read at request time, so one image works locally and on a VPS.
export const dynamic = "force-dynamic";

const API = () => process.env.API_URL || "http://localhost:8080";

async function proxy(req: Request, ctx: { params: Promise<{ path: string[] }> }) {
  const { path } = await ctx.params;
  const url = new URL(req.url);
  const target = `${API()}/api/${path.join("/")}${url.search}`;
  const init: RequestInit & { duplex?: string } = {
    method: req.method,
    headers: { "content-type": req.headers.get("content-type") || "application/json" },
    cache: "no-store",
    signal: req.signal,
  };
  if (req.method !== "GET" && req.method !== "HEAD") {
    init.body = await req.text();
  }
  try {
    const res = await fetch(target, init);
    const headers = new Headers();
    headers.set("content-type", res.headers.get("content-type") || "application/json");
    if (path[0] === "stream") {
      headers.set("cache-control", "no-cache");
      headers.set("x-accel-buffering", "no");
    }
    return new Response(res.body, { status: res.status, headers });
  } catch (e) {
    return Response.json({ error: `api unreachable at ${API()}: ${String(e)}` }, { status: 502 });
  }
}

export const GET = proxy;
export const POST = proxy;
export const DELETE = proxy;
