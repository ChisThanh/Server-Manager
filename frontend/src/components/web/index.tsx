import { useEffect, useState } from "react";
import { Globe, Lock, RefreshCw } from "lucide-react";
import { WebService } from "../../../bindings/server-manager/services/web";
import type { PanelProps } from "../../ui/types";
import { ErrorBox, Loading, Page, PageHeader } from "../../ui/Page";
import { SubTabs } from "../../ui/Tabs";
import { useRemote } from "../../ui/hooks";
import { withSudo } from "../../store/sudo";
import { useT } from "../../i18n";
import { engineLabel } from "./util";
import { Domains } from "./Domains";
import { SSL } from "./SSL";
import { registerWebLanguages } from "./langs";
import "./web.css";

type Tab = "domains" | "ssl";

export default function WebPanel({ connId, visible, connected, arg }: PanelProps) {
  const t = useT();
  const initial = (arg as { tab?: Tab } | undefined)?.tab;
  const [tab, setTab] = useState<Tab>(initial === "ssl" ? "ssl" : "domains");
  const on = visible && connected;

  useEffect(() => registerWebLanguages(), []);
  useEffect(() => {
    const a = (arg as { tab?: Tab } | undefined)?.tab;
    if (a === "ssl" || a === "domains") setTab(a);
  }, [arg]);

  // Status first: it may ask for the sudo password once; the other calls
  // then reuse it (the backend remembers a password that worked).
  const status = useRemote(() => withSudo(connId, (pw) => WebService.Status(connId, pw)), [connId], { enabled: on });
  const ready = on && !!status.data;
  const sites = useRemote(() => withSudo(connId, (pw) => WebService.Sites(connId, pw)), [connId], { enabled: ready });
  const certs = useRemote(() => withSudo(connId, (pw) => WebService.CertOverview(connId, pw)), [connId], { enabled: ready });

  const reloadAll = async () => {
    await status.reload();
    await Promise.all([sites.reload(), certs.reload()]);
  };

  const engines = (status.data?.engines ?? []).filter((e) => e.installed);
  const sub = [
    ...engines.map((e) => `${engineLabel(e.name)}${e.version ? " " + e.version : ""}${e.running ? "" : " (" + t("web.stopped") + ")"}`),
    status.data?.certbot ? `certbot ${status.data.certbotVersion}` : "",
  ]
    .filter(Boolean)
    .join(" · ");
  const expiring = (certs.data?.certificates ?? []).filter((c) => c.status === "warn" || c.status === "err" || c.status === "expired").length;
  const busy = status.loading || sites.loading || certs.loading;

  return (
    <Page className="web-page">
      <PageHeader
        icon={<Globe size={18} />}
        title={t("web.title")}
        sub={sub || t("web.subtitle")}
        actions={
          <button className="icon-btn" title={t("common.refresh")} onClick={reloadAll} disabled={busy}>
            <RefreshCw size={14} className={busy ? "spin-icon" : ""} />
          </button>
        }
      />
      <SubTabs<Tab>
        value={tab}
        onChange={setTab}
        items={[
          { id: "domains", label: t("web.tab.domains"), icon: <Globe size={14} />, badge: sites.data?.sites?.length || undefined },
          { id: "ssl", label: t("web.tab.ssl"), icon: <Lock size={14} />, badge: expiring || undefined },
        ]}
      />
      {status.error && !status.data ? (
        <ErrorBox error={status.error} onRetry={status.reload} />
      ) : !status.data ? (
        <Loading label={t("web.detecting")} />
      ) : tab === "domains" ? (
        <Domains connId={connId} status={status.data} sites={sites} certs={certs.data} onChanged={reloadAll} />
      ) : (
        <SSL connId={connId} status={status.data} overview={certs} sites={sites.data?.sites ?? []} onChanged={reloadAll} />
      )}
    </Page>
  );
}
