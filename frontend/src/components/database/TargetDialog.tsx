import { useState } from "react";
import { Modal } from "../Overlays";
import { errMsg } from "../../lib/api";
import { toast } from "../../store/ui";
import { useT } from "../../i18n";
import { DatabaseService, dbs, type Target, type TargetInput } from "./common";
import { Select } from "../../ui/Select";

const DEFAULT_PORT: Record<string, number> = {
  postgres: 5432,
  mysql: 3306,
  redis: 6379,
};

/** Add or edit a target. Detected targets can be edited too (their
 *  settings are then saved over the detected defaults). */
export function TargetDialog(props: {
  connId: string;
  target?: Target;
  containers: string[];
  onClose: () => void;
  onSaved: (t: Target) => void;
}) {
  const t = useT();
  const src = props.target;
  const [f, setF] = useState<TargetInput>({
    id: src?.id ?? "",
    engine: src?.engine ?? "postgres",
    name: src?.name ?? "",
    mode: src?.mode ?? "local",
    container: src?.container ?? "",
    auth: src?.auth ?? "password",
    host: src?.host ?? "",
    port: src?.port ?? 0,
    user: src?.user ?? "",
    database: src?.database ?? "",
  });
  const [password, setPassword] = useState("");
  const [changePw, setChangePw] = useState(!src?.hasPassword);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const set = <K extends keyof TargetInput>(k: K, v: TargetInput[K]) =>
    setF((x) => ({ ...x, [k]: v }));

  const save = async (test: boolean) => {
    setErr("");
    if (f.mode === "docker" && !f.container.trim()) {
      setErr(t("db.tg.containerRequired"));
      return;
    }
    if (f.auth === "password" && f.engine !== "redis" && !f.user.trim()) {
      setErr(t("db.tg.userRequired"));
      return;
    }
    if (f.auth === "password" && changePw && !password && !src?.hasPassword) {
      setErr(t("db.tg.passwordRequired"));
      return;
    }
    setBusy(true);
    try {
      const saved = await DatabaseService.SaveTarget(
        props.connId,
        { ...f, port: Number(f.port) || 0 },
        password,
        f.auth === "password" && changePw,
      );
      if (test) {
        const v = await dbs(props.connId, (pw) =>
          DatabaseService.Ping(props.connId, saved.id, pw),
        );
        toast(
          t("db.tg.testOk", { v: v.split(" ").slice(0, 2).join(" ") }),
          "success",
        );
      }
      props.onSaved(saved);
    } catch (e) {
      setErr(errMsg(e));
    } finally {
      setBusy(false);
    }
  };

  const peerHint =
    f.mode === "docker"
      ? t("db.tg.peerDocker")
      : f.engine === "postgres"
        ? t("db.tg.peerPg")
        : f.engine === "mysql"
          ? t("db.tg.peerMy")
          : t("db.tg.peerRedis");

  return (
    <Modal
      title={src ? t("db.tg.edit", { name: src.name }) : t("db.tg.add")}
      size="wide"
      onClose={props.onClose}
      footer={
        <>
          <button className="btn" onClick={props.onClose}>
            {t("common.cancel")}
          </button>
          <div className="grow" />
          <button className="btn" onClick={() => save(true)} disabled={busy}>
            {t("db.tg.saveTest")}
          </button>
          <button
            className="btn primary"
            onClick={() => save(false)}
            disabled={busy}
          >
            {busy ? <span className="spinner" /> : t("common.save")}
          </button>
        </>
      }
    >
      <div className="form-grid">
        <div className="field">
          <label>{t("db.tg.engine")}</label>
          <Select
            value={f.engine}
            onChange={(v) => set("engine", v)}
            disabled={!!src?.detected}
            options={[
              { value: "postgres", label: "PostgreSQL" },
              { value: "mysql", label: "MySQL / MariaDB" },
              { value: "redis", label: "Redis" },
            ]}
          />
        </div>
        <div className="field">
          <label>{t("db.tg.name")}</label>
          <input
            className="input"
            value={f.name}
            onChange={(e) => set("name", e.target.value)}
            placeholder={t("db.optional")}
          />
        </div>
        <div className="field">
          <label>{t("db.tg.mode")}</label>
          <div className="segmented">
            <button
              className={f.mode === "local" ? "on" : ""}
              onClick={() => set("mode", "local")}
              disabled={!!src?.detected}
            >
              {t("db.tg.local")}
            </button>
            <button
              className={f.mode === "docker" ? "on" : ""}
              onClick={() => set("mode", "docker")}
              disabled={!!src?.detected}
            >
              Docker
            </button>
          </div>
        </div>
        {f.mode === "docker" && (
          <div className="field">
            <label>{t("db.tg.container")}</label>
            <input
              className="input mono"
              list="db-containers"
              value={f.container}
              onChange={(e) => set("container", e.target.value)}
              disabled={!!src?.detected}
            />
            <datalist id="db-containers">
              {props.containers.map((c) => (
                <option key={c} value={c} />
              ))}
            </datalist>
          </div>
        )}
        <div className="field">
          <label>{t("db.tg.auth")}</label>
          <div className="segmented">
            <button
              className={f.auth === "peer" ? "on" : ""}
              onClick={() => set("auth", "peer")}
            >
              {t("db.tg.peer")}
            </button>
            <button
              className={f.auth === "password" ? "on" : ""}
              onClick={() => set("auth", "password")}
            >
              {t("db.tg.password")}
            </button>
          </div>
        </div>
      </div>
      <div className="hint-box">
        {f.auth === "peer" ? peerHint : t("db.tg.passwordHint")}
      </div>
      <div className="form-grid">
        <div className="field">
          <label>{t("db.tg.host")}</label>
          <input
            className="input mono"
            value={f.host}
            onChange={(e) => set("host", e.target.value)}
            placeholder={
              f.engine === "redis" ? "127.0.0.1" : t("db.tg.hostDefault")
            }
          />
        </div>
        <div className="field">
          <label>{t("db.tg.port")}</label>
          <input
            className="input mono"
            value={f.port || ""}
            onChange={(e) =>
              set("port", Number(e.target.value.replace(/\D/g, "")) || 0)
            }
            placeholder={String(DEFAULT_PORT[f.engine])}
          />
        </div>
        {f.auth === "password" && (
          <div className="field">
            <label>{t("db.tg.user")}</label>
            <input
              className="input mono"
              value={f.user}
              onChange={(e) => set("user", e.target.value)}
              placeholder={f.engine === "redis" ? t("db.tg.redisUser") : ""}
            />
          </div>
        )}
        {f.auth === "password" && (
          <div className="field">
            <label>{t("db.tg.passwordLabel")}</label>
            {src?.hasPassword && !changePw ? (
              <button className="btn sm" onClick={() => setChangePw(true)}>
                {t("db.tg.changePassword")}
              </button>
            ) : (
              <input
                className="input"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="new-password"
              />
            )}
            <div className="hint">{t("db.tg.keychain")}</div>
          </div>
        )}
        <div className="field">
          <label>
            {f.engine === "redis" ? t("db.tg.redisDb") : t("db.tg.database")}
          </label>
          <input
            className="input mono"
            value={f.database}
            onChange={(e) => set("database", e.target.value)}
            placeholder={
              f.engine === "postgres"
                ? "postgres"
                : f.engine === "redis"
                  ? "0"
                  : t("db.optional")
            }
          />
        </div>
      </div>
      {err && (
        <div className="db-error">
          <pre>{err}</pre>
        </div>
      )}
    </Modal>
  );
}
