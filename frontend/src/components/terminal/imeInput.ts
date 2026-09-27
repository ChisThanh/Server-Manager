import type { Terminal } from "@xterm/xterm";

/**
 * Text input for xterm.js that works with IMEs which edit what was already
 * typed — Vietnamese Telex/VNI (macOS built-in, OpenKey, EVKey…), dead keys,
 * CJK composition.
 *
 * xterm cancels printable keystrokes, so its hidden textarea stays empty and
 * an IME has nothing to replace; and when an IME does replace text xterm
 * sends the new characters without deleting the old ones ("Viet" → "Vieêt").
 *
 * Instead, letters/digits are let through into the textarea, which mirrors
 * the word being typed. Whenever it changes (and no composition is in
 * progress) the difference from what was last sent is transmitted as
 * DEL × removed characters + added text. That single rule covers typing,
 * character replacement, backspace-and-retype, selection-and-retype and
 * re-opened compositions alike. Everything else (Enter, arrows, Ctrl/Alt
 * combos, paste…) is still handled by xterm, after the mirror is flushed and
 * reset so the byte order sent to the shell is preserved.
 */
export function attachImeInput(term: Terminal, send: (data: string) => void, macOptionIsMeta: boolean): () => void {
  const ta = term.textarea!;
  const root = term.element!;
  let composing = false;
  let mirror = ""; // what the shell has received from the textarea since the last reset
  // Set while xterm is handling a key itself. WebKit still inserts that key
  // into the textarea after xterm cancels it; such input must be dropped,
  // not sent a second time.
  let xtermOwnsKey = false;

  const preview = document.createElement("div");
  preview.className = "ime-preview";
  root.querySelector(".xterm-screen")?.appendChild(preview);

  const codepoints = (s: string) => Array.from(s);

  const sync = () => {
    if (composing) return;
    const value = ta.value;
    if (value === mirror) return;
    const a = codepoints(mirror);
    const b = codepoints(value);
    let i = 0;
    while (i < a.length && i < b.length && a[i] === b[i]) i++;
    const data = "\x7f".repeat(a.length - i) + b.slice(i).join("");
    mirror = value;
    if (data) send(data);
    // Keep the textarea small; IMEs only need the current word as context.
    if (b.length > 256) {
      mirror = ta.value = b.slice(-32).join("");
      ta.setSelectionRange(ta.value.length, ta.value.length);
    }
  };

  const reset = () => {
    if (composing) return;
    sync();
    ta.value = "";
    mirror = "";
  };

  // Only letters and digits go through the textarea: IMEs need them as
  // context, and routing punctuation through a text field would expose it
  // to smart-quote/dash substitution.
  const isWordChar = (key: string) => codepoints(key).length === 1 && /[\p{L}\p{N}\p{M}]/u.test(key);

  const MODIFIERS = new Set(["Shift", "Control", "Alt", "Meta", "CapsLock", "Fn", "OS", "Hyper", "Super"]);

  term.attachCustomKeyEventHandler((e) => {
    if (e.type === "keyup") {
      xtermOwnsKey = false;
      return true;
    }
    // A lone modifier press must not end the current word (Shift+← follows).
    if (MODIFIERS.has(e.key)) return true;
    // Keys consumed by an IME (keyCode 229) belong to the composition.
    if (composing || e.isComposing || e.keyCode === 229 || e.key === "Process") return false;
    const alt = e.altKey && macOptionIsMeta;
    const plain = !e.ctrlKey && !e.metaKey && !alt;
    if (plain && (isWordChar(e.key) || e.key === "Dead" || e.key === "Unidentified")) {
      if (e.type === "keydown" && ta.selectionStart === ta.selectionEnd) {
        ta.setSelectionRange(ta.value.length, ta.value.length);
      }
      return false; // let the browser insert it; the input event sends it
    }
    if (e.type === "keydown" && plain && mirror) {
      // Deleting inside the current word (incl. IMEs that send backspace
      // before the replacement) is done in the textarea so it stays in sync.
      if (e.key === "Backspace" && !e.shiftKey) return false;
      // Some IMEs (OpenKey "browser" mode) select with Shift+← and retype.
      if (e.key === "ArrowLeft" && e.shiftKey) return false;
    }
    if (e.type === "keydown") reset();
    xtermOwnsKey = true;
    return true;
  });

  // Take text input away from xterm: these capture listeners on an ancestor
  // run before (and stop) xterm's own listeners on the textarea.
  const onInput = (e: Event) => {
    if (e.target !== ta) return;
    e.stopImmediatePropagation();
    if (xtermOwnsKey && !composing) {
      // xterm already sent this key; undo WebKit's copy in the textarea.
      xtermOwnsKey = false;
      ta.value = mirror;
      ta.setSelectionRange(ta.value.length, ta.value.length);
      return;
    }
    if (!(e as InputEvent).isComposing) sync();
  };
  const placePreview = () => {
    const screen = root.querySelector(".xterm-screen") as HTMLElement | null;
    if (!screen) return;
    const cellW = screen.clientWidth / term.cols;
    const cellH = screen.clientHeight / term.rows;
    const buf = term.buffer.active;
    preview.style.left = `${buf.cursorX * cellW}px`;
    preview.style.top = `${buf.cursorY * cellH}px`;
    preview.style.height = `${cellH}px`;
    preview.style.lineHeight = `${cellH}px`;
    preview.style.fontFamily = term.options.fontFamily ?? "monospace";
    preview.style.fontSize = `${term.options.fontSize ?? 13}px`;
  };
  const onCompStart = (e: Event) => {
    if (e.target !== ta) return;
    e.stopImmediatePropagation();
    composing = true;
    placePreview();
  };
  const onCompUpdate = (e: Event) => {
    if (e.target !== ta) return;
    e.stopImmediatePropagation();
    preview.textContent = (e as CompositionEvent).data ?? "";
    preview.classList.toggle("active", !!preview.textContent);
  };
  const onCompEnd = (e: Event) => {
    if (e.target !== ta) return;
    e.stopImmediatePropagation();
    composing = false;
    preview.textContent = "";
    preview.classList.remove("active");
    // The committed text lands in the textarea after this event on some engines.
    sync();
    setTimeout(sync, 0);
  };
  // Clicking moves the shell's cursor context; start a fresh word.
  const onMouseDown = () => reset();

  root.addEventListener("input", onInput, true);
  root.addEventListener("compositionstart", onCompStart, true);
  root.addEventListener("compositionupdate", onCompUpdate, true);
  root.addEventListener("compositionend", onCompEnd, true);
  root.addEventListener("mousedown", onMouseDown, true);
  ta.addEventListener("blur", sync);

  return () => {
    root.removeEventListener("input", onInput, true);
    root.removeEventListener("compositionstart", onCompStart, true);
    root.removeEventListener("compositionupdate", onCompUpdate, true);
    root.removeEventListener("compositionend", onCompEnd, true);
    root.removeEventListener("mousedown", onMouseDown, true);
    ta.removeEventListener("blur", sync);
    preview.remove();
  };
}
