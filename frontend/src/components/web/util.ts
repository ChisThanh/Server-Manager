import type { Certificate, EngineInfo, SiteSpec } from "../../../bindings/server-manager/services/web/models";
import { errCode } from "../../lib/api";
import type { Tone } from "../../ui/Status";
import type { Key } from "../../i18n";

/** Tone of a certificate status from the backend (ok | warn | err | expired | unknown). */
export function certTone(status: string | undefined): Tone {
  switch (status) {
    case "ok":
      return "ok";
    case "warn":
      return "warn";
    case "err":
    case "expired":
      return "err";
    default:
      return "muted";
  }
}

export function certStatusKey(status: string | undefined): Key {
  switch (status) {
    case "ok":
      return "web.cert.valid";
    case "warn":
      return "web.cert.expiringSoon";
    case "err":
      return "web.cert.expiringVerySoon";
    case "expired":
      return "web.cert.expired";
    default:
      return "web.cert.unknown";
  }
}

export function sourceKey(src: string): Key {
  switch (src) {
    case "certbot":
      return "web.src.certbot";
    case "custom":
      return "web.src.custom";
    case "caddy":
      return "web.src.caddy";
    default:
      return "web.src.nginx";
  }
}

export function kindKey(kind: string): Key {
  switch (kind) {
    case "proxy":
      return "web.kind.proxy";
    case "static":
      return "web.kind.static";
    case "redirect":
      return "web.kind.redirect";
    default:
      return "web.kind.other";
  }
}

export function engineLabel(name: string): string {
  switch (name) {
    case "nginx":
      return "Nginx";
    case "caddy":
      return "Caddy";
    case "apache":
      return "Apache";
    case "traefik":
      return "Traefik";
  }
  return name;
}

/** Whether an apply failed at the configuration test (show the output). */
export function isTestFailure(e: unknown): boolean {
  return errCode(e) === "web.testFailed";
}

export function editableEngines(engines: EngineInfo[] | null | undefined): string[] {
  return (engines ?? []).filter((e) => e.installed && e.editable).map((e) => e.name);
}

export function certByPath(certs: Certificate[] | null | undefined): Map<string, Certificate> {
  const m = new Map<string, Certificate>();
  for (const c of certs ?? []) m.set(c.path, c);
  return m;
}

/** Default values of a new site form. */
export function newSpec(engine: string, email: string): SiteSpec {
  return {
    engine,
    domains: [""],
    type: "proxy",
    upstreams: ["http://127.0.0.1:3000"],
    lbMethod: "",
    webSocket: false,
    connectTimeout: 0,
    readTimeout: 0,
    sendTimeout: 0,
    requestHeaders: [],
    root: "/var/www/",
    index: "index.html index.htm",
    spa: false,
    redirectTo: "",
    redirectCode: 301,
    preservePath: true,
    ssl: "none",
    certName: "",
    certPath: "",
    keyPath: "",
    email,
    keyType: "ecdsa",
    staging: false,
    forceHttps: true,
    http2: true,
    hsts: false,
    hstsMaxAge: 31536000,
    hstsSubdomains: false,
    responseHeaders: [],
    maxBodyMb: 0,
    gzip: true,
    rateLimit: false,
    rateRps: 10,
    rateBurst: 20,
    rateNoDelay: true,
    basicAuth: false,
    authRealm: "",
    authUsers: [],
    accessLog: "",
    errorLog: "",
    extra: "",
  };
}

/** Fills fields a stored spec may omit so the form stays controlled. */
export function completeSpec(sp: SiteSpec, email: string): SiteSpec {
  const base = newSpec(sp.engine || "nginx", email);
  const out: SiteSpec = { ...base, ...sp };
  out.domains = sp.domains?.length ? [...sp.domains] : [""];
  out.upstreams = sp.upstreams?.length ? [...sp.upstreams] : base.upstreams;
  out.requestHeaders = [...(sp.requestHeaders ?? [])];
  out.responseHeaders = [...(sp.responseHeaders ?? [])];
  out.authUsers = (sp.authUsers ?? []).map((u) => ({ user: u.user, password: "" }));
  if (!out.root) out.root = base.root;
  if (!out.index) out.index = base.index;
  if (!out.redirectCode) out.redirectCode = 301;
  if (!out.hstsMaxAge) out.hstsMaxAge = base.hstsMaxAge;
  if (!out.rateRps) out.rateRps = base.rateRps;
  if (!out.keyType) out.keyType = "ecdsa";
  if (!out.email) out.email = email;
  return out;
}

export function daysText(days: number, t: (k: Key, p?: Record<string, string | number>) => string): string {
  if (days < 0) return t("web.cert.expiredAgo", { n: -days });
  return t("web.cert.daysLeft", { n: days });
}
