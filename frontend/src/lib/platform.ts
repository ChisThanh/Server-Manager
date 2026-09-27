export const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);

/** Human-readable shortcut, e.g. "⌘B" on macOS and "Ctrl+Shift+B" elsewhere. */
export const sidebarShortcut = isMac ? "⌘B" : "Ctrl+Shift+B";

/**
 * Sidebar toggle. Plain Ctrl+B is left alone on Windows/Linux because shells
 * in the terminal use it (readline backward-char, tmux prefix).
 */
export function isSidebarShortcut(e: KeyboardEvent): boolean {
  if (e.key.toLowerCase() !== "b" || e.altKey) return false;
  return isMac ? e.metaKey && !e.ctrlKey && !e.shiftKey : e.ctrlKey && e.shiftKey && !e.metaKey;
}
