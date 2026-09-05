import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
const config = process.env.CLOUDFLARE_API_TOKEN
  ? ""
  : await readFile(
      process.env.WRANGLER_AUTH_CONFIG ||
        `${homedir()}/Library/Preferences/.wrangler/config/default.toml`,
      "utf8",
    );
const token =
  process.env.CLOUDFLARE_API_TOKEN ||
  config.match(/^oauth_token\s*=\s*"([^"]+)"/m)?.[1];
if (!token) throw new Error("Run wrangler login first.");
const account = process.env.CLOUDFLARE_ACCOUNT_ID;
if (!account || !/^[a-f0-9]{32}$/.test(account))
  throw new Error("Set CLOUDFLARE_ACCOUNT_ID to your Cloudflare account ID.");
const projectName = process.env.CLOUDFLARE_PAGES_PROJECT || "tailterm";
const domain = process.env.CLOUDFLARE_CUSTOM_DOMAIN || "tailterm.tailarr.com";
const zoneName = process.env.CLOUDFLARE_ZONE_NAME || "tailarr.com";
const project = `/accounts/${account}/pages/projects/${encodeURIComponent(projectName)}`;
async function api(path, method = "GET", body) {
  const r = await fetch(`https://api.cloudflare.com/client/v4${path}`, {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await r.json();
  if (!data.success) throw new Error(JSON.stringify(data.errors));
  return data.result;
}
const zones = await api(`/zones?name=${encodeURIComponent(zoneName)}`);
console.log(
  "Website zone:",
  zones.map((z) => ({ id: z.id, name: z.name, status: z.status })),
);
let domains = await api(`${project}/domains`);
if (
  process.argv.includes("--attach") &&
  !domains.some((d) => d.name === domain)
) {
  await api(`${project}/domains`, "POST", { name: domain });
  domains = await api(`${project}/domains`);
}
console.log("Tailterm domains:", JSON.stringify(domains));
if (process.argv.includes("--dns")) {
  const zone = zones.find((z) => z.name === zoneName);
  if (!zone) throw new Error("Cloudflare zone not found in this account.");
  const records = await api(
    `/zones/${zone.id}/dns_records?name=${encodeURIComponent(domain)}`,
  );
  if (records.length)
    console.log(
      "Existing DNS:",
      records.map((r) => ({ type: r.type, name: r.name, content: r.content })),
    );
  else
    console.log(
      "Created DNS:",
      await api(`/zones/${zone.id}/dns_records`, "POST", {
        type: "CNAME",
        name: domain,
        content: `${projectName}.pages.dev`,
        proxied: true,
        ttl: 1,
      }),
    );
}
