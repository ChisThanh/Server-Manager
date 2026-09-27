import { Suspense, lazy } from "react";

export interface CodeEditorProps {
  value: string;
  onChange?: (v: string) => void;
  language?: string;
  readOnly?: boolean;
  height?: number | string;
  /** Called on ⌘/Ctrl+Enter (e.g. run a query). */
  onSubmit?: () => void;
}

// Monaco is large; it is only downloaded/parsed when an editor is shown.
const Impl = lazy(() => import("./CodeEditorImpl"));

/**
 * Small embedded Monaco editor for config snippets (compose files, generated
 * nginx configs, SQL…). Controlled: `value` is pushed in when it differs.
 */
export function CodeEditor(props: CodeEditorProps) {
  return (
    <Suspense fallback={<div className="code-editor code-editor-loading" style={{ height: props.height ?? 300 }} />}>
      <Impl {...props} />
    </Suspense>
  );
}
