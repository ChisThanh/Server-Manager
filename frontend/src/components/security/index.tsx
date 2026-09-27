import { useEffect, useState } from "react";
import { Activity, Flame, KeyRound, LayoutDashboard, Network, ShieldBan, ShieldCheck, Terminal, Users as UsersIcon } from "lucide-react";
import type { PanelProps } from "../../ui/types";
import { Page, PageHeader } from "../../ui/Page";
import { SubTabs } from "../../ui/Tabs";
import { useCan } from "../../ui/perm";
import { useT } from "../../i18n";
import { SEC_TABS, type RulePrefill, type SecTab, type SecViewProps } from "./common";
import { OverviewView } from "./Overview";
import { SSHView } from "./SSH";
import { KeysView } from "./Keys";
import { FirewallView } from "./Firewall";
import { PortsView } from "./Ports";
import { AccessView } from "./Access";
import { EventsView } from "./Events";
import { UsersView } from "./Users";
import "./security.css";

export default function SecurityPanel({ connId, visible, connected, arg }: PanelProps) {
  const t = useT();
  const canSec = useCan(connId, "security");
  const canUsers = useCan(connId, "users");
  const [tab, setTab] = useState<SecTab>("overview");
  // Tabs are mounted on first visit and kept (their state survives switching).
  const [seen, setSeen] = useState<Set<SecTab>>(() => new Set(["overview"]));
  const [keysUser, setKeysUser] = useState("");
  const [rule, setRule] = useState<RulePrefill | undefined>();

  const open = (tb: SecTab) => {
    setTab(tb);
    setSeen((s) => (s.has(tb) ? s : new Set(s).add(tb)));
  };

  // navigate("security", { tab: "firewall" }) from other modules.
  useEffect(() => {
    const want = (arg as { tab?: string; user?: string } | undefined)?.tab;
    if (want && (SEC_TABS as string[]).includes(want)) open(want as SecTab);
    const user = (arg as { user?: string } | undefined)?.user;
    if (user !== undefined) setKeysUser(user);
  }, [arg]);

  const go: SecViewProps["go"] = (tb, opts) => {
    if (opts?.user !== undefined) setKeysUser(opts.user);
    if (opts?.rule) setRule({ ...opts.rule, seq: Date.now() });
    open(tb);
  };

  const base = { connId, canSec, canUsers, go };
  const on = (tb: SecTab) => visible && connected && tab === tb;

  return (
    <Page className="sec-page">
      <PageHeader icon={<ShieldCheck size={20} />} title={t("sec.title")} sub={t("sec.subtitle")} />
      <SubTabs<SecTab>
        value={tab}
        onChange={open}
        items={[
          { id: "overview", label: t("sec.tab.overview"), icon: <LayoutDashboard size={14} /> },
          { id: "ssh", label: t("sec.tab.ssh"), icon: <Terminal size={14} /> },
          { id: "keys", label: t("sec.tab.keys"), icon: <KeyRound size={14} /> },
          { id: "firewall", label: t("sec.tab.firewall"), icon: <Flame size={14} /> },
          { id: "access", label: t("sec.tab.access"), icon: <ShieldBan size={14} /> },
          { id: "ports", label: t("sec.tab.ports"), icon: <Network size={14} /> },
          { id: "events", label: t("sec.tab.events"), icon: <Activity size={14} /> },
          { id: "users", label: t("sec.tab.users"), icon: <UsersIcon size={14} /> },
        ]}
      />
      <div className="sec-body">
        {seen.has("overview") && (
          <div hidden={tab !== "overview"}>
            <OverviewView {...base} active={on("overview")} />
          </div>
        )}
        {seen.has("ssh") && (
          <div hidden={tab !== "ssh"}>
            <SSHView {...base} active={on("ssh")} />
          </div>
        )}
        {seen.has("keys") && (
          <div hidden={tab !== "keys"}>
            <KeysView {...base} active={on("keys")} user={keysUser} setUser={setKeysUser} />
          </div>
        )}
        {seen.has("firewall") && (
          <div hidden={tab !== "firewall"}>
            <FirewallView {...base} active={on("firewall")} prefill={rule} clearPrefill={() => setRule(undefined)} />
          </div>
        )}
        {seen.has("access") && (
          <div hidden={tab !== "access"}>
            <AccessView {...base} active={on("access")} />
          </div>
        )}
        {seen.has("ports") && (
          <div hidden={tab !== "ports"}>
            <PortsView {...base} active={on("ports")} />
          </div>
        )}
        {seen.has("events") && (
          <div hidden={tab !== "events"}>
            <EventsView {...base} active={on("events")} />
          </div>
        )}
        {seen.has("users") && (
          <div hidden={tab !== "users"}>
            <UsersView {...base} active={on("users")} />
          </div>
        )}
      </div>
    </Page>
  );
}
