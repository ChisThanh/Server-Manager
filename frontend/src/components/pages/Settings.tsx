import { useEffect, useState } from "react";
import { Settings as SettingsIcon } from "lucide-react";
import type { Settings } from "../../../bindings/server-manager/internal/core";
import { AppService } from "../../../bindings/server-manager/services";
import { Page, PageHeader } from "../../ui/Page";
import { useSettings } from "../../store/settings";
import { toast } from "../../store/ui";
import { LANGUAGES, useLang, useT } from "../../i18n";
import "./pages.css";
import { Select } from "../../ui/Select";

function Row({ title, desc, children }: { title: string; desc?: string; children: React.ReactNode }) {
  return (
    <div className="setting-row">
      <div className="txt">
        <div>{title}</div>
        {desc && <div className="d">{desc}</div>}
      </div>
      <div className="ctl">{children}</div>
    </div>
  );
}

export default function SettingsPage() {
  const t = useT();
  const saved = useSettings((s) => s.settings);
  const [s, setS] = useState<Settings>(saved);
  const [actor, setActor] = useState("");
  const lang = useLang((x) => x.lang);
  const setLang = useLang((x) => x.setLang);
  useEffect(() => setS(saved), [saved]);
  useEffect(() => {
    AppService.DefaultActor().then(setActor).catch(() => {});
  }, []);
  const dirty = JSON.stringify(s) !== JSON.stringify(saved);
  const set = <K extends keyof Settings>(k: K, v: Settings[K]) => setS((x) => ({ ...x, [k]: v }));
  const save = async () => {
    if (await useSettings.getState().save(s)) toast(t("set.saved"), "success");
  };
  const num = (k: keyof Settings, min: number, max: number) => (
    <input className="input" type="number" min={min} max={max} value={s[k] as number} onChange={(e) => set(k, Math.max(min, Math.min(max, Number(e.target.value) || 0)) as never)} />
  );
  const check = (k: keyof Settings) => <input type="checkbox" checked={s[k] as boolean} onChange={(e) => set(k, e.target.checked as never)} />;

  return (
    <Page className="page">
      <PageHeader
        icon={<SettingsIcon size={18} />}
        title={t("app.page.settings")}
        actions={
          <>
            {dirty && (
              <button className="btn" onClick={() => setS(saved)}>
                {t("common.cancel")}
              </button>
            )}
            <button className="btn primary" disabled={!dirty} onClick={save}>
              {t("common.save")}
            </button>
          </>
        }
      />
      <div className="settings-grid">
        <div className="panel-box">
          <div className="box-title">{t("set.general")}</div>
          <Row title={t("set.language")} desc={t("set.languageDesc")}>
            <Select value={lang} onChange={setLang} options={LANGUAGES.map((l) => ({ value: l.code, label: l.name }))} />
          </Row>
          <Row title={t("set.operator")} desc={t("set.operatorDesc")}>
            <input className="input" value={s.operatorName} placeholder={actor} onChange={(e) => set("operatorName", e.target.value)} />
          </Row>
        </div>
        <div className="panel-box">
          <div className="box-title">{t("set.monitoring")}</div>
          <Row title={t("set.interval")} desc={t("set.intervalDesc")}>
            {num("collectInterval", 5, 600)} <span className="muted">s</span>
          </Row>
          <Row title={t("set.background")} desc={t("set.backgroundDesc")}>
            {check("backgroundMode")}
          </Row>
          <Row title={t("set.desktopNotify")} desc={t("set.desktopNotifyDesc")}>
            {check("desktopNotify")}
          </Row>
          <Row title={t("set.notifyLang")} desc={t("set.notifyLangDesc")}>
            <Select
              value={s.notifyLang}
              onChange={(v) => set("notifyLang", v)}
              options={[
                { value: "vi", label: "Tiếng Việt" },
                { value: "en", label: "English" },
              ]}
            />
          </Row>
        </div>
        <div className="panel-box">
          <div className="box-title">{t("set.safety")}</div>
          <Row title={t("set.confirmProd")} desc={t("set.confirmProdDesc")}>
            {check("confirmProduction")}
          </Row>
          <Row title={t("set.termIdle")} desc={t("set.termIdleDesc")}>
            {num("terminalIdleMinutes", 0, 1440)} <span className="muted">{t("set.minutes")}</span>
          </Row>
          <Row title={t("set.recordTerm")} desc={t("set.recordTermDesc")}>
            {check("recordTerminal")}
          </Row>
          <Row title={t("set.concurrency")} desc={t("set.concurrencyDesc")}>
            {num("commandConcurrency", 1, 50)}
          </Row>
        </div>
        <div className="panel-box">
          <div className="box-title">{t("set.data")}</div>
          <Row title={t("set.eventRetention")} desc={t("set.retentionDesc")}>
            {num("eventRetentionDays", 0, 3650)} <span className="muted">{t("set.days")}</span>
          </Row>
          <Row title={t("set.auditRetention")} desc={t("set.retentionDesc")}>
            {num("auditRetentionDays", 0, 3650)} <span className="muted">{t("set.days")}</span>
          </Row>
          <div className="hint" style={{ marginTop: 8 }}>
            {t("set.dataHint")}
          </div>
        </div>
      </div>
    </Page>
  );
}
