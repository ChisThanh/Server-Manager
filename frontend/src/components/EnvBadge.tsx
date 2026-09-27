import { useT } from "../i18n";

/** Colored environment label (production / staging / development). */
export function EnvBadge({ env, short }: { env?: string; short?: boolean }) {
  const t = useT();
  if (!env) return null;
  const key = `app.env.${env}` as const;
  const label = t(key as never);
  return <span className={`env-badge ${env}`}>{short ? label.slice(0, 4) : label}</span>;
}
