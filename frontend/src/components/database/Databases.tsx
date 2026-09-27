import { useMemo, useState } from "react";
import {
  Archive,
  ArrowDown,
  ArrowUp,
  Code2,
  Database,
  Plus,
  RefreshCw,
  SquareTerminal,
  Table2,
  UserPlus,
} from "lucide-react";
import { useRemote } from "../../ui/hooks";
import { Empty, ErrorBox, Loading, Section } from "../../ui/Page";
import { errMsg } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import { toast, useUI } from "../../store/ui";
import { useT } from "../../i18n";
import type { TermRequest } from "../terminal/TerminalPanel";
import {
  DatabaseService,
  consoleExec,
  dbs,
  identError,
  num,
  pct,
  type DatabaseInfo,
  type TargetViewProps,
} from "./common";

type SortKey = "name" | "size" | "tables" | "connections";

export function DatabasesView(
  props: TargetViewProps & { onQuery: (db: string) => void },
) {
  const { connId, target, active, canEdit, canShell, navigate } = props;
  const t = useT();
  const pg = target.engine === "postgres";
  const [q, setQ] = useState("");
  const [showSystem, setShowSystem] = useState(false);
  const [sort, setSort] = useState<{ key: SortKey; desc: boolean }>({
    key: "size",
    desc: true,
  });
  const [selected, setSelected] = useState<string | null>(null);
  const list = useRemote(
    () => dbs(connId, (pw) => DatabaseService.Databases(connId, target.id, pw)),
    [connId, target.id],
    { enabled: active },
  );
  const tables = useRemote(
    () =>
      selected
        ? dbs(connId, (pw) =>
            DatabaseService.Tables(connId, target.id, selected, pw),
          )
        : Promise.resolve([]),
    [connId, target.id, selected],
    { enabled: active && !!selected },
  );

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const r = (list.data ?? []).filter(
      (d) =>
        (showSystem || !d.system || pg) &&
        (!needle || d.name.toLowerCase().includes(needle)),
    );
    const dir = sort.desc ? -1 : 1;
    return r.sort((a, b) => {
      const k = sort.key;
      if (k === "name") return a.name.localeCompare(b.name) * dir;
      return ((a[k] as number) - (b[k] as number)) * dir;
    });
  }, [list.data, q, showSystem, sort]);

  const th = (key: SortKey, label: string, numeric = true) => (
    <th
      className={`sortable ${numeric ? "num" : ""}`}
      onClick={() =>
        setSort((s) => ({
          key,
          desc: s.key === key ? !s.desc : key !== "name",
        }))
      }
    >
      {label}
      {sort.key === key &&
        (sort.desc ? <ArrowDown size={11} /> : <ArrowUp size={11} />)}
    </th>
  );

  const createDb = async () => {
    const fields = [
      {
        name: "name",
        label: t("db.dbs.name"),
        autoFocus: true,
        placeholder: "app_db",
        validate: identError,
      },
    ];
    if (pg)
      fields.push({
        name: "owner",
        label: t("db.dbs.owner"),
        autoFocus: false,
        placeholder: t("db.optional"),
        validate: (v: string) => (v.trim() ? identError(v) : null),
      });
    const r = await useUI
      .getState()
      .openDialog({
        title: t("db.dbs.createDb"),
        fields,
        confirmText: t("db.dbs.create"),
      });
    if (r?.action !== "ok") return;
    const name = String(r.values.name).trim();
    try {
      await dbs(connId, (pw) =>
        DatabaseService.CreateDatabase(
          connId,
          target.id,
          name,
          String(r.values.owner ?? "").trim(),
          pw,
        ),
      );
      toast(t("db.dbs.created", { name }), "success");
      list.reload();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const createUser = async () => {
    const dbNames = (list.data ?? [])
      .filter((d) => !d.system)
      .map((d) => d.name);
    const r = await useUI.getState().openDialog({
      title: t("db.dbs.createUser"),
      message: <div className="muted">{t("db.dbs.createUserHint")}</div>,
      fields: [
        {
          name: "name",
          label: t("db.dbs.userName"),
          autoFocus: true,
          placeholder: "app_user",
          validate: identError,
        },
        {
          name: "password",
          label: t("db.dbs.password"),
          type: "password",
          validate: (v) =>
            v.length < 8
              ? t("db.dbs.pwShort")
              : /[\n\r\0]/.test(v)
                ? t("db.dbs.pwInvalid")
                : null,
        },
        ...(pg
          ? []
          : [
              {
                name: "host",
                label: t("db.dbs.host"),
                value: "localhost",
                validate: (v: string) =>
                  /^[A-Za-z0-9_.%:-]{1,255}$/.test(v.trim())
                    ? null
                    : t("db.dbs.hostInvalid"),
              },
            ]),
        {
          name: "grant",
          label: t("db.dbs.grantOn"),
          placeholder: dbNames[0]
            ? `${t("db.optional")} — ${dbNames.slice(0, 3).join(", ")}`
            : t("db.optional"),
          validate: (v) => (v.trim() ? identError(v) : null),
        },
      ],
      confirmText: t("db.dbs.create"),
    });
    if (r?.action !== "ok") return;
    const name = String(r.values.name).trim();
    try {
      await dbs(connId, (pw) =>
        DatabaseService.CreateUser(
          connId,
          target.id,
          name,
          String(r.values.password),
          String(r.values.host ?? "").trim(),
          String(r.values.grant ?? "").trim(),
          pw,
        ),
      );
      toast(t("db.dbs.userCreated", { name }), "success");
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const openConsole = (db: string) => {
    const exec = consoleExec(target, db);
    if (exec)
      navigate("terminal", {
        cwd: "",
        nonce: Date.now(),
        exec,
      } satisfies TermRequest);
  };

  if (list.error && !list.data)
    return <ErrorBox error={list.error} onRetry={list.reload} />;
  if (!list.data) return <Loading />;

  return (
    <>
      <div className="toolbar">
        <input
          className="input"
          placeholder={t("db.filter")}
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        {!pg && (
          <label className="check">
            <input
              type="checkbox"
              checked={showSystem}
              onChange={(e) => setShowSystem(e.target.checked)}
            />{" "}
            {t("db.dbs.showSystem")}
          </label>
        )}
        <div className="grow" />
        {canEdit && (
          <>
            <button className="btn sm" onClick={createUser}>
              <UserPlus size={13} /> {t("db.dbs.createUser")}
            </button>
            <button className="btn sm primary" onClick={createDb}>
              <Plus size={13} /> {t("db.dbs.createDb")}
            </button>
          </>
        )}
        <button
          className="icon-btn"
          onClick={list.reload}
          title={t("common.refresh")}
        >
          <RefreshCw size={14} className={list.loading ? "spin-icon" : ""} />
        </button>
      </div>
      {rows.length === 0 ? (
        <Empty icon={<Database size={28} />} text={t("db.dbs.none")} />
      ) : (
        <div className="table-wrap db-scroll-x">
          <table className="grid">
            <thead>
              <tr>
                {th("name", t("db.dbs.name"), false)}
                {pg && <th>{t("db.dbs.owner")}</th>}
                <th>{pg ? t("db.dbs.encoding") : t("db.dbs.charset")}</th>
                {th("size", t("db.dbs.size"))}
                {pg
                  ? th("connections", t("db.dbs.conns"))
                  : th("tables", t("db.dbs.tables"))}
                {pg && <th className="num">{t("db.dbs.cacheHit")}</th>}
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((d: DatabaseInfo) => (
                <tr
                  key={d.name}
                  className={`db-clickable ${selected === d.name ? "db-selected" : ""}`}
                  onClick={() =>
                    setSelected(selected === d.name ? null : d.name)
                  }
                >
                  <td className="mono">
                    {d.name}{" "}
                    {d.system && (
                      <span className="badge">{t("db.dbs.system")}</span>
                    )}
                    {pg && !d.allowConn && (
                      <span className="badge warn">{t("db.dbs.noConn")}</span>
                    )}
                  </td>
                  {pg && <td>{d.owner}</td>}
                  <td className="muted">
                    {d.encoding}
                    {d.collation && (
                      <span className="db-sub"> · {d.collation}</span>
                    )}
                  </td>
                  <td className="num">
                    {d.size >= 0 ? formatBytes(d.size) : "–"}
                  </td>
                  <td className="num">
                    {pg ? num(d.connections) : num(d.tables)}
                  </td>
                  {pg && <td className="num">{pct(d.cacheHit)}</td>}
                  <td
                    className="db-actions"
                    onClick={(e) => e.stopPropagation()}
                  >
                    <button
                      className="icon-btn"
                      title={t("db.dbs.query")}
                      onClick={() => props.onQuery(d.name)}
                    >
                      <Code2 size={13} />
                    </button>
                    {canShell && consoleExec(target, d.name) && (
                      <button
                        className="icon-btn"
                        title={t("db.console")}
                        onClick={() => openConsole(d.name)}
                      >
                        <SquareTerminal size={13} />
                      </button>
                    )}
                    <button
                      className="icon-btn"
                      title={t("db.dbs.backup")}
                      onClick={() =>
                        navigate("backup", {
                          engine: target.engine,
                          database: d.name,
                          target: target.id,
                        })
                      }
                    >
                      <Archive size={13} />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {selected && (
        <Section
          title={t("db.dbs.tablesOf", { name: selected })}
          icon={<Table2 size={14} />}
        >
          {tables.error ? (
            <ErrorBox error={tables.error} onRetry={tables.reload} />
          ) : !tables.data ? (
            <Loading />
          ) : tables.data.length === 0 ? (
            <Empty text={t("db.dbs.noTables")} />
          ) : (
            <div className="table-wrap db-scroll-x">
              <table className="grid">
                <thead>
                  <tr>
                    <th>{t("db.dbs.table")}</th>
                    <th>{pg ? t("db.dbs.kind") : t("db.dbs.engine")}</th>
                    <th className="num">{t("db.dbs.rowsEst")}</th>
                    <th className="num">{t("db.dbs.total")}</th>
                    <th className="num">{t("db.dbs.data")}</th>
                    <th className="num">{t("db.dbs.indexes")}</th>
                    <th className="num">
                      {pg ? t("db.dbs.dead") : t("db.dbs.free")}
                    </th>
                    <th>{pg ? t("db.dbs.vacuum") : t("db.dbs.updated")}</th>
                  </tr>
                </thead>
                <tbody>
                  {(tables.data ?? []).map((tb) => (
                    <tr key={tb.schema + "." + tb.name}>
                      <td className="mono">
                        {tb.schema !== "public" && tb.schema !== selected && (
                          <span className="muted">{tb.schema}.</span>
                        )}
                        {tb.name}
                      </td>
                      <td className="muted">{tb.kind}</td>
                      <td className="num">{num(tb.rows)}</td>
                      <td className="num">{formatBytes(tb.totalBytes)}</td>
                      <td className="num">{formatBytes(tb.dataBytes)}</td>
                      <td className="num">{formatBytes(tb.indexBytes)}</td>
                      <td
                        className={`num ${pg && tb.rows > 0 && tb.deadRows > tb.rows * 0.2 ? "db-warn" : ""}`}
                      >
                        {pg ? num(tb.deadRows) : formatBytes(tb.freeBytes)}
                      </td>
                      <td className="muted db-nowrap">
                        {pg ? tb.lastVacuum || "–" : tb.lastUpdate || "–"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Section>
      )}
    </>
  );
}
