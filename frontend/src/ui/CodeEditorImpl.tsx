import { useEffect, useRef } from "react";
import { monaco } from "../lib/monaco";
import type { CodeEditorProps } from "./CodeEditor";

/**
 * Small embedded Monaco editor for config snippets (compose files, generated
 * nginx configs, SQL…). Controlled: `value` is pushed in when it differs.
 */
export default function CodeEditorImpl(props: CodeEditorProps) {
  const host = useRef<HTMLDivElement>(null);
  const ed = useRef<monaco.editor.IStandaloneCodeEditor | null>(null);
  const cb = useRef(props);
  cb.current = props;

  useEffect(() => {
    const e = monaco.editor.create(host.current!, {
      value: props.value,
      language: props.language ?? "plaintext",
      theme: "vs-dark",
      readOnly: props.readOnly,
      automaticLayout: true,
      minimap: { enabled: false },
      fontSize: 12.5,
      scrollBeyondLastLine: false,
      tabSize: 2,
      wordWrap: "on",
      renderLineHighlight: "none",
      lineNumbersMinChars: 3,
    });
    e.onDidChangeModelContent(() => cb.current.onChange?.(e.getValue()));
    e.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.Enter, () => cb.current.onSubmit?.());
    ed.current = e;
    return () => {
      e.getModel()?.dispose();
      e.dispose();
    };
  }, []);

  useEffect(() => {
    const e = ed.current;
    if (e && e.getValue() !== props.value) e.setValue(props.value);
  }, [props.value]);

  useEffect(() => {
    const m = ed.current?.getModel();
    if (m) monaco.editor.setModelLanguage(m, props.language ?? "plaintext");
  }, [props.language]);

  useEffect(() => {
    ed.current?.updateOptions({ readOnly: props.readOnly });
  }, [props.readOnly]);

  return <div className="code-editor" style={{ height: props.height ?? 300 }} ref={host} />;
}
